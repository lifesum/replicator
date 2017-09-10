package provider

import (
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/elsevier-core-engineering/replicator/helper"
	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
)

const (
	awsOperationFailed     = "Failed"
	awsOperationCancelled  = "Cancelled"
	awsOperationSuccessful = "Successful"
)

// AwsScalingProvider implements the ScalingProvider interface and provides
// a provider that is capable of performing scaling operations against
// Nomad worker pools running on AWS autoscaling groups.
//
// The provider performs verification of each action it takes and provides
// automatic retry for scale-out operations that fail.
type AwsScalingProvider struct {
	AsgService *autoscaling.AutoScaling
}

// NewAwsScalingProvider is a factory function that generates a new instance
// of the AwsScalingProvider.
func NewAwsScalingProvider(conf map[string]string) (structs.ScalingProvider, error) {
	region, ok := conf["replicator_region"]
	if !ok {
		return nil, fmt.Errorf("replicator_region is required for the aws " +
			"scaling provider")
	}

	return &AwsScalingProvider{
		AsgService: newAwsAsgService(region),
	}, nil
}

// newAwsAsgService returns a session object for the AWS autoscaling service.
func newAwsAsgService(region string) (Session *autoscaling.AutoScaling) {
	sess := session.Must(session.NewSession())
	svc := autoscaling.New(sess, &aws.Config{Region: aws.String(region)})
	return svc
}

// Scale is the entry point method for performing scaling operations with
// the provider.
func (sp *AwsScalingProvider) Scale(workerPool *structs.WorkerPool,
	config *structs.Config, nodeRegistry *structs.NodeRegistry) (err error) {

	switch workerPool.State.ScalingDirection {

	case structs.ScalingDirectionOut:
		// Initiate autoscaling group scaling operation.
		err = sp.scaleOut(workerPool)
		if err != nil {
			return err
		}

		// Initiate verification of the scaling operation to include retry
		// attempts if any failures are detected.
		if ok := sp.verifyScaledNode(workerPool, config, nodeRegistry); !ok {
			return fmt.Errorf("an error occurred while attempting to verify the "+
				"scaling operation, the provider automatically retried the "+
				"scaling operation up to the maximum retry threshold count %v",
				workerPool.RetryThreshold)
		}

	case structs.ScalingDirectionIn:
		// Initiate autoscaling group scaling operation.
		err = sp.scaleIn(workerPool, config)
		if err != nil {
			return err
		}
	}

	return nil
}

// scaleOut is the internal method used to initiate a scale out operation
// against a worker pool autoscaling group.
func (sp *AwsScalingProvider) scaleOut(workerPool *structs.WorkerPool) error {
	// Get the current autoscaling group configuration.
	asg, err := describeScalingGroup(workerPool.Name, sp.AsgService)
	if err != nil {
		return err
	}

	// Increment the desired capacity and copy the existing termination policies
	// and availability zones.
	availabilityZones := asg.AutoScalingGroups[0].AvailabilityZones
	terminationPolicies := asg.AutoScalingGroups[0].TerminationPolicies
	newCapacity := *asg.AutoScalingGroups[0].DesiredCapacity + int64(1)

	// Setup autoscaling group input parameters.
	params := &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(workerPool.Name),
		AvailabilityZones:    availabilityZones,
		DesiredCapacity:      aws.Int64(newCapacity),
		TerminationPolicies:  terminationPolicies,
	}

	logging.Info("provider/aws: initiating cluster scale-out operation for "+
		"worker pool %v", workerPool.Name)

	// Send autoscaling group API request to increase the desired count.
	_, err = sp.AsgService.UpdateAutoScalingGroup(params)
	if err != nil {
		return err
	}

	err = verifyAsgUpdate(workerPool.Name, newCapacity, sp.AsgService)
	if err != nil {
		return err
	}

	return nil
}

// scaleIn is the internal method used to initiate a scale in operation
// against a worker pool autoscaling group.
func (sp *AwsScalingProvider) scaleIn(workerPool *structs.WorkerPool,
	config *structs.Config) error {

	// If no nodes have been registered as eligible for targeted scaling
	// operations, throw an error and exit.
	if len(workerPool.State.EligibleNodes) == 0 {
		return fmt.Errorf("provider/aws: no nodes are marked as eligible for " +
			"scaling action, unable to detach and terminate")
	}

	// Setup client for Consul.
	consulClient := config.ConsulClient

	// Pop a target node from the list of eligible nodes.
	targetNode := workerPool.State.EligibleNodes[0]
	workerPool.State.EligibleNodes =
		workerPool.State.EligibleNodes[:len(workerPool.State.EligibleNodes)-1]

	// Translate the node IP address to the EC2 instance ID.
	instanceID := translateIptoID(targetNode, workerPool.Region)

	// Setup parameters for the AWS API call to detach the target node
	// from the worker pool autoscaling group.
	params := &autoscaling.DetachInstancesInput{
		AutoScalingGroupName:           aws.String(workerPool.Name),
		ShouldDecrementDesiredCapacity: aws.Bool(true),
		InstanceIds: []*string{
			aws.String(instanceID),
		},
	}

	// Detach the target node from the worker pool autoscaling group.
	resp, err := sp.AsgService.DetachInstances(params)
	if err != nil {
		return err
	}

	// Monitor the scaling activity result.
	if *resp.Activities[0].StatusCode != awsOperationSuccessful {
		err = checkClusterScalingResult(resp.Activities[0].ActivityId, sp.AsgService)
		if err != nil {
			return err
		}
	}

	// Once the node has been detached from the worker pool autoscaling group,
	// terminate the instance.
	err = terminateInstance(instanceID, workerPool.Region)
	if err != nil {
		return fmt.Errorf("an error occurred while attempting to terminate "+
			"instance %v from worker pool %v", instanceID, workerPool.Name)
	}

	// Record a successful scaling event and reset the failure count.
	workerPool.State.LastScalingEvent = time.Now()
	workerPool.State.FailureCount = 0

	// Attempt to update state tracking information in Consul.
	if err = consulClient.PersistState(workerPool.State); err != nil {
		logging.Error("provider/aws: %v", err)
	}

	metrics.IncrCounter([]string{"cluster", "aws", "scale_in"}, 1)

	return nil
}

func (sp *AwsScalingProvider) verifyScaledNode(workerPool *structs.WorkerPool,
	config *structs.Config, nodeRegistry *structs.NodeRegistry) (ok bool) {

	// Setup reference to Consul client.
	consulClient := config.ConsulClient

	for workerPool.State.FailureCount <= workerPool.RetryThreshold {
		if workerPool.State.FailureCount > 0 {
			logging.Info("provider/aws: attempting to launch a new node in worker "+
				"pool %v, previous node failures: %v", workerPool.Name,
				workerPool.State.FailureCount)
		}

		// Identify the most recently launched instance in the worker pool.
		instanceIP, err := getMostRecentInstance(workerPool.Name, workerPool.Region)
		if err != nil {
			logging.Error("provider/aws: failed to identify the most recently "+
				"launched instance in worker pool %v: %v", workerPool.Name, err)

			// Increment the failure count and persist the state object.
			workerPool.State.FailureCount++
			if err = consulClient.PersistState(workerPool.State); err != nil {
				logging.Error("provider/aws: %v", err)
			}
			continue
		}

		// Verify the most recently launched instance has completed bootstrapping
		// and successfully joined the worker pool.
		if ok := helper.FindNodeByAddress(nodeRegistry, workerPool.Name,
			instanceIP); ok {
			// Reset node failure count once we have verified the new node is healthy.
			workerPool.State.FailureCount = 0

			// Update the last scaling event timestamp.
			workerPool.State.LastScalingEvent = time.Now()

			// Persist the state tracking object to Consul.
			if err = consulClient.PersistState(workerPool.State); err != nil {
				logging.Error("provider/aws: %v", err)
			}

			return true
		}

		// The identified node did not successfully join the worker pool in a
		// timely fashion, so we register a failure and start cleanup procedures.
		workerPool.State.FailureCount++

		// Persist the state tracking object to Consul.
		if err = consulClient.PersistState(workerPool.State); err != nil {
			logging.Error("provider/aws: %v", err)
		}

		logging.Error("provider/aws: new node %v failed to successfully "+
			"join worker pool %v, incrementing node failure count to %v and "+
			"taking cleanup actions", instanceIP, workerPool.Name,
			workerPool.State.FailureCount)

		metrics.IncrCounter([]string{"cluster", "scale_out_failed"}, 1)

		// Perform post-failure cleanup tasks.
		if err = sp.failedEventCleanup(instanceIP, workerPool); err != nil {
			logging.Error("provider/aws: %v", err)
		}
	}

	return false
}

// failedEventCleanup is a janitorial method used to perform cleanup actions
// after a failed scaling event is detected. The node is detached and
// terminated unless the retry threshold has been reached, in that case the
// node is left in a detached state for troubleshooting.
func (sp *AwsScalingProvider) failedEventCleanup(workerNode string,
	workerPool *structs.WorkerPool) (err error) {

	// Translate the IP address of the most recently launched node to
	// EC2 instance ID so the node can be terminated or detached.
	instanceID := translateIptoID(workerNode, workerPool.Region)

	// If the retry threshold defined for the worker pool has been reached, we
	// will detach the instance from the autoscaling group and decrement the
	// autoscaling group desired count.
	if workerPool.State.FailureCount == workerPool.RetryThreshold {
		err := detachInstance(workerPool.Name, instanceID, sp.AsgService)
		if err != nil {
			return fmt.Errorf("an error occurred while attempting to detach the "+
				"failed instance %v from worker pool %v: %v", instanceID,
				workerPool.Name, err)
		}
		return nil
	}

	// Attempt to terminate the most recently launched instance to allow the
	// autoscaling group a chance to launch a new one.
	if err := terminateInstance(instanceID, workerPool.Region); err != nil {
		logging.Error("provider/aws: an error occurred while attempting to "+
			"terminate instance %v from worker pool %v: %v", instanceID,
			workerPool.Name, err)
		return err
	}

	return nil
}

// terminateInstance terminates a specified EC2 instance and confirms success.
func terminateInstance(instanceID, region string) error {
	// Setup the session and the EC2 service link to use for this operation.
	sess := session.Must(session.NewSession())
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	// Setup parameters for the termination API request.
	tparams := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{
			aws.String(instanceID),
		},
		DryRun: aws.Bool(false),
	}

	// Call the API to terminate the instance.
	logging.Info("provider/aws: terminating instance %s", instanceID)
	if _, err := svc.TerminateInstances(tparams); err != nil {
		return err
	}

	// Setup our timeout and ticker value.
	ticker := time.NewTicker(time.Second * time.Duration(10))
	timeOut := time.Tick(time.Minute * 3)

	logging.Info("provider/aws: confirming successful termination of "+
		"instance %v", instanceID)

	for {
		select {
		case <-timeOut:
			return fmt.Errorf("timeout reached while attempting to confirm "+
				"the termination of instance %v", instanceID)

		case <-ticker.C:
			// Setup the parameters to call the instance status endpoint so that we
			// can discover the status of the terminating instance.
			params := &ec2.DescribeInstanceStatusInput{
				DryRun:              aws.Bool(false),
				IncludeAllInstances: aws.Bool(true),
				InstanceIds: []*string{
					aws.String(instanceID),
				},
			}

			resp, err := svc.DescribeInstanceStatus(params)
			if err != nil {
				return err
			}

			if *resp.InstanceStatuses[0].InstanceState.Name == "terminated" {
				logging.Info("provider/aws: successfully confirmed the termination "+
					"of instance %v", instanceID)

				metrics.IncrCounter([]string{"cluster", "aws",
					"instance_terminations"}, 1)

				return nil
			}
		}
	}
}

// verifyAsgUpdate validates that a scale out operation against a worker
// pool autoscaling group has completed successfully.
func verifyAsgUpdate(workerPool string, capacity int64,
	svc *autoscaling.AutoScaling) error {

	// Setup a ticker to poll the autoscaling group and report when an instance
	// has been successfully launched.
	ticker := time.NewTicker(time.Millisecond * 500)
	timeout := time.Tick(time.Minute * 3)

	logging.Info("provider/aws: attempting to verify the autoscaling group "+
		"scaling operation for worker pool %v has completed successfully",
		workerPool)

	for {
		select {

		case <-timeout:
			return fmt.Errorf("timeout reached while attempting to verify the "+
				"autoscaling group scaling operation for worker pool %v completed "+
				"successfully", workerPool)

		case <-ticker.C:
			asg, err := describeScalingGroup(workerPool, svc)
			if err != nil {
				logging.Error("provider/aws: an error occurred while attempting to "+
					"verify the autoscaling group operation for worker pool %v: %v",
					workerPool, err)

			} else {
				if int64(len(asg.AutoScalingGroups[0].Instances)) == capacity {
					logging.Info("provider/aws: verified the autoscaling operation "+
						"for worker pool %v has completed successfully", workerPool)

					metrics.IncrCounter([]string{"cluster", "aws", "scale_out"}, 1)
					return nil
				}
			}
		}
	}
}

// SafetyCheck is an exported method that provides provider specific safety
// checks that will be used by core runner to determine if a scaling operation
// can be safely initiated.
func (sp *AwsScalingProvider) SafetyCheck(workerPool *structs.WorkerPool) bool {
	// Retrieve ASG configuration so we can check min/max/desired counts
	// against the desired scaling action.
	asg, err := describeScalingGroup(workerPool.Name, sp.AsgService)
	if err != nil {
		logging.Error("provider/aws: unable to retrieve worker pool autoscaling "+
			"group configuration to evaluate constraints: %v", err)
		return false
	}

	// Get the worker pool ASG min/max/desired constraints.
	desiredCap := *asg.AutoScalingGroups[0].DesiredCapacity
	maxSize := *asg.AutoScalingGroups[0].MaxSize
	minSize := *asg.AutoScalingGroups[0].MinSize

	if int64(len(workerPool.Nodes)) != desiredCap {
		logging.Debug("provider/aws: the number of healthy nodes %v registered "+
			"with worker pool %v does not match the current desired capacity of "+
			"the autoscaling group %v, no scaling action should be permitted",
			len(workerPool.Nodes), workerPool.Name, desiredCap)
		return false
	}

	if workerPool.State.ScalingDirection == structs.ScalingDirectionIn {
		// If scaling in would violate the ASG min count, fail the safety check.
		if desiredCap-1 < minSize {
			logging.Debug("provider/aws: cluster scale-in operation "+
				"would violate the worker pool ASG min count: %v", minSize)
			return false
		}
	}

	if workerPool.State.ScalingDirection == structs.ScalingDirectionOut {
		// If scaling out would violate the ASG max count, fail the safety check.
		if desiredCap+1 > maxSize {
			logging.Debug("provider/aws: cluster scale-out operation would "+
				"violate the worker pool ASG max count: %v", maxSize)
			return false
		}
	}

	return true
}

// checkClusterScalingResult is used to poll a worker pool autoscaling group
// to monitor a specified scaling activity for successful completion.
func checkClusterScalingResult(activityID *string,
	svc *autoscaling.AutoScaling) error {

	// Setup our timeout and ticker value.
	ticker := time.NewTicker(time.Second * time.Duration(10))
	timeOut := time.Tick(time.Minute * 3)

	for {
		select {
		case <-timeOut:
			return fmt.Errorf("timeout reached while attempting to verify scaling "+
				"activity %v completed successfully", activityID)

		case <-ticker.C:
			params := &autoscaling.DescribeScalingActivitiesInput{
				ActivityIds: []*string{
					aws.String(*activityID),
				},
			}

			// Check the status of the scaling activity.
			resp, err := svc.DescribeScalingActivities(params)
			if err != nil {
				return err
			}

			if *resp.Activities[0].StatusCode == awsOperationFailed ||
				*resp.Activities[0].StatusCode == awsOperationCancelled {

				return fmt.Errorf("scaling activity %v failed to complete "+
					"successfully", activityID)
			}

			if *resp.Activities[0].StatusCode == awsOperationSuccessful {
				return nil
			}
		}
	}
}

// describeScalingGroup returns the current configuration of a worker pool
// autoscaling group.
func describeScalingGroup(asgName string,
	svc *autoscaling.AutoScaling) (
	asg *autoscaling.DescribeAutoScalingGroupsOutput, err error) {

	params := &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{
			aws.String(asgName),
		},
	}
	resp, err := svc.DescribeAutoScalingGroups(params)

	// If we failed to get exactly one ASG, raise an error.
	if len(resp.AutoScalingGroups) != 1 {
		err = fmt.Errorf("the attempt to retrieve the current worker pool "+
			"autoscaling group configuration expected exaclty one result got %v",
			len(asg.AutoScalingGroups))
	}

	return resp, err
}

// getMostRecentInstance monitors a worker pool autoscaling group after a
// scale out operation to identify the newly launched instance.
func getMostRecentInstance(asg, region string) (node string, err error) {
	// Setup struct to track most recent instance information
	instanceTracking := &structs.MostRecentNode{}

	// Calculate instance launch threshold.
	launchThreshold := time.Now().Add(-90 * time.Second)

	// Setup a ticker to poll the autoscaling group for a recently
	// launched instance and retry up to a specified timeout.
	ticker := time.NewTicker(time.Second * 10)
	timeout := time.Tick(time.Minute * 5)

	// Setup AWS EC2 API Session
	sess := session.Must(session.NewSession())
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	// Setup query parameters to find instances that are associated with the
	// specified autoscaling group and are in a running or pending state.
	params := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:aws:autoscaling:groupName"),
				Values: []*string{
					aws.String(asg),
				},
			},
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
					aws.String("pending"),
				},
			},
		},
	}

	logging.Info("provider/aws: determining most recently launched instance "+
		"in autoscaling group %v", asg)

	for {
		select {
		case <-timeout:
			err = fmt.Errorf("provider/aws: timeout reached while attempting to "+
				"determine the most recently launched instance in autoscaling "+
				"group %v", asg)
			logging.Error("%v", err)
			return
		case <-ticker.C:
			// Query the AWS API for worker nodes.
			resp, err := svc.DescribeInstances(params)
			if err != nil {
				logging.Error("provider/aws: an error occurred while attempting to "+
					"retrieve EC2 instance details: %v", err)
				continue
			}

			// If our query returns no instances, raise an error.
			if len(resp.Reservations) == 0 {
				logging.Error("provider/aws: failed to retrieve a list of EC2 "+
					"instances in autoscaling group %v", asg)
				continue
			}

			// Iterate over and determine the most recent instance.
			for _, res := range resp.Reservations {
				for _, instance := range res.Instances {
					logging.Debug("provider/aws: discovered worker node %v which was "+
						"launched on %v", *instance.InstanceId, instance.LaunchTime)

					if instance.LaunchTime.After(instanceTracking.MostRecentLaunch) {
						instanceTracking.MostRecentLaunch = *instance.LaunchTime
						instanceTracking.InstanceIP = *instance.PrivateIpAddress
						instanceTracking.InstanceID = *instance.InstanceId
					}
				}
			}

			// If the most recent node was launched after our launch threshold
			// we've found what we were looking for otherwise, pause and recheck.
			if instanceTracking.MostRecentLaunch.After(launchThreshold) {
				logging.Info("provider/aws: instance %v is the newest instance in "+
					"autoscaling group %v and was launched after the threshold %v",
					instanceTracking.InstanceID, asg, launchThreshold)
				return instanceTracking.InstanceIP, nil
			}

			logging.Debug("provider/aws: instance %v is the most recent instance "+
				"launched in autoscaling group %v but its launch time %v is not "+
				"after the launch threshold %v", instanceTracking.InstanceID, asg,
				instanceTracking.MostRecentLaunch, launchThreshold)
		}

	}
}

// translateIptoID translates the IP address of a node to the EC2 instance ID.
func translateIptoID(ip, region string) (id string) {
	sess := session.Must(session.NewSession())
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	params := &ec2.DescribeInstancesInput{
		DryRun: aws.Bool(false),
		Filters: []*ec2.Filter{
			{
				Name: aws.String("private-ip-address"),
				Values: []*string{
					aws.String(ip),
				},
			},
		},
	}
	resp, err := svc.DescribeInstances(params)

	if err != nil {
		logging.Error("provider/aws: unable to convert node IP to AWS EC2 "+
			"instance ID: %v", err)
		return
	}

	return *resp.Reservations[0].Instances[0].InstanceId
}

// detachInstance is used to detach a specified node from a worker pool
// autoscaling group and automatically decrements the desired count.
func detachInstance(asg, instanceID string,
	svc *autoscaling.AutoScaling) (err error) {

	// Setup the request parameters for the DetachInstances API call.
	params := &autoscaling.DetachInstancesInput{
		AutoScalingGroupName:           aws.String(asg),
		ShouldDecrementDesiredCapacity: aws.Bool(true),
		InstanceIds: []*string{
			aws.String(instanceID),
		},
	}

	logging.Info("provider/aws: attempting to detach instance %v from "+
		"autoscaling group %v", instanceID, asg)

	// Detach specified instance from the ASG. Note, this also strips the
	// aws:autoscaling:groupName tag from the instance so it will be hidden
	// from the getMostRecentInstance method.
	resp, err := svc.DetachInstances(params)
	if err != nil {
		return
	}

	// If the immediate API response does not indicate the detachment has
	// completed successfully, call the checkClusterScalingResult() method which
	// will poll the ASG until it can verify the status.
	if *resp.Activities[0].StatusCode != awsOperationSuccessful {
		err = checkClusterScalingResult(resp.Activities[0].ActivityId, svc)
	}
	if err == nil {
		logging.Info("provider/aws: successfully detached instance %v from "+
			"autoscaling group %v", instanceID, asg)
	}

	return
}
