# MOS Docker Watchdog

MOS Docker Watchdog is a small **Go-based service** designed to monitor Docker containers and improve system stability.

---

## Overview

The watchdog continuously observes Docker containers and detects unstable behavior such as **restart loops**.

If a container restarts **5 times within 30 seconds**, it is considered unhealthy and will be **automatically stopped**.  
In this case, a notification is sent via the **MOS Notification Service** to inform the user.

---

## Container Dependency Handling

MOS Docker Watchdog is aware of **container network dependencies**.

For setups where multiple containers depend on a shared network container (for example a **VPN container acting as a parent**), the watchdog ensures consistent behavior:

- Child containers attached to a parent network container are
  - started
  - stopped
  - rebuilt

depending on the action required for the parent container.

This ensures dependent containers stay in a valid and functional state.

---

## Design Philosophy

MOS Docker Watchdog is written **with performance and efficiency in mind**.

- Implemented in **Go** for low memory usage and fast execution
- Listens directly to the **Docker socket** for events
- Avoids polling and unnecessary system calls
- Designed to run continuously with **minimal CPU and memory overhead**

---

## Purpose

The watchdog is intended to prevent endless reboot loops, reduce manual intervention, and keep container-based services in a healthy and predictable state.

---

### Documentation

Comprehensive documentation is still a TODO for this repository.

For a more general overview please refer to [MOS Releases](https://github.com/ich777/mos-releases/blob/master/README.md) repostiory.

---

## Contributing

Contributions, bug reports, and feature requests are welcome.  
Please use GitHub Issues to report bugs or discuss ideas before submitting larger changes.
