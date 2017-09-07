package provider

import (
	"fmt"
	"strings"

	"github.com/elsevier-core-engineering/replicator/logging"
	"github.com/elsevier-core-engineering/replicator/replicator/structs"
)

// BuiltinScalingProviders does stuff and things.
var BuiltinScalingProviders = map[string]ScalingProviderFactory{
	"aws": NewAwsScalingProvider,
}

// ScalingProviderFactory does stuff and things.
type ScalingProviderFactory func(
	conf map[string]string) (structs.ScalingProvider, error)

// NewScalingProvider does stuff and things.
func NewScalingProvider(conf map[string]string) (structs.ScalingProvider, error) {
	// Query configuration for scaling provider name.
	providerName, ok := conf["replicator_provider"]
	if !ok {
		return nil, fmt.Errorf("no scaling provider specified")
	}

	// Lookup the scaling provider factory function.
	providerFactory, ok := BuiltinScalingProviders[providerName]
	if !ok {
		// Build a list of all supported scaling providers.
		availableProviders := make([]string, len(BuiltinScalingProviders))
		for k := range BuiltinScalingProviders {
			availableProviders = append(availableProviders, k)
		}

		return nil, fmt.Errorf("unknown scaling provider %v, must be one of: %v",
			providerName, strings.Join(availableProviders, ", "))
	}

	// Instantiate the scaling provider.
	scalingProvider, err := providerFactory(conf)
	if err != nil {
		return nil, fmt.Errorf("an error occurred while setting up scaling "+
			"provider %v: %v", providerName, err)
	}

	logging.Debug("provider/scaling_provider: initialized scaling provider %v "+
		"for worker pool %v", providerName, conf["replicator_worker_pool"])

	return scalingProvider, nil
}
