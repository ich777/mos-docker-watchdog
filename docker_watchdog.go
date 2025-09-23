package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

type ContainerState struct {
	WasRunning    bool
	StoppedAt     time.Time
	RestartTimer  *time.Timer
}

type ContainerConfig struct {
	Name      string `json:"name"`
	Autostart bool   `json:"autostart"`
	// Ignore all other fields with json:"-" or just don't define them
}

type DeletedParentInfo struct {
	Name         string
	Children     []string
	DeletedAt    time.Time
}

type Notification struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority string `json:"priority"`
}

type DependencyManager struct {
	client            *client.Client
	dependencies      map[string][]string           // parent -> []children
	childStates       map[string]*ContainerState    // child -> state info
	parentStates      map[string]*ContainerState    // parent -> state info for restart detection
	childNames        map[string]string             // child ID -> container name for recreation
	parentNames       map[string]string             // parent ID -> container name for recreation
	mutex             sync.RWMutex
	ctx               context.Context
	cancel            context.CancelFunc
	restartTimeout    time.Duration
	recreationTimeout time.Duration
	templatesPath     string
	containerConfigPath string
	autostartCache    map[string]bool  // Cache for autostart settings
	cacheLastUpdate  time.Time        // When cache was last updated
	deletedParents    map[string]DeletedParentInfo // Store info about deleted parents for recreation
	recreatedParents  map[string]bool              // Track successfully recreated parents to suppress timer warnings
	stoppedContainers map[string]bool              // Track recently stopped containers to avoid duplicate stops
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
		parentNames:    make(map[string]string),
		ctx:            ctx,
		cancel:         cancel,
		restartTimeout: 5 * time.Second,
		recreationTimeout: 10 * time.Second,
		templatesPath:  "/boot/config/system/docker/templates/",
		containerConfigPath: "/var/lib/docker/mos/containers",
		autostartCache: make(map[string]bool),
		deletedParents: make(map[string]DeletedParentInfo),
		recreatedParents: make(map[string]bool),
		stoppedContainers: make(map[string]bool),
	}

	return dm, nil
}

func (dm *DependencyManager) getMOSContainerFilter() filters.Args {
	filter := filters.NewArgs()
	filter.Add("label", "mos.backend=docker")
	return filter
}

func (dm *DependencyManager) sendNotification(title, message, priority string) {
	notification := Notification{
		Title:    title,
		Message:  message,
		Priority: priority,
	}
	
	jsonData, err := json.Marshal(notification)
	if err != nil {
		log.Printf("Error marshaling notification: %v", err)
		return
	}
	
	conn, err := net.Dial("unix", "/var/run/mos-notify.sock")
	if err != nil {
		log.Printf("Error connecting to notification socket: %v", err)
		return
	}
	defer conn.Close()
	
	_, err = conn.Write(jsonData)
	if err != nil {
		log.Printf("Error sending notification: %v", err)
		return
	}
	
	log.Printf("Sent notification: %s - %s", title, message)
}

func (dm *DependencyManager) formatContainerMessage(action string, containerNames []string, parentName string) string {
	containerWord := "container"
	if len(containerNames) > 1 {
		containerWord = "containers"
	}
	
	return fmt.Sprintf("%s %s %s for parent %s", action, containerWord, strings.Join(containerNames, ", "), parentName)
}

func (dm *DependencyManager) scanContainers() error {
	// Only get containers with the MOS label
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
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

		fullNetworkMode := string(inspect.HostConfig.NetworkMode)

		// Check if container uses "container:..." network mode - use the full network mode string
		if strings.HasPrefix(fullNetworkMode, "container:") {
			parentFullID := strings.TrimPrefix(fullNetworkMode, "container:")
			
			// Use the short SHA (first 12 chars) to find the parent in our container list
			parentShortID := parentFullID
			if len(parentFullID) > 12 {
				parentShortID = parentFullID[:12]
			}
			
			// Find the parent container in our current container list
			var actualParentID string
			for _, containerCandidate := range containers {
				if strings.HasPrefix(containerCandidate.ID, parentShortID) {
					actualParentID = containerCandidate.ID
					break
				}
			}
			
			if actualParentID == "" {
				log.Printf("Warning: parent container with ID %s not found for %s", parentShortID, c.Names[0])
				continue
			}

			// Add to new dependencies
			if newDependencies[actualParentID] == nil {
				newDependencies[actualParentID] = make([]string, 0)
			}
			newDependencies[actualParentID] = append(newDependencies[actualParentID], c.ID)
			
			// Store parent name for recreation
			dm.parentNames[actualParentID] = dm.getContainerName(actualParentID)
			
			// Initialize child state if not exists
			if _, exists := dm.childStates[c.ID]; !exists {
				isRunning := c.State == "running"
				dm.childStates[c.ID] = &ContainerState{
					WasRunning: isRunning,
				}
				// Store container name for potential recreation
				dm.childNames[c.ID] = dm.getContainerName(c.ID)
				log.Printf("New child container detected: %s -> %s", 
					dm.getContainerName(actualParentID), dm.getContainerName(c.ID))
			} else {
				// Update current running state if container exists (only if it's actually running)
				isRunning := c.State == "running"
				if isRunning {
					dm.childStates[c.ID].WasRunning = true
				}
			}
			
			// Initialize parent state if not exists
			if _, exists := dm.parentStates[actualParentID]; !exists {
				dm.parentStates[actualParentID] = &ContainerState{
					WasRunning: false,
				}
			}
		}
	}

	// Check for recreated parents BEFORE updating dependencies (so we still have the old ones)
	dm.handleRecreatedParents()
	
	// Update dependencies
	dm.dependencies = newDependencies
	
	// Check for missing parents and stop their children
	dm.handleMissingParents()

	return nil
}

func (dm *DependencyManager) handleMissingParents() {
	currentContainers := make(map[string]bool)
	
	// Get all currently existing MOS containers
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
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
			parentName := dm.getContainerName(parentID)
			log.Printf("Parent container %s no longer exists, stopping its children", parentName)
			dm.stopChildrenUnsafe(parentID, children, "parent no longer exists")
			
			// Store deleted parent info for potential recreation
			dm.deletedParents[parentName] = DeletedParentInfo{
				Name:      parentName,
				Children:  append([]string{}, children...), // Make a copy
				DeletedAt: time.Now(),
			}
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

func (dm *DependencyManager) handleRecreatedParents() {
	// Get all currently existing MOS containers
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
	if err != nil {
		log.Printf("Error listing containers for recreated parent check: %v", err)
		return
	}
	
	// Build map of current container names to IDs
	currentContainersByName := make(map[string]string)
	for _, c := range containers {
		for _, name := range c.Names {
			cleanName := strings.TrimPrefix(name, "/")
			currentContainersByName[cleanName] = c.ID
		}
	}
	

	
	// Check for parents that no longer exist but might have been recreated with same name
	var parentsToRecreate []string
	var childrenToRecreate [][]string
	var parentNames []string
	
	for parentID, children := range dm.dependencies {
		// Check if this parent ID still exists
		parentExists := false
		for _, c := range containers {
			if c.ID == parentID {
				parentExists = true
				break
			}
		}
		
		if !parentExists {
			// Parent doesn't exist, check if a container with the same name exists
			oldParentName := dm.parentNames[parentID] // Use stored name instead of trying to get it from deleted container
			if oldParentName == "" {
				oldParentName = dm.getContainerName(parentID) // Fallback to old method
			}
			
			if newParentID, found := currentContainersByName[oldParentName]; found {
				log.Printf("Found recreated parent container: %s", oldParentName)
				
				parentsToRecreate = append(parentsToRecreate, newParentID)
				childrenToRecreate = append(childrenToRecreate, children)
				parentNames = append(parentNames, oldParentName)
				
				// Stop any running timer for this parent
				if parentState, exists := dm.parentStates[parentID]; exists && parentState.RestartTimer != nil {
					parentState.RestartTimer.Stop()
				}
				
				// Remove old dependency
				delete(dm.dependencies, parentID)
				delete(dm.parentStates, parentID)
				delete(dm.parentNames, parentID)
			}
		}
	}
	
	// Also check deleted parents for recreation
	for parentName, deletedInfo := range dm.deletedParents {
		// Clean up old entries (older than recreation timeout + buffer)
		if time.Since(deletedInfo.DeletedAt) > dm.recreationTimeout + 30*time.Second {
			delete(dm.deletedParents, parentName)
			continue
		}
		
		// Check if a container with this name now exists
		if newParentID, found := currentContainersByName[parentName]; found {
			log.Printf("Found recreated parent container: %s", parentName)
			
			parentsToRecreate = append(parentsToRecreate, newParentID)
			childrenToRecreate = append(childrenToRecreate, deletedInfo.Children)
			parentNames = append(parentNames, parentName)
			
			// Remove from deleted cache
			delete(dm.deletedParents, parentName)
		}
	}
	

	
	// Recreate children for recreated parents
	for i, newParentID := range parentsToRecreate {
		children := childrenToRecreate[i]
		parentName := parentNames[i]
		
		log.Printf("Recreating children for parent %s, waiting 2s before recreation...", parentName)
		
		// Send notification about starting recreation
		var childNames []string
		for _, oldChildID := range children {
			if name, exists := dm.childNames[oldChildID]; exists {
				childNames = append(childNames, name)
			}
		}
		
		if len(childNames) > 0 {
			message := dm.formatContainerMessage("Starting recreation of", childNames, parentName)
			dm.sendNotification("Docker", message, "normal")
		}
		
		// Wait 2 seconds before starting recreation
		time.Sleep(2 * time.Second)
		
		// Initialize new parent state
		dm.parentStates[newParentID] = &ContainerState{
			WasRunning: false,
		}
		
		// Recreate all children (regardless of autostart setting)
		var newChildren []string
		for _, oldChildID := range children {
			childName := dm.childNames[oldChildID]
			
			log.Printf("Recreating child container %s for parent %s", childName, parentName)
			
			if err := dm.recreateContainer(childName); err != nil {
				log.Printf("Failed to recreate child container %s: %v", childName, err)
				continue
			}
			
			// Wait for container to be created
			time.Sleep(2 * time.Second)
			
			// Find new child container ID
			newChildID, err := dm.findContainerByName(childName)
			if err != nil {
				log.Printf("Failed to find recreated child container %s: %v", childName, err)
				continue
			}
			
			// Add to new dependencies
			newChildren = append(newChildren, newChildID)
			
			// Transfer state to new child ID - preserve the original WasRunning state
			var wasRunning bool
			if childState, exists := dm.childStates[oldChildID]; exists {
				wasRunning = childState.WasRunning
			}
			
			dm.childStates[newChildID] = &ContainerState{
				WasRunning: wasRunning,
			}
			dm.childNames[newChildID] = childName
			
			// Clean up old state
			delete(dm.childStates, oldChildID)
			delete(dm.childNames, oldChildID)
			
			// Start the recreated child container ONLY if it was running before (ignore autostart for recreation)
			if wasRunning {
				log.Printf("Starting recreated child container %s (was running before recreation)", childName)
				go func(id, name string) {
					if err := dm.client.ContainerStart(dm.ctx, id, container.StartOptions{}); err != nil {
						log.Printf("Error starting recreated child container %s: %v", name, err)
					} else {
						log.Printf("Successfully started recreated child container %s", name)
					}
				}(newChildID, childName)
			} else {
				log.Printf("Child container %s was not running before recreation, leaving stopped", childName)
			}
		}
		
		// Update dependencies with new parent and children
		if len(newChildren) > 0 {
			dm.dependencies[newParentID] = newChildren
		}
		
		// Send notification about completed recreation
		var recreatedNames []string
		for _, childID := range newChildren {
			if name, exists := dm.childNames[childID]; exists {
				recreatedNames = append(recreatedNames, name)
			}
		}
		
		if len(recreatedNames) > 0 {
			message := dm.formatContainerMessage("Successfully recreated", recreatedNames, parentName)
			dm.sendNotification("Docker", message, "normal")
		}
		
		// Mark parent as successfully recreated (to suppress timer warnings)
		// We need to mark both the old ID and the parent name
		for oldParentID := range dm.parentStates {
			if dm.parentNames[oldParentID] == parentName {
				dm.recreatedParents[oldParentID] = true
				break
			}
		}
	}
}

func (dm *DependencyManager) findContainerByName(name string) (string, error) {
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
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

func (dm *DependencyManager) findContainerByPartialName(name string) (string, error) {
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
	if err != nil {
		return "", err
	}

	for _, c := range containers {
		for _, containerName := range c.Names {
			cleanName := strings.TrimPrefix(containerName, "/")
			// Try case-insensitive partial match
			if strings.Contains(strings.ToLower(cleanName), strings.ToLower(name)) ||
			   strings.Contains(strings.ToLower(name), strings.ToLower(cleanName)) {
				return c.ID, nil
			}
		}
	}

	return "", fmt.Errorf("container %s not found (partial match)", name)
}

func (dm *DependencyManager) containerExists(containerID string) bool {
	_, err := dm.client.ContainerInspect(dm.ctx, containerID)
	return err == nil
}

func (dm *DependencyManager) loadAutostartCache() {
	// Update cache every 30 seconds max, or on first load
	if time.Since(dm.cacheLastUpdate) < 30*time.Second && len(dm.autostartCache) > 0 {
		return
	}
	
	// Clear existing cache
	dm.autostartCache = make(map[string]bool)
	
	// Read the single containers file
	data, err := ioutil.ReadFile(dm.containerConfigPath)
	if err != nil {
		log.Printf("Error reading container config file %s: %v", dm.containerConfigPath, err)
		return
	}
	
	// Parse as array of container configs
	var containers []ContainerConfig
	if err := json.Unmarshal(data, &containers); err != nil {
		log.Printf("Error parsing container config file: %v", err)
		return
	}
	
	// Build cache from array
	for _, container := range containers {
		dm.autostartCache[container.Name] = container.Autostart
	}
	
	dm.cacheLastUpdate = time.Now()
	log.Printf("Loaded autostart settings for %d containers", len(dm.autostartCache))
}

func (dm *DependencyManager) getContainerAutostart(containerName string) bool {
	// Remove leading slash if present
	cleanName := strings.TrimPrefix(containerName, "/")
	
	// Load/refresh cache if needed
	dm.loadAutostartCache()
	
	// Get from cache
	autostart, exists := dm.autostartCache[cleanName]
	if !exists {
		log.Printf("No autostart config found for %s, defaulting to false", cleanName)
		return false
	}
	
	log.Printf("Container %s autostart setting: %v", cleanName, autostart)
	return autostart
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
	case "destroy":
		// Handle container deletion - might trigger recreation
		go func() {
			time.Sleep(2 * time.Second) // Wait a bit longer for potential recreation
			if err := dm.scanContainers(); err != nil {
				log.Printf("Error during rescan after destroy event: %v", err)
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
			// Quick restart - start children based on autostart setting
			log.Printf("Parent container %s restarted within restart timeout (%.2fs), waiting 2s before starting children...", 
				dm.getContainerName(containerID), timeSinceStop.Seconds())
			go func() {
				time.Sleep(2 * time.Second)
				dm.startChildrenForRestart(containerID, children)
			}()
		} else if timeSinceStop > dm.restartTimeout && timeSinceStop <= dm.recreationTimeout {
			// Slower restart/recreation - still start children based on autostart setting
			log.Printf("Parent container %s restarted within recreation timeout (%.2fs), waiting 2s before starting children...", 
				dm.getContainerName(containerID), timeSinceStop.Seconds())
			go func() {
				time.Sleep(2 * time.Second)
				dm.startChildrenForRestart(containerID, children)
			}()
		} else if timeSinceStop > dm.recreationTimeout {
			// Very long time - likely manual restart, still honor autostart
			log.Printf("Parent container %s started after recreation timeout (%.2fs), waiting 2s before starting children...", 
				dm.getContainerName(containerID), timeSinceStop.Seconds())
			go func() {
				time.Sleep(2 * time.Second)
				dm.startChildrenForRestart(containerID, children)
			}()
		} else {
			// Brand new container
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
		
		log.Printf("Parent container %s stopped, stopping children...", 
			dm.getContainerName(containerID))
		
		// Stop all child containers
		dm.stopChildrenUnsafe(containerID, children, "parent stopped")

		// Set up restart timeout timer
		if parentState.RestartTimer != nil {
			parentState.RestartTimer.Stop()
		}
		
		// Check if parent container still exists to determine timeout duration
		var timeoutDuration time.Duration
		var timeoutType string
		
		if dm.containerExists(containerID) {
			timeoutDuration = dm.restartTimeout
			timeoutType = "restart"
		} else {
			timeoutDuration = dm.recreationTimeout
			timeoutType = "recreation"
		}
		
		log.Printf("Parent container %s stopped, starting %s timeout (%v)...", 
			dm.getContainerName(containerID), timeoutType, timeoutDuration)
		
		parentState.RestartTimer = time.AfterFunc(timeoutDuration, func() {
			dm.mutex.Lock()
			defer dm.mutex.Unlock()
			
			// Check if this parent was successfully recreated
			if dm.recreatedParents[containerID] {
				delete(dm.recreatedParents, containerID) // Clean up
				return
			}
			
			// Just log that timeout exceeded - don't modify WasRunning states
			// The decision about starting containers should be made when parent actually starts
			if dm.containerExists(containerID) {
				log.Printf("Restart timeout exceeded for parent %s", dm.getContainerName(containerID))
			} else {
				log.Printf("Recreation timeout exceeded for parent %s", dm.getContainerName(containerID))
			}
		})
	}
}

func (dm *DependencyManager) stopChildrenUnsafe(parentID string, children []string, reason string) {
	// Collect containers to stop and update their states first
	var containersToStop []string
	var containerNames []string
	
	for _, childID := range children {
		inspect, err := dm.client.ContainerInspect(dm.ctx, childID)
		if err != nil {
			log.Printf("Error inspecting child container %s: %v", childID, err)
			continue
		}

		childName := dm.getContainerName(childID)
		
		// Store current state for potential future restart
		// Only update WasRunning if the container is actually running
		// Don't overwrite with false if it was running before
		if childState, exists := dm.childStates[childID]; exists {
			if inspect.State.Running {
				childState.WasRunning = true
			}
		} else {
			// Create new state if it doesn't exist
			dm.childStates[childID] = &ContainerState{
				WasRunning: inspect.State.Running,
			}
		}

		if inspect.State.Running && !dm.stoppedContainers[childID] {
			// Mark as being stopped immediately to prevent duplicates
			dm.stoppedContainers[childID] = true
			containersToStop = append(containersToStop, childID)
			containerNames = append(containerNames, childName)
		} else if !dm.stoppedContainers[childID] {
			log.Printf("Child container %s was already stopped", childName)
		}
	}
	
	if len(containersToStop) == 0 {
		return
	}
	
	log.Printf("Stopping %d child containers (%s)", len(containersToStop), reason)
	
	// Send notification about stopping containers
	parentName := dm.getContainerName(parentID)
	message := dm.formatContainerMessage("Stopping", containerNames, parentName)
	dm.sendNotification("Docker", message, "normal")
	
	// Stop all containers asynchronously
	for i, childID := range containersToStop {
		childName := containerNames[i]
		go func(id, name string) {
			log.Printf("Stopping child container %s (%s)", name, reason)
			timeout := int(10) // 10 seconds timeout for stopping
			if err := dm.client.ContainerStop(dm.ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
				log.Printf("Error stopping child container %s: %v", name, err)
			} else {
				log.Printf("Successfully stopped child container %s", name)
			}
			
			// Clean up after 5 seconds
			go func() {
				time.Sleep(5 * time.Second)
				dm.mutex.Lock()
				delete(dm.stoppedContainers, id)
				dm.mutex.Unlock()
			}()
		}(childID, childName)
	}
}

func (dm *DependencyManager) recreateContainer(containerName string) error {
	log.Printf("Recreating container: %s", containerName)
	
	templateFile := fmt.Sprintf("%s.json", containerName)
	templatePath := fmt.Sprintf("%s%s", dm.templatesPath, templateFile)
	
	// Check if template exists
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
	// For restart: start containers based on autostart setting (ignore WasRunning for normal restarts)
	
	// Collect containers to start
	var containersToStart []string
	var containerNames []string
	
	for _, childID := range children {
		childName := dm.getContainerName(childID)
		
		// Check autostart setting - this is the only criteria for normal restarts
		if !dm.getContainerAutostart(childName) {
			log.Printf("Child container %s has autostart disabled, keeping it stopped", childName)
			continue
		}
		
		// Check if child is already running
		inspect, err := dm.client.ContainerInspect(dm.ctx, childID)
		if err != nil {
			log.Printf("Error inspecting child container %s: %v", childID, err)
			continue
		}

		if inspect.State.Running {
			log.Printf("Child container %s is already running", childName)
			continue
		}
		
		containersToStart = append(containersToStart, childID)
		containerNames = append(containerNames, childName)
	}
	
	if len(containersToStart) == 0 {
		return
	}
	
	parentName := dm.getContainerName(parentID)
	log.Printf("Starting %d child containers for parent %s", len(containersToStart), parentName)
	
	// Send notification about starting containers
	message := dm.formatContainerMessage("Starting", containerNames, parentName)
	dm.sendNotification("Docker", message, "normal")
	
	// Start all containers asynchronously
	for i, childID := range containersToStart {
		childName := containerNames[i]
		go func(id, name string) {
			log.Printf("Starting child container %s", name)
			if err := dm.client.ContainerStart(dm.ctx, id, container.StartOptions{}); err != nil {
				log.Printf("Error starting child container %s: %v", name, err)
			} else {
				log.Printf("Successfully started child container %s", name)
			}
		}(childID, childName)
	}
}

func (dm *DependencyManager) startChildrenForRecreation(parentID string, children []string) {
	// For recreation: check if containers exist, recreate if needed, then start
	currentContainers := make(map[string]bool)
	
	// Get current MOS container list to check which ones still exist
	containers, err := dm.client.ContainerList(dm.ctx, container.ListOptions{
		All:     true,
		Filters: dm.getMOSContainerFilter(),
	})
	if err != nil {
		log.Printf("Error listing containers: %v", err)
		return
	}
	
	for _, c := range containers {
		currentContainers[c.ID] = true
	}
	
	for _, childID := range children {
		childName := dm.getContainerName(childID)
		
		// Check autostart setting
		if !dm.getContainerAutostart(childName) {
			log.Printf("Child container %s has autostart disabled, keeping it stopped", childName)
			continue
		}
		
		childState, exists := dm.childStates[childID]
		if !exists {
			log.Printf("DEBUG: No state found for child %s in recreation, keeping it stopped", childName)
			continue
		}
		
		log.Printf("DEBUG: Child %s recreation state: WasRunning=%v", childName, childState.WasRunning)
		
		if !childState.WasRunning {
			log.Printf("Child container %s was not running before, keeping it stopped", childName)
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
			log.Printf("Child container %s is already running", childName)
			continue
		}

		log.Printf("Starting child container %s", childName)
		if err := dm.client.ContainerStart(dm.ctx, childID, container.StartOptions{}); err != nil {
			log.Printf("Error starting child container %s: %v", childName, err)
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

	log.Printf("Container dependencies (restart timeout: %v, recreation timeout: %v):", dm.restartTimeout, dm.recreationTimeout)
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
