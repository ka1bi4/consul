package agent

import (
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul/consul"
	"github.com/hashicorp/consul/consul/structs"
)

const (
	syncStaggerIntv = 3 * time.Second
	syncRetryIntv   = 15 * time.Second

	// permissionDenied is returned when an ACL based rejection happens
	permissionDenied = "Permission denied"
)

// syncStatus is used to represent the difference between
// the local and remote state, and if action needs to be taken
type syncStatus struct {
	remoteDelete bool // Should this be deleted from the server
	inSync       bool // Is this in sync with the server
}

// localState is used to represent the node's services,
// and checks. We used it to perform anti-entropy with the
// catalog representation
type localState struct {
	// paused is used to check if we are paused. Must be the first
	// element due to a go bug.
	paused int32

	sync.Mutex
	logger *log.Logger

	// Config is the agent config
	config *Config

	// iface is the consul interface to use for keeping in sync
	iface consul.Interface

	// Services tracks the local services
	services      map[string]*structs.NodeService
	serviceStatus map[string]syncStatus

	// Checks tracks the local checks
	checks      map[string]*structs.HealthCheck
	checkStatus map[string]syncStatus

	// Used to track checks that are being deferred
	deferCheck map[string]*time.Timer

	// consulCh is used to inform of a change to the known
	// consul nodes. This may be used to retry a sync run
	consulCh chan struct{}

	// triggerCh is used to inform of a change to local state
	// that requires anti-entropy with the server
	triggerCh chan struct{}
}

// Init is used to initialize the local state
func (l *localState) Init(config *Config, logger *log.Logger) {
	l.config = config
	l.logger = logger
	l.services = make(map[string]*structs.NodeService)
	l.serviceStatus = make(map[string]syncStatus)
	l.checks = make(map[string]*structs.HealthCheck)
	l.checkStatus = make(map[string]syncStatus)
	l.deferCheck = make(map[string]*time.Timer)
	l.consulCh = make(chan struct{}, 1)
	l.triggerCh = make(chan struct{}, 1)
}

// SetIface is used to set the Consul interface. Must be set prior to
// starting anti-entropy
func (l *localState) SetIface(iface consul.Interface) {
	l.iface = iface
}

// changeMade is used to trigger an anti-entropy run
func (l *localState) changeMade() {
	select {
	case l.triggerCh <- struct{}{}:
	default:
	}
}

// ConsulServerUp is used to inform that a new consul server is now
// up. This can be used to speed up the sync process if we are blocking
// waiting to discover a consul server
func (l *localState) ConsulServerUp() {
	select {
	case l.consulCh <- struct{}{}:
	default:
	}
}

// Pause is used to pause state synchronization, this can be
// used to make batch changes
func (l *localState) Pause() {
	atomic.StoreInt32(&l.paused, 1)
}

// Resume is used to resume state synchronization
func (l *localState) Resume() {
	atomic.StoreInt32(&l.paused, 0)
	l.changeMade()
}

// isPaused is used to check if we are paused
func (l *localState) isPaused() bool {
	return atomic.LoadInt32(&l.paused) == 1
}

// AddService is used to add a service entry to the local state.
// This entry is persistent and the agent will make a best effort to
// ensure it is registered
func (l *localState) AddService(service *structs.NodeService) {
	// Assign the ID if none given
	if service.ID == "" && service.Service != "" {
		service.ID = service.Service
	}

	l.Lock()
	defer l.Unlock()

	l.services[service.ID] = service
	l.serviceStatus[service.ID] = syncStatus{}
	l.changeMade()
}

// RemoveService is used to remove a service entry from the local state.
// The agent will make a best effort to ensure it is deregistered
func (l *localState) RemoveService(serviceID string) {
	l.Lock()
	defer l.Unlock()

	delete(l.services, serviceID)
	l.serviceStatus[serviceID] = syncStatus{remoteDelete: true}
	l.changeMade()
}

// Services returns the locally registered services that the
// agent is aware of and are being kept in sync with the server
func (l *localState) Services() map[string]*structs.NodeService {
	services := make(map[string]*structs.NodeService)
	l.Lock()
	defer l.Unlock()

	for name, serv := range l.services {
		services[name] = serv
	}
	return services
}

// AddCheck is used to add a health check to the local state.
// This entry is persistent and the agent will make a best effort to
// ensure it is registered
func (l *localState) AddCheck(check *structs.HealthCheck) {
	// Set the node name
	check.Node = l.config.NodeName

	l.Lock()
	defer l.Unlock()

	l.checks[check.CheckID] = check
	l.checkStatus[check.CheckID] = syncStatus{}
	l.changeMade()
}

// RemoveCheck is used to remove a health check from the local state.
// The agent will make a best effort to ensure it is deregistered
func (l *localState) RemoveCheck(checkID string) {
	l.Lock()
	defer l.Unlock()

	delete(l.checks, checkID)
	l.checkStatus[checkID] = syncStatus{remoteDelete: true}
	l.changeMade()
}

// UpdateCheck is used to update the status of a check
func (l *localState) UpdateCheck(checkID, status, output string) {
	l.Lock()
	defer l.Unlock()

	check, ok := l.checks[checkID]
	if !ok {
		return
	}

	// Do nothing if update is idempotent
	if check.Status == status && check.Output == output {
		return
	}

	// Defer a sync if the output has changed. This is an optimization around
	// frequent updates of output. Instead, we update the output internally,
	// and periodically do a write-back to the servers. If there is a status
	// change we do the write immediately.
	if l.config.CheckUpdateInterval > 0 && check.Status == status {
		check.Output = output
		if _, ok := l.deferCheck[checkID]; !ok {
			deferSync := time.AfterFunc(l.config.CheckUpdateInterval, func() {
				l.Lock()
				if _, ok := l.checkStatus[checkID]; ok {
					l.checkStatus[checkID] = syncStatus{inSync: false}
					l.changeMade()
				}
				delete(l.deferCheck, checkID)
				l.Unlock()
			})
			l.deferCheck[checkID] = deferSync
		}
		return
	}

	// Update status and mark out of sync
	check.Status = status
	check.Output = output
	l.checkStatus[checkID] = syncStatus{inSync: false}
	l.changeMade()
}

// Checks returns the locally registered checks that the
// agent is aware of and are being kept in sync with the server
func (l *localState) Checks() map[string]*structs.HealthCheck {
	checks := make(map[string]*structs.HealthCheck)
	l.Lock()
	defer l.Unlock()

	for name, check := range l.checks {
		checks[name] = check
	}
	return checks
}

// antiEntropy is a long running method used to perform anti-entropy
// between local and remote state.
func (l *localState) antiEntropy(shutdownCh chan struct{}) {
SYNC:
	// Sync our state with the servers
	for {
		err := l.setSyncState()
		if err == nil {
			break
		}
		l.logger.Printf("[ERR] agent: failed to sync remote state: %v", err)
		select {
		case <-l.consulCh:
			// Stagger the retry on leader election, avoid a thundering heard
			select {
			case <-time.After(randomStagger(aeScale(syncStaggerIntv, len(l.iface.LANMembers())))):
			case <-shutdownCh:
				return
			}
		case <-time.After(syncRetryIntv + randomStagger(aeScale(syncRetryIntv, len(l.iface.LANMembers())))):
		case <-shutdownCh:
			return
		}
	}

	// Force-trigger AE to pickup any changes
	l.changeMade()

	// Schedule the next full sync, with a random stagger
	aeIntv := aeScale(l.config.AEInterval, len(l.iface.LANMembers()))
	aeIntv = aeIntv + randomStagger(aeIntv)
	aeTimer := time.After(aeIntv)

	// Wait for sync events
	for {
		select {
		case <-aeTimer:
			goto SYNC
		case <-l.triggerCh:
			// Skip the sync if we are paused
			if l.isPaused() {
				continue
			}
			if err := l.syncChanges(); err != nil {
				l.logger.Printf("[ERR] agent: failed to sync changes: %v", err)
			}
		case <-shutdownCh:
			return
		}
	}
}

// setSyncState does a read of the server state, and updates
// the local syncStatus as appropriate
func (l *localState) setSyncState() error {
	req := structs.NodeSpecificRequest{
		Datacenter:   l.config.Datacenter,
		Node:         l.config.NodeName,
		QueryOptions: structs.QueryOptions{Token: l.config.ACLToken},
	}
	var out1 structs.IndexedNodeServices
	var out2 structs.IndexedHealthChecks
	if e := l.iface.RPC("Catalog.NodeServices", &req, &out1); e != nil {
		return e
	}
	if err := l.iface.RPC("Health.NodeChecks", &req, &out2); err != nil {
		return err
	}
	services := out1.NodeServices
	checks := out2.HealthChecks

	l.Lock()
	defer l.Unlock()

	if services != nil {
		for id, service := range services.Services {
			// If we don't have the service locally, deregister it
			existing, ok := l.services[id]
			if !ok {
				l.serviceStatus[id] = syncStatus{remoteDelete: true}
				continue
			}

			// If our definition is different, we need to update it
			equal := reflect.DeepEqual(existing, service)
			l.serviceStatus[id] = syncStatus{inSync: equal}
		}
	}

	for _, check := range checks {
		// If we don't have the check locally, deregister it
		id := check.CheckID
		existing, ok := l.checks[id]
		if !ok {
			// The Serf check is created automatically, and does not
			// need to be registered
			if id == consul.SerfCheckID {
				continue
			}
			l.checkStatus[id] = syncStatus{remoteDelete: true}
			continue
		}

		// If our definition is different, we need to update it
		var equal bool
		if l.config.CheckUpdateInterval == 0 {
			equal = reflect.DeepEqual(existing, check)
		} else {
			eCopy := new(structs.HealthCheck)
			*eCopy = *existing
			eCopy.Output = ""
			check.Output = ""
			equal = reflect.DeepEqual(eCopy, check)
		}

		// Update the status
		l.checkStatus[id] = syncStatus{inSync: equal}
	}
	return nil
}

// syncChanges is used to scan the status our local services and checks
// and update any that are out of sync with the server
func (l *localState) syncChanges() error {
	l.Lock()
	defer l.Unlock()

	// Sync the checks first. This allows registering the service in the
	// same transaction as its checks.
	var checkIDs []string
	for id, status := range l.checkStatus {
		if status.remoteDelete {
			if err := l.deleteCheck(id); err != nil {
				return err
			}
		} else if !status.inSync {
			// Cancel a deferred sync
			if timer, ok := l.deferCheck[id]; ok {
				timer.Stop()
				delete(l.deferCheck, id)
			}

			checkIDs = append(checkIDs, id)
		} else {
			l.logger.Printf("[DEBUG] agent: Check '%s' in sync", id)
		}
	}
	if len(checkIDs) > 0 {
		if err := l.syncChecks(checkIDs); err != nil {
			return err
		}
	}

	// Sync any remaining services.
	for id, status := range l.serviceStatus {
		if status.remoteDelete {
			if err := l.deleteService(id); err != nil {
				return err
			}
		} else if !status.inSync {
			if err := l.syncService(id); err != nil {
				return err
			}
		} else {
			l.logger.Printf("[DEBUG] agent: Service '%s' in sync", id)
		}
	}

	return nil
}

// deleteService is used to delete a service from the server
func (l *localState) deleteService(id string) error {
	req := structs.DeregisterRequest{
		Datacenter:   l.config.Datacenter,
		Node:         l.config.NodeName,
		ServiceID:    id,
		WriteRequest: structs.WriteRequest{Token: l.config.ACLToken},
	}
	var out struct{}
	err := l.iface.RPC("Catalog.Deregister", &req, &out)
	if err == nil {
		delete(l.serviceStatus, id)
		l.logger.Printf("[INFO] agent: Deregistered service '%s'", id)
	}
	return err
}

// deleteCheck is used to delete a service from the server
func (l *localState) deleteCheck(id string) error {
	req := structs.DeregisterRequest{
		Datacenter:   l.config.Datacenter,
		Node:         l.config.NodeName,
		CheckID:      id,
		WriteRequest: structs.WriteRequest{Token: l.config.ACLToken},
	}
	var out struct{}
	err := l.iface.RPC("Catalog.Deregister", &req, &out)
	if err == nil {
		delete(l.checkStatus, id)
		l.logger.Printf("[INFO] agent: Deregistered check '%s'", id)
	}
	return err
}

// syncService is used to sync a service to the server
func (l *localState) syncService(id string) error {
	req := structs.RegisterRequest{
		Datacenter:   l.config.Datacenter,
		Node:         l.config.NodeName,
		Address:      l.config.AdvertiseAddr,
		Service:      l.services[id],
		WriteRequest: structs.WriteRequest{Token: l.config.ACLToken},
	}
	var out struct{}
	err := l.iface.RPC("Catalog.Register", &req, &out)
	if err == nil {
		l.serviceStatus[id] = syncStatus{inSync: true}
		l.logger.Printf("[INFO] agent: Synced service '%s'", id)
	} else if strings.Contains(err.Error(), permissionDenied) {
		l.serviceStatus[id] = syncStatus{inSync: true}
		l.logger.Printf("[WARN] agent: Service '%s' registration blocked by ACLs", id)
		return nil
	}
	return err
}

// syncChecks is used to sync checks to the server. If a check is associated
// with a service and the service is out of sync, it will piggyback with the
// sync so that it is updated as part of the same transaction.
func (l *localState) syncChecks(checkIDs []string) error {
	checkMap := make(map[string]structs.HealthChecks)

	for _, id := range checkIDs {
		if check, ok := l.checks[id]; ok {
			checkMap[check.ServiceID] = append(checkMap[check.ServiceID], check)
		}
	}

	for serviceID, checks := range checkMap {
		// Create the sync request
		req := structs.RegisterRequest{
			Datacenter:   l.config.Datacenter,
			Node:         l.config.NodeName,
			Address:      l.config.AdvertiseAddr,
			WriteRequest: structs.WriteRequest{Token: l.config.ACLToken},
		}

		// Attach the service if it should also be synced
		if service, ok := l.services[serviceID]; ok {
			if status, ok := l.serviceStatus[serviceID]; ok && !status.inSync {
				req.Service = service
			}
		}

		// Send single Check element for backwards compat with 0.4.x
		if len(checks) == 1 {
			req.Check = checks[0]
		} else {
			req.Checks = checks
		}

		// Perform the sync
		var out struct{}
		if err := l.iface.RPC("Catalog.Register", &req, &out); err != nil {
			if strings.Contains(err.Error(), permissionDenied) {
				for _, check := range checks {
					l.checkStatus[check.CheckID] = syncStatus{inSync: true}
					l.logger.Printf(
						"[WARN] agent: Check '%s' registration blocked by ACLs",
						check.CheckID)
				}
				return nil
			}
			return err
		}

		// Mark the checks and services as synced
		if req.Service != nil {
			l.serviceStatus[serviceID] = syncStatus{inSync: true}
			l.logger.Printf("[INFO] agent: Synced service '%s'", serviceID)
		}
		for _, check := range checks {
			l.checkStatus[check.CheckID] = syncStatus{inSync: true}
			l.logger.Printf("[INFO] agent: Synced check '%s'", check.CheckID)
		}
	}

	return nil
}
