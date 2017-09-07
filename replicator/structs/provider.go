package structs

// ScalingProvider does stuff and things.
type ScalingProvider interface {
	Name() string
	SafetyCheck(*WorkerPool, string) bool
	ScaleOut(*WorkerPool) error
}
