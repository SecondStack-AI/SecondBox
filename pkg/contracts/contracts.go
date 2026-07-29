// Package contracts defines the public and persisted SecondBox domain language.
package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

const (
	ProfileStateEnabled  = "enabled"
	ProfileStateDisabled = "disabled"

	RunnerPoolStateReady    = "ready"
	RunnerPoolStateDraining = "draining"
	RunnerPoolStateOffline  = "offline"

	SandboxStateCreating = "creating"
	SandboxStateStopped  = "stopped"
	SandboxStateStarting = "starting"
	SandboxStateReady    = "ready"
	SandboxStateDraining = "draining"
	SandboxStateStopping = "stopping"
	SandboxStateFailed   = "failed"
	SandboxStateDeleting = "deleting"
	SandboxStateDeleted  = "deleted"

	SandboxDesiredStateRunning = "running"
	SandboxDesiredStateStopped = "stopped"
	SandboxDesiredStateDeleted = "deleted"

	OperationStatePending   = "pending"
	OperationStateRunning   = "running"
	OperationStateSucceeded = "succeeded"
	OperationStateFailed    = "failed"
	OperationStateCancelled = "cancelled"

	LeaseStateActive   = "active"
	LeaseStateReleased = "released"
	LeaseStateExpired  = "expired"
	LeaseStateFenced   = "fenced"

	GuestLivenessUnknown  = "unknown"
	GuestLivenessStarting = "starting"
	GuestLivenessReady    = "ready"
	GuestLivenessLost     = "lost"
	GuestLivenessStopped  = "stopped"

	ActivitySessionStateActive = "active"
	ActivitySessionStateClosed = "closed"
	ActivitySessionKindExec    = "exec"
	ActivitySessionKindFile    = "file"
	ActivitySessionKindPTY     = "pty"
	ActivitySessionKindPort    = "port"

	PortSessionStateOpen    = "open"
	PortSessionStateClosing = "closing"
	PortSessionStateClosed  = "closed"
	PortSessionStateExpired = "expired"
	PortSessionStateFenced  = "fenced"

	ObjectStateStaging         = "staging"
	ObjectStateVerified        = "verified"
	ObjectStatePublished       = "published"
	ObjectStateIntegrityFailed = "integrity_failed"
	ObjectStateQuotaFailed     = "quota_failed"
	ObjectStateGarbagePending  = "garbage_pending"
	ObjectStateGarbageDeleting = "garbage_deleting"
	ObjectStateDeleted         = "deleted"

	TerminationReasonRequestedDrain     = "requested_drain"
	TerminationReasonRequestedStop      = "requested_stop"
	TerminationReasonIdleTimeout        = "idle_timeout"
	TerminationReasonMaximumDuration    = "maximum_duration"
	TerminationReasonGuestShutdown      = "guest_shutdown"
	TerminationReasonResourceExhaustion = "resource_exhaustion"
	TerminationReasonGuestAgentLost     = "guest_agent_lost"
	TerminationReasonRunnerLost         = "runner_lost"
	TerminationReasonStartupFailed      = "startup_failed"
	TerminationReasonFenced             = "fenced"
	TerminationReasonInternalFailure    = "internal_failure"
)

// Principal is the platform-asserted ownership scope for one request.
type Principal struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	TenantRef  string `json:"tenantRef,omitempty"`
	SubjectRef string `json:"subjectRef,omitempty"`
}

// Profile is the stable name and mutable head for immutable policy revisions.
type Profile struct {
	Name            string          `json:"name"`
	State           string          `json:"state"`
	CurrentRevision ProfileRevision `json:"currentRevision"`
	Revision        int64           `json:"revision"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

// ProfilePage is one bounded stable Profile traversal page.
type ProfilePage struct {
	Items      []Profile `json:"items"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// ProfileRevision is immutable policy selected by future Sandbox creation.
type ProfileRevision struct {
	ID        string              `json:"id"`
	Number    int64               `json:"number"`
	Spec      ProfileRevisionSpec `json:"spec"`
	CreatedAt time.Time           `json:"createdAt"`
}

// ProfileRevisionSpec resolves every execution, durability, and placement bound.
type ProfileRevisionSpec struct {
	Pool                  string          `json:"pool"`
	Architecture          string          `json:"architecture"`
	RuntimeBundleDigest   string          `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string          `json:"toolchainBundleDigest"`
	Resources             ResourcePolicy  `json:"resources"`
	Lifecycle             LifecyclePolicy `json:"lifecycle"`
	Retention             RetentionPolicy `json:"retention"`
	Execution             ExecutionPolicy `json:"execution"`
	Network               NetworkPolicy   `json:"network"`
	Ports                 []PortPolicy    `json:"ports"`
}

// ResourcePolicy contains per-Sandbox enforceable compute and workspace limits.
type ResourcePolicy struct {
	CPUMillis            int64 `json:"cpuMillis"`
	MemoryBytes          int64 `json:"memoryBytes"`
	WorkspaceBytes       int64 `json:"workspaceBytes"`
	ProcessLimit         int64 `json:"processLimit"`
	ConcurrentOperations int64 `json:"concurrentOperations"`
}

// LifecyclePolicy contains explicit Instance timing and initial-state policy.
type LifecyclePolicy struct {
	InitialState           string `json:"initialState"`
	DrainGraceSeconds      int64  `json:"drainGraceSeconds"`
	IdleSeconds            int64  `json:"idleSeconds"`
	MaximumDurationSeconds int64  `json:"maximumDurationSeconds"`
	LeaseSeconds           int64  `json:"leaseSeconds"`
}

// RetentionPolicy bounds local Snapshot count/lifetime and Artifact retention.
type RetentionPolicy struct {
	SnapshotLimit            int64 `json:"snapshotLimit"`
	SnapshotRetentionSeconds int64 `json:"snapshotRetentionSeconds"`
	ArtifactRetentionSeconds int64 `json:"artifactRetentionSeconds"`
}

// ExecutionPolicy bounds exec, transfer, terminal, and port-session resources.
type ExecutionPolicy struct {
	MaximumDeadlineMilliseconds int64 `json:"maximumDeadlineMilliseconds"`
	MaximumBufferedOutputBytes  int64 `json:"maximumBufferedOutputBytes"`
	StreamWindowBytes           int64 `json:"streamWindowBytes"`
	MaximumTransferBytes        int64 `json:"maximumTransferBytes"`
	TerminalDetachSeconds       int64 `json:"terminalDetachSeconds"`
}

// NetworkPolicy is an explicit deny-all or destination allow-list.
type NetworkPolicy struct {
	Mode         string               `json:"mode"`
	Destinations []NetworkDestination `json:"destinations"`
}

// NetworkDestination is one bounded protocol and host or CIDR allowance.
type NetworkDestination struct {
	Protocol string `json:"protocol"`
	Domain   string `json:"domain,omitempty"`
	CIDR     string `json:"cidr,omitempty"`
	Port     int64  `json:"port"`
}

// PortPolicy is one profile-approved guest port and session bound.
type PortPolicy struct {
	Name                  string `json:"name"`
	Port                  int64  `json:"port"`
	Protocol              string `json:"protocol"`
	MaximumSessions       int64  `json:"maximumSessions"`
	MaximumSessionSeconds int64  `json:"maximumSessionSeconds"`
}

// CreateProfileRequest creates a stable Profile and its first immutable revision.
type CreateProfileRequest struct {
	Name string              `json:"name"`
	Spec ProfileRevisionSpec `json:"spec"`
}

// ReviseProfileRequest appends an immutable Profile revision.
type ReviseProfileRequest struct {
	Spec ProfileRevisionSpec `json:"spec"`
}

// RunnerPool is the operator-owned placement and trust boundary.
type RunnerPool struct {
	Name             string           `json:"name"`
	State            string           `json:"state"`
	Architectures    []string         `json:"architectures"`
	Capabilities     []string         `json:"capabilities"`
	CapacityPolicy   map[string]int64 `json:"capacityPolicy"`
	ReadyRunnerCount int64            `json:"readyRunnerCount"`
	Revision         int64            `json:"revision"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

// RunnerPoolPage is one bounded stable administrative placement traversal page.
type RunnerPoolPage struct {
	Items      []RunnerPool `json:"items"`
	NextCursor *string      `json:"nextCursor,omitempty"`
}

// CreateRunnerPoolRequest declares one operator-owned runner placement boundary.
type CreateRunnerPoolRequest struct {
	Name           string           `json:"name"`
	State          string           `json:"state"`
	Architectures  []string         `json:"architectures"`
	Capabilities   []string         `json:"capabilities"`
	CapacityPolicy map[string]int64 `json:"capacityPolicy"`
}

// UpdateRunnerPoolRequest changes explicit runner admission policy under revision control.
type UpdateRunnerPoolRequest struct {
	State          *string           `json:"state,omitempty"`
	Architectures  *[]string         `json:"architectures,omitempty"`
	Capabilities   *[]string         `json:"capabilities,omitempty"`
	CapacityPolicy *map[string]int64 `json:"capacityPolicy,omitempty"`
}

// Runner is enrolled execution identity and fixed-capacity evidence.
type Runner struct {
	ID                          string           `json:"id"`
	PoolName                    string           `json:"poolName"`
	Name                        string           `json:"name"`
	State                       string           `json:"state"`
	CredentialState             string           `json:"credentialState"`
	Architectures               []string         `json:"architectures"`
	Capabilities                []string         `json:"capabilities"`
	Capacity                    map[string]int64 `json:"capacity"`
	ProtocolVersions            []string         `json:"protocolVersions"`
	SandboxStartSampleCount     int64            `json:"sandboxStartSampleCount"`
	SandboxStartP95Milliseconds int64            `json:"sandboxStartP95Milliseconds"`
	LastSeenAt                  *time.Time       `json:"lastSeenAt,omitempty"`
	Revision                    int64            `json:"revision"`
	CreatedAt                   time.Time        `json:"createdAt"`
	UpdatedAt                   time.Time        `json:"updatedAt"`
}

// RunnerPage is one bounded stable administrative Runner traversal page.
type RunnerPage struct {
	Items      []Runner `json:"items"`
	NextCursor *string  `json:"nextCursor,omitempty"`
}

// Assignment is internal writer authority for one Sandbox generation.
type Assignment struct {
	ID                 string
	SandboxID          string
	InstanceID         string
	RunnerID           string
	Generation         int64
	FencingToken       []byte
	State              string
	CapabilitySnapshot map[string]string
	ResolvedArtifacts  map[string]string
	ReleaseProof       map[string]string
	ClaimExpiresAt     time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Lease is bounded Project authority for one Sandbox generation.
type Lease struct {
	ID         string    `json:"id"`
	TenantRef  string    `json:"-"`
	SubjectRef string    `json:"-"`
	SandboxID  string    `json:"sandboxId"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Revision   int64     `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Snapshot is one immutable runner-local projection of a committed Workspace.
type Snapshot struct {
	ID                 string            `json:"id"`
	TenantRef          string            `json:"-"`
	SubjectRef         string            `json:"-"`
	SandboxID          string            `json:"sandboxId"`
	WorkspaceID        string            `json:"-"`
	SourceGeneration   int64             `json:"generation"`
	Name               string            `json:"name"`
	SizeBytes          int64             `json:"sizeBytes"`
	State              string            `json:"state"`
	Metadata           map[string]string `json:"metadata"`
	RetainUntil        *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt          time.Time         `json:"createdAt"`
	RetentionEndedAt   *time.Time        `json:"-"`
	GarbageCollectedAt *time.Time        `json:"-"`
}

// CreateSnapshotRequest names one immutable projection of the current committed disk state.
type CreateSnapshotRequest struct {
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata"`
}

// SnapshotPage is one bounded, newest-first retained Snapshot page.
type SnapshotPage struct {
	Items      []Snapshot `json:"items"`
	NextCursor *string    `json:"nextCursor,omitempty"`
}

// Artifact is immutable application exchange evidence.
type Artifact struct {
	ID                 string            `json:"id"`
	TenantRef          string            `json:"-"`
	SubjectRef         string            `json:"-"`
	SandboxID          string            `json:"sandboxId"`
	SourceGeneration   int64             `json:"generation"`
	Name               string            `json:"name"`
	MediaType          string            `json:"mediaType"`
	SizeBytes          int64             `json:"sizeBytes"`
	SHA256             string            `json:"sha256"`
	State              string            `json:"-"`
	Metadata           map[string]string `json:"metadata"`
	RetainUntil        time.Time         `json:"expiresAt"`
	CreatedAt          time.Time         `json:"createdAt"`
	PublishedAt        *time.Time        `json:"-"`
	GarbageCollectedAt *time.Time        `json:"-"`
}

// ArtifactPage is one bounded, newest-first immutable Artifact page.
type ArtifactPage struct {
	Items      []Artifact `json:"items"`
	NextCursor *string    `json:"nextCursor,omitempty"`
}

// CreatePortSessionRequest requests one pinned Profile port by name.
type CreatePortSessionRequest struct {
	Name            string `json:"name"`
	DurationSeconds int64  `json:"durationSeconds"`
}

// PortSession is an authenticated, expiring control-plane tunnel.
type PortSession struct {
	ID         string    `json:"id"`
	SandboxID  string    `json:"sandboxId"`
	Generation int64     `json:"generation"`
	Name       string    `json:"name"`
	Protocol   string    `json:"protocol"`
	Endpoint   string    `json:"endpoint"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// QuotaLimits bounds one subject's aggregate reservations.
type QuotaLimits struct {
	MaxSandboxes            int64 `json:"maxSandboxes"`
	MaxActiveInstances      int64 `json:"maxActiveInstances"`
	MaxCPUMillis            int64 `json:"maxCpuMillis"`
	MaxMemoryBytes          int64 `json:"maxMemoryBytes"`
	MaxArtifactBytes        int64 `json:"maxArtifactBytes"`
	MaxSnapshots            int64 `json:"maxSnapshots"`
	MaxArtifacts            int64 `json:"maxArtifacts"`
	MaxPortSessions         int64 `json:"maxPortSessions"`
	MaxConcurrentOperations int64 `json:"maxConcurrentOperations"`
}

// QuotaUsage projects one subject's aggregate persisted reservations.
type QuotaUsage struct {
	Sandboxes            int64 `json:"sandboxes"`
	ActiveInstances      int64 `json:"activeInstances"`
	CPUMillis            int64 `json:"cpuMillis"`
	MemoryBytes          int64 `json:"memoryBytes"`
	ArtifactBytes        int64 `json:"artifactBytes"`
	Snapshots            int64 `json:"snapshots"`
	Artifacts            int64 `json:"artifacts"`
	PortSessions         int64 `json:"portSessions"`
	ConcurrentOperations int64 `json:"concurrentOperations"`
}

// SubjectUsage reports one trusted caller subject's limits and current usage.
type SubjectUsage struct {
	TenantRef  string      `json:"tenantRef"`
	SubjectRef string      `json:"subjectRef"`
	Limits     QuotaLimits `json:"limits"`
	Usage      QuotaUsage  `json:"usage"`
}

// Workspace is public retained-workspace evidence without a provider location.
type Workspace struct {
	ID         string    `json:"id"`
	TenantRef  string    `json:"-"`
	SubjectRef string    `json:"-"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	SizeBytes  int64     `json:"sizeBytes"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Instance is replaceable compute evidence without runner or backend authority.
type Instance struct {
	ID                string     `json:"id"`
	SandboxID         string     `json:"sandboxId"`
	Generation        int64      `json:"generation"`
	State             string     `json:"state"`
	GuestLiveness     string     `json:"guestLiveness"`
	TerminationReason string     `json:"terminationReason,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	ReadyAt           *time.Time `json:"readyAt,omitempty"`
	GuestHeartbeatAt  *time.Time `json:"guestHeartbeatAt,omitempty"`
	StoppedAt         *time.Time `json:"stoppedAt,omitempty"`
}

// ActivitySession is useful generation-bound work that prevents idle reclamation.
type ActivitySession struct {
	ID             string     `json:"id"`
	TenantRef      string     `json:"tenantRef"`
	SubjectRef     string     `json:"subjectRef"`
	SandboxID      string     `json:"sandboxId"`
	Generation     int64      `json:"generation"`
	Kind           string     `json:"kind"`
	State          string     `json:"state"`
	LeaseID        string     `json:"leaseId,omitempty"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
}

// Sandbox is durable Project intent pinned to one immutable ProfileRevision.
type Sandbox struct {
	ID                string            `json:"id"`
	TenantRef         string            `json:"-"`
	SubjectRef        string            `json:"-"`
	Profile           string            `json:"profile"`
	ProfileRevisionID string            `json:"profileRevisionId"`
	State             string            `json:"state"`
	DesiredState      string            `json:"desiredState"`
	Generation        int64             `json:"generation"`
	Workspace         Workspace         `json:"workspace"`
	Instance          *Instance         `json:"instance,omitempty"`
	Metadata          map[string]string `json:"metadata"`
	LastActivityAt    *time.Time        `json:"lastActivityAt,omitempty"`
	Revision          int64             `json:"revision"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
	DeletedAt         *time.Time        `json:"deletedAt,omitempty"`
}

// SandboxPage is one bounded stable Project Sandbox traversal page.
type SandboxPage struct {
	Items      []Sandbox `json:"items"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// CreateSandboxRequest contains only caller-selected Profile and bounded metadata.
type CreateSandboxRequest struct {
	Profile  string            `json:"profile"`
	Metadata map[string]string `json:"metadata"`
}

// RestoreSnapshotRequest selects one Snapshot owned by the stopped Sandbox.
type RestoreSnapshotRequest struct {
	SnapshotID string `json:"snapshotId"`
}

// WaitSandboxRequest bounds lifecycle observation without renewing activity.
type WaitSandboxRequest struct {
	States               []string `json:"states"`
	DeadlineMilliseconds int64    `json:"deadlineMilliseconds"`
}

// AcquireLeaseRequest selects a duration within the pinned Profile policy.
type AcquireLeaseRequest struct {
	DurationSeconds int64 `json:"durationSeconds"`
}

// RenewLeaseRequest selects a new bounded duration for active Lease authority.
type RenewLeaseRequest struct {
	DurationSeconds int64 `json:"durationSeconds"`
}

// ExecCommand is the frozen shell-or-argv command union.
type ExecCommand struct {
	Mode       string
	Command    string
	Executable string
	Arguments  []string
}

func (command *ExecCommand) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	switch discriminator.Mode {
	case "shell":
		var value struct {
			Mode    string `json:"mode"`
			Command string `json:"command"`
		}
		if err := unmarshalClosedJSON(data, &value); err != nil {
			return err
		}
		*command = ExecCommand{Mode: value.Mode, Command: value.Command}
	case "argv":
		var value struct {
			Mode       string   `json:"mode"`
			Executable string   `json:"executable"`
			Arguments  []string `json:"arguments"`
		}
		if err := unmarshalClosedJSON(data, &value); err != nil {
			return err
		}
		*command = ExecCommand{
			Mode: value.Mode, Executable: value.Executable, Arguments: value.Arguments,
		}
	default:
		return errors.New("SecondBox Exec command mode must be shell or argv")
	}
	return nil
}

func (command ExecCommand) MarshalJSON() ([]byte, error) {
	switch command.Mode {
	case "shell":
		return json.Marshal(struct {
			Mode    string `json:"mode"`
			Command string `json:"command"`
		}{Mode: command.Mode, Command: command.Command})
	case "argv":
		return json.Marshal(struct {
			Mode       string   `json:"mode"`
			Executable string   `json:"executable"`
			Arguments  []string `json:"arguments"`
		}{Mode: command.Mode, Executable: command.Executable, Arguments: command.Arguments})
	default:
		return nil, errors.New("SecondBox Exec command mode must be shell or argv")
	}
}

func unmarshalClosedJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

// BufferedExecRequest is one bounded non-PTY execution.
type BufferedExecRequest struct {
	Command              ExecCommand       `json:"command"`
	Cwd                  *string           `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment"`
	StdinBase64          *string           `json:"stdinBase64,omitempty"`
	DeadlineMilliseconds int64             `json:"deadlineMilliseconds"`
	MaximumOutputBytes   int64             `json:"maximumOutputBytes"`
}

// StreamingExecRequest starts one non-PTY command controlled by WebSocket frames.
type StreamingExecRequest struct {
	Command              ExecCommand       `json:"command"`
	Cwd                  *string           `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment"`
	DeadlineMilliseconds int64             `json:"deadlineMilliseconds"`
	MaximumOutputBytes   int64             `json:"maximumOutputBytes"`
	WindowBytes          int64             `json:"windowBytes"`
}

// ExecStreamSession is the durable public streaming-exec negotiation result.
type ExecStreamSession struct {
	ID           string    `json:"id"`
	SandboxID    string    `json:"sandboxId"`
	Generation   int64     `json:"generation"`
	State        string    `json:"state"`
	WebsocketURL string    `json:"websocketUrl"`
	Subprotocol  string    `json:"subprotocol"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// CreateTerminalRequest starts one real PTY under the pinned Profile policy.
type CreateTerminalRequest struct {
	Command              ExecCommand       `json:"command"`
	Cwd                  *string           `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment"`
	Rows                 int64             `json:"rows"`
	Columns              int64             `json:"columns"`
	DeadlineMilliseconds int64             `json:"deadlineMilliseconds"`
	Detachable           bool              `json:"detachable"`
}

// TerminalSession is the durable public terminal negotiation result.
type TerminalSession struct {
	ID                 string    `json:"id"`
	SandboxID          string    `json:"sandboxId"`
	Generation         int64     `json:"generation"`
	State              string    `json:"state"`
	WebsocketURL       string    `json:"websocketUrl"`
	Subprotocol        string    `json:"subprotocol"`
	NextClientSequence int64     `json:"nextClientSequence"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

type TerminalInputFrame struct {
	Type       string `json:"type"`
	Sequence   int64  `json:"sequence"`
	DataBase64 string `json:"dataBase64"`
}

type TerminalResizeFrame struct {
	Type     string `json:"type"`
	Sequence int64  `json:"sequence"`
	Rows     int64  `json:"rows"`
	Columns  int64  `json:"columns"`
}

type TerminalOutputFrame struct {
	Type       string `json:"type"`
	Sequence   int64  `json:"sequence"`
	DataBase64 string `json:"dataBase64"`
}

type StreamInputFrame struct {
	Type       string `json:"type"`
	Sequence   int64  `json:"sequence"`
	DataBase64 string `json:"dataBase64"`
	EndOfInput *bool  `json:"endOfInput"`
}

type StreamCreditFrame struct {
	Type     string `json:"type"`
	Sequence int64  `json:"sequence"`
	Bytes    int64  `json:"bytes"`
}

type StreamCancelFrame struct {
	Type     string `json:"type"`
	Sequence int64  `json:"sequence"`
}

type StreamOutputFrame struct {
	Type       string `json:"type"`
	Sequence   int64  `json:"sequence"`
	Stream     string `json:"stream"`
	DataBase64 string `json:"dataBase64"`
}

type StreamOutcomeFrame struct {
	Type     string `json:"type"`
	Sequence int64  `json:"sequence"`
	Outcome  any    `json:"outcome"`
}

type ExecOutput struct {
	StdoutBase64 string `json:"stdoutBase64"`
	StderrBase64 string `json:"stderrBase64"`
}

type ExecExited struct {
	Kind                string     `json:"kind"`
	ExitCode            int32      `json:"exitCode"`
	Signal              *int32     `json:"signal,omitempty"`
	ElapsedMilliseconds int64      `json:"elapsedMilliseconds"`
	Output              ExecOutput `json:"output"`
}

type ExecSpawnFailed struct {
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type ExecDeadlineExceeded struct {
	Kind                string     `json:"kind"`
	ElapsedMilliseconds int64      `json:"elapsedMilliseconds"`
	Output              ExecOutput `json:"output"`
}

type ExecCancelled struct {
	Kind   string     `json:"kind"`
	Output ExecOutput `json:"output"`
}

type ExecOutputExhausted struct {
	Kind       string     `json:"kind"`
	LimitBytes int64      `json:"limitBytes"`
	Output     ExecOutput `json:"output"`
}

type ExecInfrastructureFailed struct {
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

type FileStat struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	SizeBytes  int64     `json:"sizeBytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type DirectoryListing struct {
	Path    string     `json:"path"`
	Entries []FileStat `json:"entries"`
}

type FileExistsResult struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type FileWriteResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type CreateDirectoryRequest struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
}

type RemovePathRequest struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
	Force     *bool  `json:"force"`
}

// SandboxInspection is persisted guest and useful-session evidence.
type SandboxInspection struct {
	SandboxID      string    `json:"sandboxId"`
	Generation     int64     `json:"generation"`
	GuestHealthy   bool      `json:"guestHealthy"`
	ActiveSessions int64     `json:"activeSessions"`
	ObservedAt     time.Time `json:"observedAt"`
}

// PingResult reports guest health without renewing useful activity.
type PingResult struct {
	SandboxID  string    `json:"sandboxId"`
	Generation int64     `json:"generation"`
	Healthy    bool      `json:"healthy"`
	ObservedAt time.Time `json:"observedAt"`
}

// TouchResult reports the durable useful-activity timestamp.
type TouchResult struct {
	SandboxID      string    `json:"sandboxId"`
	Generation     int64     `json:"generation"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

// Operation is durable asynchronous mutation evidence.
type Operation struct {
	ID              string            `json:"id"`
	TenantRef       string            `json:"-"`
	SubjectRef      string            `json:"-"`
	SandboxID       string            `json:"sandboxId"`
	Kind            string            `json:"kind"`
	State           string            `json:"state"`
	RequestID       string            `json:"requestId"`
	RequestMetadata map[string]string `json:"-"`
	Sandbox         *Sandbox          `json:"sandbox,omitempty"`
	Snapshot        *Snapshot         `json:"snapshot,omitempty"`
	Error           *Problem          `json:"error,omitempty"`
	CreatedAt       time.Time         `json:"createdAt"`
	StartedAt       *time.Time        `json:"startedAt,omitempty"`
	CompletedAt     *time.Time        `json:"completedAt,omitempty"`
	UpdatedAt       time.Time         `json:"updatedAt"`
}

// DurationPercentiles is one bounded duration distribution.
type DurationPercentiles struct {
	Count           int64  `json:"count"`
	P50Milliseconds *int64 `json:"p50Milliseconds,omitempty"`
	P95Milliseconds *int64 `json:"p95Milliseconds,omitempty"`
	P99Milliseconds *int64 `json:"p99Milliseconds,omitempty"`
}

// BootStageTiming attributes one provider-neutral Sandbox startup milestone.
type BootStageTiming struct {
	Stage                  string    `json:"stage"`
	ObservedAt             time.Time `json:"observedAt"`
	ReceivedAt             time.Time `json:"receivedAt"`
	ElapsedMilliseconds    int64     `json:"elapsedMilliseconds"`
	CumulativeMilliseconds int64     `json:"cumulativeMilliseconds"`
}

// BootTiming is one Sandbox generation's bounded startup attribution.
type BootTiming struct {
	Generation           int64             `json:"generation"`
	DurationMilliseconds int64             `json:"durationMilliseconds"`
	Completed            bool              `json:"completed"`
	Stages               []BootStageTiming `json:"stages"`
}

// OperationTiming separates durable queue and execution time.
type OperationTiming struct {
	OperationID           string       `json:"operationId"`
	SandboxID             string       `json:"sandboxId"`
	Kind                  string       `json:"kind"`
	State                 string       `json:"state"`
	CreatedAt             time.Time    `json:"createdAt"`
	StartedAt             *time.Time   `json:"startedAt,omitempty"`
	CompletedAt           *time.Time   `json:"completedAt,omitempty"`
	QueueMilliseconds     *int64       `json:"queueMilliseconds,omitempty"`
	ExecutionMilliseconds *int64       `json:"executionMilliseconds,omitempty"`
	TotalMilliseconds     *int64       `json:"totalMilliseconds,omitempty"`
	Boots                 []BootTiming `json:"boots"`
}

// ExecTiming reports one completed buffered or streaming execution.
type ExecTiming struct {
	SessionID           string    `json:"sessionId"`
	Mode                string    `json:"mode"`
	Outcome             string    `json:"outcome"`
	ElapsedMilliseconds int64     `json:"elapsedMilliseconds"`
	CreatedAt           time.Time `json:"createdAt"`
	CompletedAt         time.Time `json:"completedAt"`
}

// SandboxTiming is a bounded per-Sandbox timing history.
type SandboxTiming struct {
	SandboxID  string            `json:"sandboxId"`
	Operations []OperationTiming `json:"operations"`
	Execs      []ExecTiming      `json:"execs"`
}

// BootStageTimingSummary aggregates one provider-neutral startup stage.
type BootStageTimingSummary struct {
	Stage    string              `json:"stage"`
	Duration DurationPercentiles `json:"duration"`
}

// ExecTimingSummary aggregates one fixed Exec mode and outcome.
type ExecTimingSummary struct {
	Mode     string              `json:"mode"`
	Outcome  string              `json:"outcome"`
	Duration DurationPercentiles `json:"duration"`
}

// OperationTimingSummary aggregates one fixed Operation kind and terminal state.
type OperationTimingSummary struct {
	Kind      string              `json:"kind"`
	State     string              `json:"state"`
	Queue     DurationPercentiles `json:"queue"`
	Execution DurationPercentiles `json:"execution"`
	Total     DurationPercentiles `json:"total"`
}

// HTTPRouteTimingSummary aggregates one fixed route-template and status-class series.
type HTTPRouteTimingSummary struct {
	Route       string              `json:"route"`
	StatusClass string              `json:"statusClass"`
	Duration    DurationPercentiles `json:"duration"`
}

// DeploymentTimingSummary is one bounded current-deployment timing projection.
type DeploymentTimingSummary struct {
	WindowSeconds     int64                    `json:"windowSeconds"`
	ObservedAt        time.Time                `json:"observedAt"`
	Boot              DurationPercentiles      `json:"boot"`
	BootStages        []BootStageTimingSummary `json:"bootStages"`
	DominantBootStage *BootStageTimingSummary  `json:"dominantBootStage,omitempty"`
	Exec              DurationPercentiles      `json:"exec"`
	ExecSeries        []ExecTimingSummary      `json:"execSeries"`
	API               DurationPercentiles      `json:"api"`
	APISeries         []HTTPRouteTimingSummary `json:"apiSeries"`
	Operations        []OperationTimingSummary `json:"operations"`
}

// Problem is the stable typed failure envelope.
type Problem struct {
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Status    int             `json:"status"`
	Code      string          `json:"code"`
	RequestID string          `json:"requestId"`
	Retryable bool            `json:"retryable"`
	Details   []ProblemDetail `json:"details,omitempty"`
}

// ProblemDetail identifies one bounded invalid field.
type ProblemDetail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// AuditEvent is immutable security and administration evidence without secrets.
type AuditEvent struct {
	ID           string            `json:"id"`
	TenantRef    string            `json:"tenantRef,omitempty"`
	SubjectRef   string            `json:"subjectRef,omitempty"`
	ActorKind    string            `json:"actorKind"`
	ActorID      string            `json:"actorId"`
	Action       string            `json:"action"`
	ResourceKind string            `json:"resourceKind"`
	ResourceID   string            `json:"resourceId"`
	Outcome      string            `json:"outcome"`
	RequestID    string            `json:"requestId"`
	Details      map[string]string `json:"details"`
	CreatedAt    time.Time         `json:"createdAt"`
}

// MetricDurationHistogram is one cumulative duration histogram.
type MetricDurationHistogram struct {
	Count        uint64
	SumSeconds   float64
	BucketCounts []uint64
}

// OperationDurationMetric is one bounded Operation kind and terminal-state series.
type OperationDurationMetric struct {
	Kind          string
	TerminalState string
	Histogram     MetricDurationHistogram
}

// BootStageDurationMetric is one bounded startup-stage series.
type BootStageDurationMetric struct {
	Stage     string
	Histogram MetricDurationHistogram
}

// ExecDurationMetric is one bounded Exec mode and outcome series.
type ExecDurationMetric struct {
	Mode      string
	Outcome   string
	Histogram MetricDurationHistogram
}

// MetricsSnapshot contains only fixed-cardinality state, outcome, and timing signals.
type MetricsSnapshot struct {
	SandboxStates      map[string]int64
	OperationStates    map[string]int64
	OperationDurations []OperationDurationMetric
	BootDuration       MetricDurationHistogram
	BootStageDurations []BootStageDurationMetric
	ExecDurations      []ExecDurationMetric
}
