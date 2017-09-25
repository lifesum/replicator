package structs

import (
	"reflect"
	"testing"
)

func TestStructs_Merge(t *testing.T) {
	c := &Config{
		Consul:                 "localhost:8500",
		ConsulKeyRoot:          "replicator/config",
		Nomad:                  "http://localhost:4646",
		LogLevel:               "INFO",
		ClusterScalingInterval: 10,
		JobScalingInterval:     10,
		Telemetry:              &Telemetry{},
		Notification:           &Notification{},
	}

	partialConfig := &Config{
		Consul:                 "consul.rocks.systems",
		ConsulToken:            "afb3bc3a-6acd-11e7-b70c-784f43a63381",
		Nomad:                  "http://nomad.rocks.systems:4646",
		LogLevel:               "ERROR",
		ClusterScalingInterval: 60,
		Telemetry: &Telemetry{
			StatsdAddress:     "8.8.8.8:8125",
			PrometheusMetrics: true,
		},
		Notification: &Notification{
			ClusterIdentifier:   "nomad-rocks",
			PagerDutyServiceKey: "onlyopsoncall",
		},
	}

	fullConfig := &Config{
		Consul:                 "consul.rocks.systems",
		ConsulKeyRoot:          "jobs/woz",
		ConsulToken:            "afb3bc3a-6acd-11e7-b70c-784f43a63381",
		Nomad:                  "http://nomad.rocks.systems:4646",
		LogLevel:               "ERROR",
		ClusterScalingDisable:  true,
		JobScalingDisable:      true,
		JobScalingInterval:     5,
		ClusterScalingInterval: 60,
		Telemetry: &Telemetry{
			StatsdAddress:     "8.8.8.8:8125",
			PrometheusMetrics: true,
		},
		Notification: &Notification{
			ClusterIdentifier:   "nomad-rocks",
			OpsGenieAPIKey:      "onlygenieoncall",
			PagerDutyServiceKey: "onlyopsoncall",
		},
	}

	partialExpected := &Config{
		Consul:                 "consul.rocks.systems",
		ConsulKeyRoot:          "replicator/config",
		ConsulToken:            "afb3bc3a-6acd-11e7-b70c-784f43a63381",
		Nomad:                  "http://nomad.rocks.systems:4646",
		LogLevel:               "ERROR",
		ClusterScalingInterval: 60,
		JobScalingInterval:     10,
		Telemetry: &Telemetry{
			StatsdAddress:     "8.8.8.8:8125",
			PrometheusMetrics: true,
		},
		Notification: &Notification{
			ClusterIdentifier:   "nomad-rocks",
			PagerDutyServiceKey: "onlyopsoncall",
		},
	}

	fullExpected := &Config{
		Consul:                 "consul.rocks.systems",
		ConsulKeyRoot:          "jobs/woz",
		ConsulToken:            "afb3bc3a-6acd-11e7-b70c-784f43a63381",
		Nomad:                  "http://nomad.rocks.systems:4646",
		LogLevel:               "ERROR",
		ClusterScalingDisable:  true,
		JobScalingDisable:      true,
		JobScalingInterval:     5,
		ClusterScalingInterval: 60,
		Telemetry: &Telemetry{
			StatsdAddress:     "8.8.8.8:8125",
			PrometheusMetrics: true,
		},
		Notification: &Notification{
			ClusterIdentifier:   "nomad-rocks",
			OpsGenieAPIKey:      "onlygenieoncall",
			PagerDutyServiceKey: "onlyopsoncall",
		},
	}

	partialResult := c.Merge(partialConfig)
	fullResult := c.Merge(fullConfig)

	if !reflect.DeepEqual(partialResult, partialExpected) {
		t.Fatalf("expected \n%#v\n\n, got \n\n%#v\n\n", partialExpected, partialResult)
	}
	if !reflect.DeepEqual(fullResult, fullExpected) {
		t.Fatalf("expected \n%#v\n\n, got \n\n%#v\n\n", fullExpected, fullResult)
	}
}
