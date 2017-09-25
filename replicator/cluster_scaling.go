package replicator

import (
	"fmt"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"

	"github.com/elsevier-core-engineering/replicator/client"
	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/notifier"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
)

// newNodeRegistry returns a new NodeRegistry object to allow Replicator
// to track discovered worker pools and nodes.
func newNodeRegistry() *structs.NodeRegistry {
	return &structs.NodeRegistry{
		WorkerPools:     make(map[string]*structs.WorkerPool),
		RegisteredNodes: make(map[string]string),
		Lock:            sync.RWMutex{},
	}
}

// checkPoolScalingThreshold determines if we've reached the required number
// of consecutive scaling attempts.
func checkPoolScalingThreshold(state *structs.ScalingState, direction string,
	workerPool *structs.WorkerPool, config *structs.Config) (scale bool) {

	switch direction {
	case client.ScalingDirectionIn:
		state.ScaleInRequests++
		state.ScaleOutRequests = 0
		if state.ScaleInRequests == workerPool.ScalingThreshold {
			logging.Debug("core/cluster_scaling: cluster scale-in request %v for "+
				"worker pool %v meets the threshold %v", state.ScaleInRequests,
				workerPool.Name, workerPool.ScalingThreshold)
			state.ScaleInRequests = 0
			scale = true
		} else {
			logging.Debug("core/cluster_scaling: cluster scale-in request %v for "+
				"worker pool %v does not meet the threshold %v", state.ScaleInRequests,
				workerPool.Name, workerPool.ScalingThreshold)
		}

	case client.ScalingDirectionOut:
		state.ScaleOutRequests++
		state.ScaleInRequests = 0
		if state.ScaleOutRequests == workerPool.ScalingThreshold {
			logging.Debug("core/cluster_scaling: cluster scale-out request %v for "+
				"worker pool %v meets the threshold %v", state.ScaleOutRequests,
				workerPool.Name, workerPool.ScalingThreshold)
			state.ScaleOutRequests = 0
			scale = true
		} else {
			logging.Debug("core/cluster_scaling: cluster scale-out request %v for "+
				"worker pool %v does not meet the threshold %v", state.ScaleOutRequests,
				workerPool.Name, workerPool.ScalingThreshold)
		}

	default:
		state.ScaleInRequests = 0
		state.ScaleOutRequests = 0
	}

	// One way or another we have updated our internal state, therefore this needs
	// to be written to our persistent state store.
	if err := config.ConsulClient.PersistState(state); err != nil {
		logging.Error("core:cluster_scaling: unable to update cluster scaling "+
			"state to persistent store at path %v: %v", state.StatePath, err)
		scale = false
	}

	return scale
}

// asyncClusterScaling triggers concurrent cluster scaling operations for
// each worker group in the node registry.
func (r *Runner) asyncClusterScaling(nodeRegistry *structs.NodeRegistry,
	jobRegistry *structs.JobScalingPolicies) {

	// Setup our wait group to ensure we block until all worker pool scaling
	// operations have completed.
	var wg sync.WaitGroup

	// Register an entry to the wait group for each worker pool.
	wg.Add(len(nodeRegistry.WorkerPools))

	for _, workerPool := range nodeRegistry.WorkerPools {
		go r.workerPoolScaling(workerPool.Name, nodeRegistry, jobRegistry, &wg)
	}

	// Block on all worker pool scaling threads.
	wg.Wait()

	return
}

// workerPoolScaling is a thread safe method for scaling a worker pool.
func (r *Runner) workerPoolScaling(poolName string,
	NodeRegistry *structs.NodeRegistry, jobs *structs.JobScalingPolicies,
	wg *sync.WaitGroup) {

	// Obtain a read-only lock on the Node registry.
	NodeRegistry.Lock.RLock()
	defer NodeRegistry.Lock.RUnlock()

	// Obtain a reference to our worker pool.
	workerPool := NodeRegistry.WorkerPools[poolName]

	// Inform the wait group we have finished our task upon completion.
	defer wg.Done()

	// Setup references to clients for Nomad and Consul.
	nomadClient := r.config.NomadClient
	consulClient := r.config.ConsulClient

	// Initialize a new disposable capacity object.
	poolCapacity := &structs.ClusterCapacity{}

	// Initialize a new scaling state object and set helper fields.
	poolState := &structs.ScalingState{}
	poolState.ResourceType = ClusterType
	poolState.ResourceName = workerPool.Name
	poolState.StatePath = r.config.ConsulKeyRoot + "/state/nodes/" +
		workerPool.Name

	// Attempt to load state from persistent storage.
	consulClient.ReadState(poolState, true)

	// Setup a failure message to pass to the failsafe check.
	message := &notifier.FailureMessage{
		AlertUID:     workerPool.NotificationUID,
		ResourceID:   workerPool.Name,
		ResourceType: ClusterType,
	}

	// If the worker pool is in failsafe mode, decline to perform any scaling
	// evaluation or action.
	if !FailsafeCheck(poolState, r.config, workerPool.RetryThreshold, message) {
		logging.Warning("core/cluster_scaling: worker pool %v is in failsafe "+
			"mode, no scaling evaluations will be performed", workerPool.Name)
		return
	}

	// Evaluate worker pool to determine if a scaling operation is required.
	scale, err := nomadClient.EvaluatePoolScaling(poolCapacity, workerPool, jobs)
	if err != nil || !scale {
		logging.Debug("core/cluster_scaling: scaling operation for worker pool %v "+
			"is either not required or not permitted: %v", workerPool.Name, err)
		return
	}

	// Determine if the scaling cooldown threshold has been met.
	ok := checkCooldownThreshold(workerPool.Cooldown, workerPool.Name, poolState)
	if !ok {
		return
	}

	// Determine if we've reached the required number of consecutive scaling
	// requests.
	ok = checkPoolScalingThreshold(poolState, poolCapacity.ScalingDirection,
		workerPool, r.config)
	if !ok {
		return
	}

	// Setup session to AWS auto scaling service.
	asgSess := client.NewAWSAsgService(workerPool.Region)

	if poolCapacity.ScalingDirection != client.ScalingDirectionNone {
		scaleMetric := poolCapacity.ScalingMetric

		logging.Info("core/cluster_scaling: worker pool %v requires a scaling "+
			"operation: (Direction: %v, Nodes: %v, Metric: %v, Capacity: %v, "+
			"Utilization: %v, Max Allowed: %v)", workerPool.Name,
			poolCapacity.ScalingDirection, len(workerPool.Nodes), scaleMetric.Type,
			scaleMetric.Capacity, scaleMetric.Utilization,
			poolCapacity.MaxAllowedUtilization)
	}

	if poolCapacity.ScalingDirection == client.ScalingDirectionOut {
		// If we've determined the worker pool should be scaled out, initiate
		// the scaling operation.
		err = client.ScaleOutCluster(workerPool.Name, poolCapacity.NodeCount, asgSess)
		if err != nil {
			metrics.IncrCounter([]string{"cluster", workerPool.Name, "scale_out", "failure"}, 1)
			logging.Error("core/cluster_scaling: unable to successfully initiate a "+
				"scaling operation against worker pool %v: %v",
				workerPool.Name, err)
			return
		}

		// Verify scaling operation has completed successfully.
		if ok := r.verifyPoolScaling(workerPool, poolState, r.config); !ok {
			metrics.IncrCounter([]string{"cluster", workerPool.Name, "scale_out", "failure"}, 1)
			logging.Error("core/cluster_scaling: unable to successfully verify "+
				"scaling operation completed successfully against worker pool %v: %v",
				workerPool.Name, err)
			return
		}
	}

	if poolCapacity.ScalingDirection == client.ScalingDirectionIn {
		// Identify the least allocated node in the worker pool.
		nodeID, nodeIP := nomadClient.LeastAllocatedNode(poolCapacity,
			workerPool.ProtectedNode)
		if nodeIP == "" || nodeID == "" {
			logging.Error("core/cluster_scaling: unable to identify the least "+
				"allocated node in worker pool %v", workerPool.Name)
			return
		}
		logging.Info("client/nomad: identified node %v as the least allocated "+
			"node in worker pool %v", nodeID, workerPool.Name)

		// Place the least allocated noded in drain mode.
		logging.Info("client/nomad: placing node %v from worker pool %v in drain "+
			"mode", nodeID, workerPool.Name)
		if err = nomadClient.DrainNode(nodeID); err != nil {
			logging.Error("core/cluster_scaling: an error occurred while "+
				"attempting to place node %v from worker pool %v in drain mode: "+
				"%v", nodeID, workerPool.Name, err)
			metrics.IncrCounter([]string{"cluster", workerPool.Name, "scale_in", "failure"}, 1)
			return
		}

		// Detach node from worker pool and terminate the instance.
		logging.Info("core/cluster_scaling: terminating node %v from worker "+
			"pool %v", nodeID, workerPool.Name)
		if err = client.ScaleInCluster(workerPool.Name, nodeIP, asgSess); err != nil {
			metrics.IncrCounter([]string{"cluster", workerPool.Name, "scale_in", "failure"}, 1)
			logging.Error("core/cluster_scaling: attempt to terminate node %v from "+
				"worker pool %v failed: %v", nodeID, workerPool.Name, err)
			return
		}

		// Update the last scaling event timestamp and reset failure counts.
		poolState.LastScalingEvent = time.Now()
		poolState.FailureCount = 0

		// Attempt to update state tracking information in Consul.
		if err = consulClient.PersistState(poolState); err != nil {
			logging.Error("core/cluster_scaling: %v", err)
		}
	}

	// Our metric counter to track successful cluster scaling activities.
	m := fmt.Sprintf("scale_%s", strings.ToLower(poolCapacity.ScalingDirection))
	metrics.IncrCounter([]string{"cluster", workerPool.Name, m, "success"}, 1)

	return
}

// verifyPoolScaling verifies a cluster scaling operation completed
// successfully and retries the operation if failures are detected.
func (r *Runner) verifyPoolScaling(workerPool *structs.WorkerPool,
	state *structs.ScalingState, config *structs.Config) (ok bool) {

	// Setup references to clients for Nomad and Consul.
	nomadClient := r.config.NomadClient
	consulClient := r.config.ConsulClient

	// Setup session to AWS auto scaling service.
	asgSess := client.NewAWSAsgService(workerPool.Region)

	// Identify the most recently launched instance and verify it joins the
	// worker pool in a healthy state. If any failures are detected, retry
	// up to the configured retry threshold.
	for state.FailureCount <= workerPool.RetryThreshold {
		if state.FailureCount > 0 {
			logging.Info("core/cluster_scaling: attempting to launch a new worker "+
				"node in worker pool %v, previous node failures: %v", workerPool.Name,
				state.FailureCount)
		}

		// Identify the most recently launched instance in the worker pool.
		newestNode, err := client.GetMostRecentInstance(workerPool.Name,
			workerPool.Region)
		if err != nil {
			logging.Error("core/cluster_scaling: failed to identify the most "+
				"recently launched instance in worker pool %v: %v",
				workerPool.Name, err)

			// Increment failure count and write state object.
			state.FailureCount++
			if err = consulClient.PersistState(state); err != nil {
				logging.Error("core/cluster_scaling: %v", err)
			}
			continue
		}

		// Verify the most recently launched instance has completed bootstrapping
		// and successfully joined the worker pool.
		if healthy := nomadClient.VerifyNodeHealth(newestNode); healthy {
			// Reset node failure count once we have a verified healthy worker.
			state.FailureCount = 0

			// Update the last scaling event timestamp.
			state.LastScalingEvent = time.Now()

			// Attempt to update state tracking information in Consul.
			if err = consulClient.PersistState(state); err != nil {
				logging.Error("core/cluster_scaling: %v", err)
			}

			return true
		}

		// Increment failure count and write state object.
		state.FailureCount++
		logging.Error("core/cluster_scaling: new node %v failed to successfully "+
			"join worker pool %v, incrementing node failure count to %v and "+
			"terminating instance", newestNode, workerPool.Name,
			state.FailureCount)

		// Attempt to update state tracking information in Consul.
		if err = consulClient.PersistState(state); err != nil {
			logging.Error("core/cluster_scaling: %v", err)
		}

		// Translate the IP address of the most recently launched instance to the
		// EC2 instance ID so we can terminate it.
		instanceID := client.TranslateIptoID(newestNode, workerPool.Region)

		// If we've reached the retry threshold, attempt to detach the last
		// failed instance and decrement the autoscaling group desired count.
		if state.FailureCount == workerPool.RetryThreshold {
			err := client.DetachInstance(workerPool.Name, instanceID, asgSess)
			if err != nil {
				logging.Error("core/cluster_scaling: an error occurred while "+
					"attempting to detach the failed instance %v from the worker pool "+
					"%v: %v", instanceID, workerPool.Name, err)
			}

			// Place worker pool in failsafe mode.
			state.FailsafeMode = true
			metrics.IncrCounter([]string{"failsafe_count"}, 1)

			// Attempt to update state tracking information in Consul.
			if err = consulClient.PersistState(state); err != nil {
				logging.Error("core/cluster_scaling: %v", err)
			}

			return
		}

		// Attempt to clean up the most recent instance to allow the auto scaling
		// group to launch a new one.
		if err := client.TerminateInstance(instanceID, workerPool.Region); err != nil {
			logging.Error("core/cluster_scaling: an error occurred while "+
				"attempting to terminate instance %v from worker pool %v: %v",
				instanceID, workerPool.Name, err)
		}
	}

	return
}

// CheckCooldownThreshold checks to see if a scaling cooldown threshold has
// been reached.
func checkCooldownThreshold(interval int, workerPool string,
	state *structs.ScalingState) bool {

	if state.LastScalingEvent.IsZero() {
		logging.Debug("core/cluster_scaling: no previous scaling operations for "+
			"worker pool %v have occurred, scaling operations should be permitted.",
			workerPool)
		return true
	}

	// Calculate the cooldown threshold.
	cooldown := state.LastScalingEvent.Add(time.Duration(interval) * time.Second)

	if time.Now().Before(cooldown) {
		logging.Debug("core/cluster_scaling: cluster scaling cooldown threshold "+
			"has not been reached: %v, scaling operations for worker pool %v should "+
			"not be permitted", cooldown, workerPool)
		return false
	}

	logging.Debug("core/cluster_scaling: cluster scaling cooldown threshold %v "+
		"has been reached, scaling operations for worker pool %v should be "+
		"permitted", cooldown, workerPool)

	return true
}
