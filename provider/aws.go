package provider

import (
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
)

const awsOperationSuccessful = "Successful"

// AwsScalingProvider does stuff and things.
type AwsScalingProvider struct {
	AsgService *autoscaling.AutoScaling
}

// Name does stuff and things.
func (sp *AwsScalingProvider) Name() string {
	return "AwsScalingProvider"
}

// ScaleOut does stuff and things.
func (sp *AwsScalingProvider) ScaleOut(workerPool *structs.WorkerPool) (err error) {
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

	err = verifyScaleOut(workerPool.Name, sp.AsgService, int(newCapacity))
	if err != nil {
		return err
	}

	return nil
}

func verifyScaleOut(workerPool string,
	svc *autoscaling.AutoScaling, capacity int) error {
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
			return fmt.Errorf("timeout reached while waiting for the autoscaling"+
				"group operation for worker pool %v to complete successfully",
				workerPool)

		case <-ticker.C:
			asg, err := describeScalingGroup(workerPool, svc)
			if err != nil {
				logging.Error("provider/aws: an error occurred while attempting to "+
					"verify the autoscaling group operation for worker pool %v: %v",
					workerPool, err)
			} else {
				if len(asg.AutoScalingGroups[0].Instances) == int(capacity) {
					logging.Info("provider/aws: verified the autoscaling operation "+
						"for worker pool %v has completed successfully", workerPool)
					metrics.IncrCounter([]string{"cluster", "aws", "scale_out"}, 1)
					return nil
				}
			}
		}
	}
}

// SafetyCheck does stuff and things.
func (sp *AwsScalingProvider) SafetyCheck(workerPool *structs.WorkerPool,
	scalingDirection string) (safe bool) {
	// Retrieve ASG configuration so we can check min/max/desired counts
	// against the desired scaling action.
	asg, err := describeScalingGroup(workerPool.Name, sp.AsgService)
	if err != nil {
		logging.Error("provider/aws: unable to retrieve worker pool ASG "+
			"configuration to evaluate constraints: %v", err)
		return
	}

	// If we failed to get exactly one ASG, raise an error and halt processing.
	if len(asg.AutoScalingGroups) != 1 {
		logging.Error("provider/aws: the attempt to retrieve worker "+
			"pool ASG configuration failed to return a single result: results %v",
			len(asg.AutoScalingGroups))
		return
	}

	// Get the worker pool ASG min/max/desired constraints.
	desiredCap := *asg.AutoScalingGroups[0].DesiredCapacity
	maxSize := *asg.AutoScalingGroups[0].MaxSize
	minSize := *asg.AutoScalingGroups[0].MinSize

	if scalingDirection == structs.ScalingDirectionIn {
		// If scaling in would violate the ASG min count, fail the safety check.
		if desiredCap-1 < minSize {
			logging.Debug("provider/aws: cluster scale-in operation " +
				"would violate the worker pool ASG min count")
			return
		}
	}

	if scalingDirection == structs.ScalingDirectionOut {
		// If scaling out would violate the ASG max count, fail the safety check.
		if desiredCap+1 > maxSize {
			logging.Debug("provider/aws: cluster scale-out operation would " +
				"violate the worker pool ASG max count")
			return
		}
	}

	return true
}

// NewAwsScalingProvider does stuff and things.
func NewAwsScalingProvider(conf map[string]string) (structs.ScalingProvider, error) {
	region, ok := conf["replicator_region"]
	if !ok {
		return nil, fmt.Errorf("%v is required for the aws scaling provider",
			"replicator_region")
	}

	return &AwsScalingProvider{
		AsgService: newAwsAsgService(region),
	}, nil
}

func newAwsAsgService(region string) (Session *autoscaling.AutoScaling) {
	sess := session.Must(session.NewSession())
	svc := autoscaling.New(sess, &aws.Config{Region: aws.String(region)})
	return svc
}

// checkClusterScalingResult is used to poll the scaling activity and check for
// a successful completion.
func checkClusterScalingResult(activityID *string,
	svc *autoscaling.AutoScaling) error {

	// Setup our timeout and ticker value.
	ticker := time.NewTicker(time.Second * time.Duration(10))
	timeOut := time.Tick(time.Minute * 3)

	for {
		select {
		case <-timeOut:
			return fmt.Errorf("timeout %v reached on checking scaling activity "+
				"success", timeOut)
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

			if *resp.Activities[0].StatusCode == "Failed" ||
				*resp.Activities[0].StatusCode == "Cancelled" {

				return fmt.Errorf("scaling activity %v was unsuccessful ", activityID)
			}

			if *resp.Activities[0].StatusCode == awsOperationSuccessful {
				return nil
			}
		}
	}
}

// DescribeScalingGroup returns the AWS ASG information of the specified ASG.
func describeScalingGroup(asgName string,
	svc *autoscaling.AutoScaling) (
	asg *autoscaling.DescribeAutoScalingGroupsOutput, err error) {

	params := &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{
			aws.String(asgName),
		},
	}
	resp, err := svc.DescribeAutoScalingGroups(params)

	if err != nil {
		return resp, err
	}

	return resp, nil
}
