package contracts

import "time"

// FleetPoolObservation is an operator view. It never grants workspace access.
type FleetPoolObservation struct {
	PoolName   string              `json:"poolName"`
	ObservedAt time.Time           `json:"observedAt"`
	Demand     FleetResourceDemand `json:"demand"`
	Runners    []FleetRunner       `json:"runners"`
	Workspaces []FleetWorkspace    `json:"workspaces"`
	Truncated  bool                `json:"truncated"`
}

type FleetResourceDemand struct {
	Instances      int64      `json:"instances"`
	VCPUCount      int64      `json:"vcpuCount"`
	MemoryBytes    int64      `json:"memoryBytes"`
	WorkspaceBytes int64      `json:"workspaceBytes"`
	OldestWaitAt   *time.Time `json:"oldestWaitAt,omitempty"`
}

type FleetRunner struct {
	ID                     string              `json:"id"`
	Revision               int64               `json:"revision"`
	State                  string              `json:"state"`
	FleetState             string              `json:"fleetState"`
	Connected              bool                `json:"connected"`
	LastSeenAt             *time.Time          `json:"lastSeenAt,omitempty"`
	LastActivityAt         *time.Time          `json:"lastActivityAt,omitempty"`
	StorageAdmissionDenied bool                `json:"storageAdmissionDenied"`
	WorkspaceCapacityBytes int64               `json:"workspaceCapacityBytes"`
	ReservedWorkspaceBytes int64               `json:"reservedWorkspaceBytes"`
	Allocatable            PoolComputeCapacity `json:"allocatable"`
	Reserved               PoolComputeCapacity `json:"reserved"`
	WorkspaceCount         int64               `json:"workspaceCount"`
	SnapshotCount          int64               `json:"snapshotCount"`
	PendingMutations       int64               `json:"pendingMutations"`
	PendingCommands        int64               `json:"pendingCommands"`
	ActiveAssignments      int64               `json:"activeAssignments"`
}

type FleetWorkspace struct {
	SandboxID    string `json:"sandboxId"`
	TenantRef    string `json:"tenantRef"`
	SubjectRef   string `json:"subjectRef"`
	RunnerID     string `json:"runnerId"`
	Revision     int64  `json:"revision"`
	State        string `json:"state"`
	DesiredState string `json:"desiredState"`
	Mutation     string `json:"mutation"`
}
