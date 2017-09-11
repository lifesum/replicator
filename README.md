# Replicator

[![Build Status](https://travis-ci.org/elsevier-core-engineering/replicator.svg?branch=master)](https://travis-ci.org/elsevier-core-engineering/replicator) [![Go Report Card](https://goreportcard.com/badge/github.com/elsevier-core-engineering/replicator)](https://goreportcard.com/report/github.com/elsevier-core-engineering/replicator) [![Join the chat at https://gitter.im/els-core-engineering/replicator/Lobby](https://badges.gitter.im/els-core-engineering/replicator/Lobby.svg)](https://gitter.im/els-core-engineering/replicator?utm_source=badge&utm_medium=badge&utm_campaign=pr-badge&utm_content=badge) [![GoDoc](https://godoc.org/github.com/elsevier-core-engineering/replicator?status.svg)](https://godoc.org/github.com/elsevier-core-engineering/replicator)

Replicator is a fast and highly concurrent Go daemon that provides dynamic scaling of [Nomad](https://github.com/hashicorp/nomad) jobs and worker nodes.

- Replicator job scaling policies are configured as [meta parameters](https://www.nomadproject.io/docs/job-specification/meta.html) within the job specification. A job scaling policy allows scaling constraints to be defined per task-group. Currently supported scaling metrics are CPU and Memory; there are plans for additional metrics as well as different metric backends in the future. Details of configuring job scaling and other important information can be found on the Replicator [Job Scaling wiki page](https://github.com/elsevier-core-engineering/replicator/wiki/Job-Scaling).

- Replicator supports dynamic scaling of multiple, distinct cluster worker nodes in an AWS autoscaling group. Worker pool autoscaling is configured through Nomad client [meta parameters](https://www.nomadproject.io/docs/agent/configuration/client.html#meta). Details of configuring worker pool scaling and other important information can be found on the Replicator [Cluster Scaling wiki page](https://github.com/elsevier-core-engineering/replicator/wiki/Cluster-Scaling).

*At present, worker pool autoscaling is only supported on AWS, however, future support for GCE and Azure are planned using the Go factory/provider pattern.*

### Download

Pre-compiled releases for a number of platforms are available on the [GitHub release page](https://github.com/elsevier-core-engineering/replicator/releases). Docker images are also available from the elsce [Docker Hub page](https://hub.docker.com/r/elsce/replicator/).

## Running

Replicator can be run in a number of ways; the recommended way is as a Nomad service job either using the [Docker driver](https://www.nomadproject.io/docs/drivers/docker.html) or the [exec driver](https://www.nomadproject.io/docs/drivers/exec.html). There are example Nomad [job specification files](https://github.com/elsevier-core-engineering/replicator/tree/master/example-jobs) available as a starting point.

Replicator is fully capable in running as a distributed service; using [Consul sessions](https://www.consul.io/docs/internals/sessions.html) to provide leadership locking and exclusion. State is also written by Replicator to the Consul KV store, allowing Replicator failures to be handled quickly and efficiently.

### Permissions

Replicator requires permissions to Consul and the AWS (the only currently supported cloud provider) API in order to function correctly. The Consul ACL token is passed as a configuration parameter and AWS API access should be granted using an EC2 instance IAM role. Vault support is planned for the near future, which will change the way in which permissions are managed and provide a much more secure method of delivering these.

#### Consul ACL Token Permissions

If the Consul cluster being used is running ACLs; the following ACL policy will allow Replicator the required access to perform all functions based on its default configuration:

```hcl
key "" {
  policy = "read"
}
key "replicator/config" {
  policy = "write"
}
node "" {
  policy = "read"
}
node "" {
  policy = "write"
}
session "" {
  policy = "read"
}
session "" {
  policy = "write"
}
```

#### AWS IAM Permissions

Until Vault integration is added, the instance pool which is capable of running the Replicator daemon requires the following IAM permissions in order to perform worker pool scaling:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Authorize AutoScaling Actions",
      "Action": [
        "autoscaling:DescribeAutoScalingGroups",
        "autoscaling:DescribeAutoScalingInstances",
        "autoscaling:DescribeScalingActivities",
        "autoscaling:DetachInstances",
        "autoscaling:UpdateAutoScalingGroup"
      ],
      "Effect": "Allow",
      "Resource": "*"
    },
    {
      "Sid": "Authorize EC2 Actions",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeRegions",
        "ec2:TerminateInstances"
      ],
      "Effect": "Allow",
      "Resource": "*"
    }
  ]
}
```

### Commands

Replicator supports a number of commands (CLI) which allow for the easy control and manipulation of the replicator binary. In-depth documentation about each command can be found on the Replicator [commands wiki page](https://github.com/elsevier-core-engineering/replicator/wiki/Commands).

#### Command: `agent`

The `agent` command is the main entry point into Replicator. A subset of the available replicator agent configuration can optionally be passed in via CLI arguments and the configuration parameters passed via CLI flags will always take precedent over parameters specified in configuration files.

Detailed information regarding the available CLI flags can be found in the Replicator [agent command wiki page](https://github.com/elsevier-core-engineering/replicator/wiki/Agent-Command).

#### Command: `failsafe`

The `failsafe` command is used to toggle failsafe mode across the pool of Replicator agents. Failsafe mode prevents any Replicator agent from taking any scaling actions on the resource placed into failsafe mode.

Detailed information about failsafe mode operations and the available CLI options can be found in the Replicator [failsafe command wiki page](https://github.com/elsevier-core-engineering/replicator/wiki/Failsafe-Command).

#### Command: `init`

The `init` command creates example job scaling and worker pool scaling meta documents in the current directory. These files provide a starting example for configuring both scaling functionalities.

#### Command: `version`

The `version` command displays build information about the running binary, including the release version.

## Frequently Asked Questions

### When does Replicator adjust the size of the worker pool?

Replicator will dynamically scale-in the worker pool when:
- Resource utilization falls below the capacity required to run all current jobs while sustaining the configured node fault-tolerance. When calculating required capacity, Replicator includes scaling overhead required to increase the count of all running jobs by one.
- Before removing a worker node, Replicator simulates capacity thresholds if we were to remove a node. If the new required capacity is within 10% of the current utilization, Replicator will decline to remove a node to prevent thrashing.

Replicator will dynamically scale-out the worker pool when:
- Resource utilization exceeds or closely approaches the capacity required to run all current jobs while sustaining the configured node fault-tolerance. When calculating required capacity, Replicator includes scaling overhead required to increase the count of all running jobs by one.

### When does Replicator perform scaling actions against running jobs?

Replicator will dynamically scale a job when:
- A valid scaling policy for the job task-group is present within the job specification meta parameters and has the enabled flag set to true.
- A job specification can consist of multiple groups, each group can contain multiple tasks. Resource allocations and count are specified at the group level.
- Replicator evaluates scaling thresholds against the resource requirements defined within a group task. If any task within a group is found to violate the scaling thresholds, the group count will be adjusted accordingly.

## Contributing

Contributions to Replicator are very welcome! Please refer to our [contribution guide](https://github.com/elsevier-core-engineering/replicator/blob/master/.github/CONTRIBUTING.md) for details about hacking on Replicator.

For questions, please check out the [Elsevier Core Engineering/replicator](https://gitter.im/els-core-engineering/replicator) room in Gitter.
