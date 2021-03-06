package structs

import (
	"sync"

	nomad "github.com/hashicorp/nomad/api"
)

// NewWorkerPool is a constructor method that provides a pointer to a new
// worker pool object
func NewWorkerPool() *WorkerPool {
	// Return a new worker pool object with default values set.
	return &WorkerPool{
		Cooldown:         300,
		FaultTolerance:   1,
		Nodes:            make(map[string]*nomad.Node),
		RetryThreshold:   3,
		ScalingThreshold: 3,
	}
}

// NodeRegistry tracks worker pools and nodes discovered by Replicator.
// The object contains a lock to provide mutual exclusion protection.
type NodeRegistry struct {
	LastChangeIndex     uint64
	Lock                sync.RWMutex
	RegisteredNodes     map[string]string
	RegisteredNodesHash uint64
	WorkerPools         map[string]*WorkerPool
}

// WorkerPool represents the scaling configuration of a discovered
// worker pool and its associated node membership.
type WorkerPool struct {
	Cooldown         int                    `mapstructure:"replicator_cooldown"`
	FaultTolerance   int                    `mapstructure:"replicator_node_fault_tolerance"`
	Max              int                    `mapstructure:"replicator_max"`
	Min              int                    `mapstructure:"replicator_min"`
	Name             string                 `mapstructure:"replicator_worker_pool"`
	Nodes            map[string]*nomad.Node `hash:"ignore"`
	NotificationUID  string                 `mapstructure:"replicator_notification_uid"`
	ProtectedNode    string                 `hash:"ignore"`
	Region           string                 `mapstructure:"replicator_region"`
	RetryThreshold   int                    `mapstructure:"replicator_retry_threshold"`
	ScalingEnabled   bool                   `mapstructure:"replicator_enabled"`
	ScalingThreshold int                    `mapstructure:"replicator_scaling_threshold"`
}
