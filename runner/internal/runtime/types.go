// Package runtimemanager defines SecondBox Runner-owned launch and guest protocol values.
package runtimemanager

import (
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

type RuntimeClass string

const RuntimeClassToolExecutor RuntimeClass = "tool_executor"

type StartOpts struct {
	Timezone                string
	CompartmentID           string
	WorkspaceAttachmentID   string
	WorkspaceCheckpointPath string
	ShapeFingerprint        string
	SandboxGeneration       uint64
	GuestBuildID            string
	ImageManifestDigest     string
	ToolchainManifestDigest string
	MandatoryGuestFeatures  []string
	RuntimeClass            RuntimeClass
	Ephemeral               bool
	SandboxPolicy           *SandboxRuntimePolicy
	NetworkPolicy           *networkpolicy.CompiledPolicy
	RequestID               string
	OperationID             string
	LeaseID                 string
	AssignmentID            string
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
	ConcurrentVMsBySandbox  map[string]int
	ConcurrentVMsTotal      int
	PendingVMsBySandbox     map[string]int
	PendingVMsTotal         int
	MaxConcurrentPerSandbox int
	MaxConcurrentGlobal     int
	MemoryReservedMiB       int
	MemoryBudgetMiB         int
	GuestIPsInUse           int
	GuestIPCapacity         int
	WarmToolVMs             int
	ColdStartCount          int
	ColdStartP95            time.Duration
}
