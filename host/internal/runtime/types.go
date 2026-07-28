// Package runtimemanager defines Sandbox Host-owned launch and guest protocol values.
package runtimemanager

import (
	"time"

	"secondstack/sandbox-host/internal/runtimecontext"
)

type RuntimeClass string

const RuntimeClassToolExecutor RuntimeClass = "tool_executor"

type StartOpts struct {
	EnvironmentID            string
	InstanceID               string
	Generation               int64
	Timezone                 string
	CompartmentID            string
	ActorPrincipal           string
	RuntimeActorContext      runtimecontext.VerifiedActorContext
	ShapeFingerprint         string
	RuntimeClass             RuntimeClass
	Ephemeral                bool
	RuntimeContextProjection runtimecontext.Projection
	SandboxPolicy            *SandboxRuntimePolicy
}

type SandboxRuntimePolicy struct {
	VCPUs             int
	MemoryMiB         int
	WorkspaceSizeMiB  int
	ProcessLimit      int
	WorkspaceWritable bool
	SharedReadOnly    bool
}

type WorkspaceEntry struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size,omitempty"`
	MTime string `json:"mtime,omitempty"`
}

type RuntimeMetricsSnapshot struct {
	ConcurrentVMsByAgent  map[string]int
	ConcurrentVMsTotal    int
	PendingVMsByAgent     map[string]int
	PendingVMsTotal       int
	MaxConcurrentPerAgent int
	MaxConcurrentGlobal   int
	MemoryReservedMiB     int
	MemoryBudgetMiB       int
	GuestIPsInUse         int
	GuestIPCapacity       int
	WarmToolVMs           int
	ColdStartCount        int
	ColdStartP95          time.Duration
}
