package replicator

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elsevier-core-engineering/replicator/client"
	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/notifier"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
)

// newJobScalingPolicy returns a new JobScalingPolicies struct for Replicator
// to track and view Nomad job scaling.
func newJobScalingPolicy() *structs.JobScalingPolicies {

	return &structs.JobScalingPolicies{
		Policies: make(map[string][]*structs.GroupScalingPolicy),
		Lock:     sync.RWMutex{},
	}
}

func (r *Runner) asyncJobScaling(jobScalingPolicies *structs.JobScalingPolicies) {

	for job := range jobScalingPolicies.Policies {
		if r.config.NomadClient.IsJobInDeployment(job) {
			logging.Debug("core/job_scaling: job %s is in deployment, no scaling evaluation will be triggerd", job)
			continue
		}
		go r.jobScaling(job, jobScalingPolicies)
	}
}

func (r *Runner) jobScaling(jobName string, jobScalingPolicies *structs.JobScalingPolicies) {

	g := jobScalingPolicies.Policies[jobName]

	// Scaling a Cluster Jobs requires access to both Consul and Nomad therefore
	// we setup the clients here.
	nomadClient := r.config.NomadClient
	consulClient := r.config.ConsulClient

	// EvaluateJobScaling performs read/write to our map therefore we wrap it
	// in a read/write lock and remove this as soon as possible as the
	// remaining functions only need a read lock.
	jobScalingPolicies.Lock.Lock()
	err := nomadClient.EvaluateJobScaling(jobName, g)
	jobScalingPolicies.Lock.Unlock()

	// Horrible but required for jobs that have been purged as the policy
	// watcher will not get notified and such cannot remove the policy even
	// though the job doesn't exist. The string check is due to
	// github.com/hashicorp/nomad/issues/1849
	if err != nil && strings.Contains(err.Error(), "404") {
		client.RemoveJobScalingPolicy(jobName, jobScalingPolicies)
		return
	} else if err != nil {
		logging.Error("core/job_scaling: unable to perform job resource evaluation: %v", err)
		return
	}

	jobScalingPolicies.Lock.RLock()
	for _, group := range g {
		// Setup a failure message to pass to the failsafe check.
		message := &notifier.FailureMessage{
			AlertUID:     group.UID,
			ResourceID:   fmt.Sprintf("%s/%s", jobName, group.GroupName),
			ResourceType: JobType,
		}

		// Read or JobGroup state and check failsafe.
		s := &structs.ScalingState{
			ResourceName: group.GroupName,
			ResourceType: JobType,
			StatePath: r.config.ConsulKeyRoot + "/state/jobs/" + jobName +
				"/" + group.GroupName,
		}
		consulClient.ReadState(s, true)

		if !FailsafeCheck(s, r.config, 5, message) {
			logging.Error("core/job_scaling: job \"%v\" and group \"%v\" is in "+
				"failsafe mode", jobName, group.GroupName)
			continue
		}

		// Check the JobGroup scaling cooldown.
		cd := time.Duration(group.Cooldown) * time.Second

		if !s.LastScalingEvent.Before(time.Now().Add(-cd)) {
			logging.Debug("core/job_scaling: job \"%v\" and group \"%v\" has not reached scaling cooldown threshold of %s",
				jobName, group.GroupName, cd)
			continue
		}

		if group.ScaleDirection == client.ScalingDirectionOut || group.ScaleDirection == client.ScalingDirectionIn {
			if group.Enabled {
				logging.Debug("core/job_scaling: scaling for job \"%v\" and group \"%v\" is enabled; a "+
					"scaling operation (%v) will be requested", jobName, group.GroupName, group.ScaleDirection)

				// Submit the job and group for scaling.
				nomadClient.JobGroupScale(jobName, group, s)

			} else {
				logging.Debug("core/job_scaling: job scaling has been disabled; a "+
					"scaling operation (%v) would have been requested for \"%v\" "+
					"and group \"%v\"", group.ScaleDirection, jobName, group.GroupName)
			}
		}

		// Persist our state to Consul.
		consulClient.PersistState(s)

	}
	jobScalingPolicies.Lock.RUnlock()
}
