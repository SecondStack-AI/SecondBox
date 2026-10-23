package contracts

import "time"

// PoolCapacityObservation reports aggregate compute capacity for the
// RunnerPools a caller can place Sandboxes into or already has Sandboxes in.
// It names no Runner and is a point-in-time reading, not a reservation.
type PoolCapacityObservation struct {
	Pools      []RunnerPoolCapacity `json:"pools"`
	ObservedAt time.Time            `json:"observedAt"`
}

// RunnerPoolCapacity aggregates one RunnerPool's placeable Runners.
type RunnerPoolCapacity struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// PlaceableRunnerCount counts Runners placement can currently choose:
	// ready, not draining, and connected.
	PlaceableRunnerCount int64 `json:"placeableRunnerCount"`
	// StorageAdmissionDeniedRunnerCount counts connected Runners whose latest
	// storage-pressure report denies new Workspaces and Instances.
	StorageAdmissionDeniedRunnerCount int64 `json:"storageAdmissionDeniedRunnerCount"`
	// Allocatable and Reserved sum the placeable Runners' advertised capacity
	// and current reservations.
	Allocatable PoolComputeCapacity `json:"allocatable"`
	Reserved    PoolComputeCapacity `json:"reserved"`
	// PendingStartCount counts Sandboxes, across every Tenant, wanted running
	// on a home Runner in this pool and not yet placed there.
	PendingStartCount int64 `json:"pendingStartCount"`
}

// PoolComputeCapacity is the compute a pool's placeable Runners advertise or
// hold. Operations are concurrent-operation slots, which placement reserves
// per Instance alongside vCPU and memory.
type PoolComputeCapacity struct {
	Instances   int64 `json:"instances"`
	VCPUCount   int64 `json:"vcpuCount"`
	MemoryBytes int64 `json:"memoryBytes"`
	Operations  int64 `json:"operations"`
}
