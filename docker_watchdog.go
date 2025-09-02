package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

type ContainerState struct {
	WasRunning    bool
	StoppedAt     time.Time
	RestartTimer  *time.Timer
}

type DependencyManager struct {
	client            *client.Client
	dependencies      map[string][]string           // parent -> []children
	childStates       map[string]*ContainerState    // child -> state info
	parentStates      map[string]*ContainerState    // parent -> state info for restart detection
	childNames        map[string]string             // child ID -> container name for recreation
	mutex             sync.RWMutex
	ctx               context.Context
	cancel            context.CancelFunc
	restartTimeout    time.Duration
	templatesPath     string
}

func NewDependencyManager() (*DependencyManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	
	dm := &DependencyManager{
		client:         cli,
		dependencies:   make(map[string][]string),
		childStates:    make(map[string]*ContainerState),
		parentStates:   make(map[string]*ContainerState),
		childNames:     make(map[string]string),
		ctx:            ctx,
		cancel:         cancel,
		restartTimeout: 5 * time.Second,
		templatesPath:  "/boot/config/system/docker/templates/",
	}

	return dm, nil
}

func (dm *DependencyManager) scanContainers() error {
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	// Don't clear existing dependencies, just update them
	newDependencies := make(map[string][]string)
	
	for _, c := range containers {
		inspect, err := dm.client.ContainerInspect(dm.ctx, c.ID)
		if err != nil {
			log.Printf("Warning: failed to inspect container %s: %v", c.ID, err)
			continue
		}

		// Check if container uses "container:..." network mode
		if strings.HasPrefix(inspect.HostConfig.NetworkMode.NetworkName(), "container:") {
			parentName := strings.TrimPrefix(inspect.HostConfig.NetworkMode.NetworkName(), "container:")
			
			// Find parent container ID by name
			parentID, err := dm.findContainerByName(parentName)
			if err != nil {
				log.Printf("Warning: parent container %s not found for %s: %v", parentName, c.Names[0], err)
				continue
			}

			// Add to new dependencies
			if newDependencies[parentID] == nil {
				newDependencies[parentID] = make([]string, 0)
			}
			newDependencies[parentID] = append(newDependencies[parentID], c.ID)
			
			// Initialize child state if not exists
			if _, exists := dm.childStates[c.ID]; !exists {
				dm.childStates[c.ID] = &ContainerState{
					WasRunning: c.State == "running",
				}
				// Store container name for potential recreation
				dm.childNames[c.ID] = dm.getContainerName(c.ID)
				log.Printf("New child container detected: %s -> %s (currently %s)", 
					dm.getContainerName(parentID), dm.getContainerName(c.ID), c.State)
			}
			
			// Initialize parent state if not exists
			if _, exists := dm.parentStates[parentID]; !exists {
				dm.parentStates[parentID] = &ContainerState{
					WasRunning: false,
				}
			}
		}
	}

	// Update dependencies
	dm.dependencies = newDependencies
	
	// Check for missing parents and stop their children
	dm.handleMissingParents()

	return nil
}

func (dm *DependencyManager) handleMissingParents() {
	currentContainers := make(map[string]bool)
	
	// Get all currently existing containers
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{All: true})
	if err != nil {
		log.Printf("Error listing containers for missing parent check: %v", err)
		return
	}
	
	for _, c := range containers {
		currentContainers[c.ID] = true
	}
	
	// Check each parent in our dependencies
	for parentID, children := range dm.dependencies {
		if !currentContainers[parentID] {
			log.Printf("Parent container %s no longer exists, stopping its children", 
				dm.getContainerName(parentID))
			dm.stopChildrenUnsafe(parentID, children, "parent no longer exists")
		} else {
			// Parent exists, check if it's running
			inspect, err := dm.client.ContainerInspect(dm.ctx, parentID)
			if err != nil {
				log.Printf("Error inspecting parent container %s: %v", parentID, err)
				continue
			}
			
			if !inspect.State.Running {
				log.Printf("Parent container %s is stopped, ensuring children are stopped", 
					dm.getContainerName(parentID))
				dm.stopChildrenUnsafe(parentID, children, "parent is stopped")
			}
		}
	}
}

func (dm *DependencyManager) findContainerByName(name string) (string, error) {
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, c := range containers {
		for _, containerName := range c.Names {
			cleanName := strings.TrimPrefix(containerName, "/")
			if cleanName == name || c.ID == name {
				return c.ID, nil
			}
		}
	}

	return "", fmt.Errorf("container %s not found", name)
}

func (dm *DependencyManager) getContainerName(id string) string {
	inspect, err := dm.client.ContainerInspect(dm.ctx, id)
	if err != nil {
		return id[:12]
	}
	return strings.TrimPrefix(inspect.Name, "/")
}

func (dm *DependencyManager) handleContainerEvent(event events.Message) {
	switch event.Action {
	case "start":
		dm.handleContainerStart(event.ID)
	case "stop", "die":
		dm.handleContainerStop(event.ID)
	case "create":
		// Rescan when new containers are created
		go func() {
			time.Sleep(1 * time.Second) // Small delay to ensure container is fully created
			if err := dm.scanContainers(); err != nil {
				log.Printf("Error during rescan after create event: %v", err)
			}
		}()
	}
}

func (dm *DependencyManager) handleContainerStart(containerID string) {
	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	// Check if this is a parent container
	if children, isParent := dm.dependencies[containerID]; isParent {
		parentState := dm.parentStates[containerID]
		
		// Cancel any existing restart timer
		if parentState.RestartTimer != nil {
			parentState.RestartTimer.Stop()
			parentState.RestartTimer = nil
		}

		// Check if this is a restart (parent was stopped recently) or a recreation
		timeSinceStop := time.Since(parentState.StoppedAt)
		if timeSinceStop > 0 && timeSinceStop <= dm.restartTimeout {
			log.Printf("Parent container %s restarted within timeout (%.2fs), starting children...", 
				dm.getContainerName(containerID), timeSinceStop.Seconds())
			dm.startChildrenForRestart(containerID, children)
		} else if timeSinceStop > dm.restartTimeout {
			log.Printf("Parent container %s started after timeout (%.2fs), children will remain stopped", 
				dm.getContainerName(containerID), timeSinceStop.Seconds())
		} else {
			log.Printf("Parent container %s started (newly created), recreating and starting children...", 
				dm.getContainerName(containerID))
			dm.startChildrenForRecreation(containerID, children)
		}
	}
}

func (dm *DependencyManager) handleContainerStop(containerID string) {
	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	// Check if this is a parent container
	if children, isParent := dm.dependencies[containerID]; isParent {
		parentState := dm.parentStates[containerID]
		parentState.StoppedAt = time.Now()
		
		log.Printf("Parent container %s stopped, stopping children and starting timeout...", 
			dm.getContainerName(containerID))
		
		// Stop all child containers
		dm.stopChildrenUnsafe(containerID, children, "parent stopped")

		// Set up restart timeout timer
		if parentState.RestartTimer != nil {
			parentState.RestartTimer.Stop()
		}
		
		parentState.RestartTimer = time.AfterFunc(dm.restartTimeout, func() {
			dm.mutex.Lock()
			defer dm.mutex.Unlock()
			
			log.Printf("Restart timeout exceeded for parent %s, children will not be restarted", 
				dm.getContainerName(containerID))
			
			// Mark all children as should not be restarted
			for _, childID := range children {
				if childState, exists := dm.childStates[childID]; exists {
					childState.WasRunning = false
				}
			}
		})
	}
}

func (dm *DependencyManager) stopChildrenUnsafe(parentID string, children []string, reason string) {
	for _, childID := range children {
		inspect, err := dm.client.ContainerInspect(dm.ctx, childID)
		if err != nil {
			log.Printf("Error inspecting child container %s: %v", childID, err)
			continue
		}

		// Store current state for potential future restart
		if childState, exists := dm.childStates[childID]; exists {
			childState.WasRunning = inspect.State.Running
		}

		if inspect.State.Running {
			log.Printf("Stopping child container %s (%s)", dm.getContainerName(childID), reason)
			timeout := int(10) // 10 seconds timeout for stopping
			if err := dm.client.ContainerStop(dm.ctx, childID, container.StopOptions{Timeout: &timeout}); err != nil {
				log.Printf("Error stopping child container %s: %v", dm.getContainerName(childID), err)
			}
		}
	}
}

func (dm *DependencyManager) recreateContainer(containerName string) error {
	log.Printf("Attempting to recreate container: %s", containerName)
	
	templateFile := fmt.Sprintf("%s.json", containerName)
	
	// Check if template exists
	templatePath := fmt.Sprintf("%s%s", dm.templatesPath, templateFile)
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return fmt.Errorf("template file not found: %s", templatePath)
	}
	
	// Change to templates directory and execute mos-deploy_docker
	cmd := exec.CommandContext(dm.ctx, "mos-deploy_docker", templateFile, "recreate_container")
	cmd.Dir = dm.templatesPath
	output, err := cmd.CombinedOutput()
	
	if err != nil {
		return fmt.Errorf("failed to recreate container %s: %v, output: %s", 
			containerName, err, string(output))
	}
	
	log.Printf("Successfully recreated container: %s", containerName)
	return nil
}

func (dm *DependencyManager) startChildrenForRestart(parentID string, children []string) {
	// For restart: just start existing containers
	for _, childID := range children {
		childState, exists := dm.childStates[childID]
		if !exists || !childState.WasRunning {
			log.Printf("Child container %s was not running before, keeping it stopped", 
				dm.getContainerName(childID))
			continue
		}

		// Check if child is already running
		inspect, err := dm.client.ContainerInspect(dm.ctx, childID)
		if err != nil {
			log.Printf("Error inspecting child container %s: %v", childID, err)
			continue
		}

		if inspect.State.Running {
			log.Printf("Child container %s is already running", dm.getContainerName(childID))
			continue
		}

		log.Printf("Starting child container %s", dm.getContainerName(childID))
		if err := dm.client.ContainerStart(dm.ctx, childID, container.StartOptions{}); err != nil {
			log.Printf("Error starting child container %s: %v", dm.getContainerName(childID), err)
		}
	}
}

func (dm *DependencyManager) startChildrenForRecreation(parentID string, children []string) {
	// For recreation: check if containers exist, recreate if needed, then start
	currentContainers := make(map[string]bool)
	
	// Get current container list to check which ones still exist
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{All: true})
	if err != nil {
		log.Printf("Error listing containers: %v", err)
		return
	}
	
	for _, c := range containers {
		currentContainers[c.ID] = true
	}
	
	for _, childID := range children {
		childState, exists := dm.childStates[childID]
		if !exists || !childState.WasRunning {
			log.Printf("Child container %s was not running before, keeping it stopped", 
				dm.getContainerName(childID))
			continue
		}
		
		// Check if container still exists
		if !currentContainers[childID] {
			// Container doesn't exist, try to recreate it
			if containerName, nameExists := dm.childNames[childID]; nameExists {
				log.Printf("Child container %s no longer exists, attempting to recreate...", containerName)
				
				if err := dm.recreateContainer(containerName); err != nil {
					log.Printf("Failed to recreate container %s: %v", containerName, err)
					continue
				}
				
				// Wait a moment for container to be created
				time.Sleep(2 * time.Second)
				
				// Find the new container ID
				newChildID, err := dm.findContainerByName(containerName)
				if err != nil {
					log.Printf("Failed to find recreated container %s: %v", containerName, err)
					continue
				}
				
				// Update the dependency mapping
				dm.updateChildID(childID, newChildID)
				childID = newChildID
			} else {
				log.Printf("Child container %s no longer exists and name unknown, skipping", childID)
				continue
			}
		}

		// Check if child is already running
		inspect, err := dm.client.ContainerInspect(dm.ctx, childID)
		if err != nil {
			log.Printf("Error inspecting child container %s: %v", childID, err)
			continue
		}

		if inspect.State.Running {
			log.Printf("Child container %s is already running", dm.getContainerName(childID))
			continue
		}

		log.Printf("Starting child container %s", dm.getContainerName(childID))
		if err := dm.client.ContainerStart(dm.ctx, childID, container.StartOptions{}); err != nil {
			log.Printf("Error starting child container %s: %v", dm.getContainerName(childID), err)
		}
	}
}

func (dm *DependencyManager) updateChildID(oldID, newID string) {
	// Update dependencies
	for parentID, children := range dm.dependencies {
		for i, childID := range children {
			if childID == oldID {
				dm.dependencies[parentID][i] = newID
			}
		}
	}
	
	// Transfer state
	if state, exists := dm.childStates[oldID]; exists {
		dm.childStates[newID] = state
		delete(dm.childStates, oldID)
	}
	
	// Transfer name
	if name, exists := dm.childNames[oldID]; exists {
		dm.childNames[newID] = name
		delete(dm.childNames, oldID)
	}
}

func (dm *DependencyManager) watchEvents() {
	for {
		eventChan, errChan := dm.client.Events(dm.ctx, events.ListOptions{})
		
		for {
			select {
			case event := <-eventChan:
				if event.Type == events.ContainerEventType {
					dm.handleContainerEvent(event)
				}
			case err := <-errChan:
				if err != nil {
					log.Printf("Error watching events: %v", err)
					// Break inner loop to reconnect
					goto reconnect
				}
			case <-dm.ctx.Done():
				return
			}
		}
		
		reconnect:
		log.Println("Reconnecting to Docker events...")
		time.Sleep(5 * time.Second)
	}
}

func (dm *DependencyManager) Start() error {
	log.Printf("Starting Docker Dependency Manager (restart timeout: %v)...", dm.restartTimeout)
	
	// Initial scan to find existing containers
	if err := dm.scanContainers(); err != nil {
		return fmt.Errorf("initial container scan failed: %w", err)
	}

	// Watch for events - this handles all dynamic changes
	go dm.watchEvents()

	log.Println("Docker Dependency Manager started successfully")
	return nil
}

func (dm *DependencyManager) Stop() {
	log.Println("Stopping Docker Dependency Manager...")
	
	// Cancel all restart timers
	dm.mutex.Lock()
	for _, parentState := range dm.parentStates {
		if parentState.RestartTimer != nil {
			parentState.RestartTimer.Stop()
		}
	}
	dm.mutex.Unlock()
	
	dm.cancel()
	if dm.client != nil {
		dm.client.Close()
	}
}

func (dm *DependencyManager) PrintDependencies() {
	dm.mutex.RLock()
	defer dm.mutex.RUnlock()

	if len(dm.dependencies) == 0 {
		log.Println("No container dependencies found")
		return
	}

	log.Printf("Container dependencies (restart timeout: %v):", dm.restartTimeout)
	for parent, children := range dm.dependencies {
		parentName := dm.getContainerName(parent)
		log.Printf("  %s:", parentName)
		for _, child := range children {
			childName := dm.getContainerName(child)
			state := "stopped"
			if childState, exists := dm.childStates[child]; exists && childState.WasRunning {
				state = "was running"
			}
			log.Printf("    -> %s (%s)", childName, state)
		}
	}
}

func main() {
	dm, err := NewDependencyManager()
	if err != nil {
		log.Fatalf("Failed to create dependency manager: %v", err)
	}

	if err := dm.Start(); err != nil {
		log.Fatalf("Failed to start dependency manager: %v", err)
	}

	// Print initial dependencies
	time.Sleep(2 * time.Second)
	dm.PrintDependencies()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	dm.Stop()
	log.Println("Dependency Manager stopped")
}


// go mod tidy && go build -ldflags="-s -w"
