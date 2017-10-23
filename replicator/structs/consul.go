package structs

// The ConsulClient interface is used to provide common method signatures for
// interacting with the Consul API.
type ConsulClient interface {
	// AcquireLeadership attempts to acquire a Consul leadersip lock using the
	// provided session. If the lock is already taken this will return false in
	// a show that there is already a leader.
	AcquireLeadership(string, *string) bool

	// CreateSession creates a Consul session for use in the Leadership locking
	// process and will spawn off the renewing of the session in order to ensure
	// leadership can be maintained.
	CreateSession(int, chan struct{}) (string, error)

	// PersistState is responsible for persistently storing scaling
	// state information in the Consul Key/Value Store.
	PersistState(*ScalingState) error

	// ReadState attempts to read state tracking information from the Consul
	// Key/Value Store from the path provided.
	ReadState(*ScalingState, bool)

	// ResignLeadership attempts to remove the leadership lock upon shutdown of the
	// replicator daemon. If this is unsuccessful there is not too much we can do
	// therefore there is no return.
	ResignLeadership(string, string)

	// ServiceHealth
	ServiceHealth() map[string]map[string]string
}
