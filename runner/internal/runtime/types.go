// Package runtimemanager defines SecondBox Runner-owned launch and guest protocol values.
package runtimemanager

import (
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type RuntimeClass string

const RuntimeClassToolExecutor RuntimeClass = "tool_executor"

// StartupStage is one provider-neutral runtime startup milestone.
type StartupStage string

const (
	StartupStageNetworkReady    StartupStage = "network_ready"
	StartupStageComputeStarted  StartupStage = "compute_started"
	StartupStageGuestNegotiated StartupStage = "guest_negotiated"
)

// StartupMode is the provider-neutral startup policy an immutable Profile
// revision pinned. It selects the start path and never falls back: a
// snapshot-resume Sandbox that cold booted would come up without the
// identity-neutral template, the shared golden memory inode, or the one-time
// assignment bind that define the mode.
type StartupMode string

const (
	StartupModeColdBoot       StartupMode = "cold_boot"
	StartupModeSnapshotResume StartupMode = "snapshot_resume"
)

type StartOpts struct {
	Timezone                string
	CompartmentID           string
	WorkspaceAttachment     workspacestore.ComputeAttachment
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
	StartupProgress         func(StartupStage) error
	// TemplateMode boots an identity-neutral guest for snapshot-template
	// capture. Such a guest has no Sandbox identity to negotiate and receives no
	// runtime secrets; readiness is its control endpoint answering. It is used
	// only by the privileged template-build path and never serves a tenant.
	TemplateMode bool
	// StartupMode selects the start path. An assignment always states it,
	// because the control plane refuses a Profile revision that does not. It is
	// empty only for runner-internal launches that are not Profile-driven — the
	// tool VM and the template build — and those are cold by construction.
	StartupMode StartupMode
}

type SandboxRuntimePolicy struct {
	VCPUs             int
	MemoryMiB         int
	WorkspaceSizeMiB  int
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
