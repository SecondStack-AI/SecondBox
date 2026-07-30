package secondboxclient

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type (
	OpaqueID            = string
	ProfileName         = string
	WorkspacePath       = string
	Metadata            = map[string]string
	StringMap           = map[string]string
	SandboxState        = string
	OperationState      = string
	ServiceAccountScope = string
	ProblemCode         = string

	Profile                 = contracts.Profile
	ProfileRevisionSpec     = contracts.ProfileRevisionSpec
	ResourcePolicy          = contracts.ResourcePolicy
	LifecyclePolicy         = contracts.LifecyclePolicy
	RetentionPolicy         = contracts.RetentionPolicy
	ExecutionPolicy         = contracts.ExecutionPolicy
	NetworkPolicy           = contracts.NetworkPolicy
	NetworkDestination      = contracts.NetworkDestination
	PortPolicy              = contracts.PortPolicy
	CreateProfileRequest    = contracts.CreateProfileRequest
	RunnerPool              = contracts.RunnerPool
	RunnerPoolPage          = contracts.RunnerPoolPage
	CreateRunnerPoolRequest = contracts.CreateRunnerPoolRequest
	Runner                  = contracts.Runner
	RunnerPage              = contracts.RunnerPage
	Sandbox                 = contracts.Sandbox
	CreateSandboxRequest    = contracts.CreateSandboxRequest
	RestoreSnapshotRequest  = contracts.RestoreSnapshotRequest
	CreateSnapshotRequest   = contracts.CreateSnapshotRequest
	Snapshot                = contracts.Snapshot
	SnapshotPage            = contracts.SnapshotPage
	Operation               = contracts.Operation
	Lease                   = contracts.Lease
	AcquireLeaseRequest     = contracts.AcquireLeaseRequest
	RenewLeaseRequest       = contracts.RenewLeaseRequest
	DurationPercentiles     = contracts.DurationPercentiles
	BootStageTiming         = contracts.BootStageTiming
	BootTiming              = contracts.BootTiming
	OperationTiming         = contracts.OperationTiming
	ExecTiming              = contracts.ExecTiming
	SandboxTiming           = contracts.SandboxTiming
	DeploymentTimingSummary = contracts.DeploymentTimingSummary
	Problem                 = contracts.Problem
)

const (
	SandboxStateCreating = contracts.SandboxStateCreating
	SandboxStateStopped  = contracts.SandboxStateStopped
	SandboxStateStarting = contracts.SandboxStateStarting
	SandboxStateReady    = contracts.SandboxStateReady
	SandboxStateDraining = contracts.SandboxStateDraining
	SandboxStateStopping = contracts.SandboxStateStopping
	SandboxStateFailed   = contracts.SandboxStateFailed
	SandboxStateDeleting = contracts.SandboxStateDeleting
	SandboxStateDeleted  = contracts.SandboxStateDeleted

	OperationStatePending   = contracts.OperationStatePending
	OperationStateRunning   = contracts.OperationStateRunning
	OperationStateSucceeded = contracts.OperationStateSucceeded
	OperationStateFailed    = contracts.OperationStateFailed
	OperationStateCancelled = contracts.OperationStateCancelled

	ServiceAccountScopeSandboxRead      = "sandbox:read"
	ServiceAccountScopeSandboxLifecycle = "sandbox:lifecycle"
	ServiceAccountScopeSandboxExec      = "sandbox:exec"
	ServiceAccountScopeSandboxFiles     = "sandbox:files"
	ServiceAccountScopeSandboxArtifacts = "sandbox:artifacts"
	ServiceAccountScopeSandboxPorts     = "sandbox:ports"

	ProblemCodeStateConflict    = "state_conflict"
	ProblemCodeGenerationFenced = "generation_fenced"
	ProblemCodeLeaseFenced      = "lease_fenced"
	ProblemCodeWaitExpired      = "wait_expired"

	LeaseStateActive   = contracts.LeaseStateActive
	LeaseStateReleased = contracts.LeaseStateReleased
	LeaseStateExpired  = contracts.LeaseStateExpired
	LeaseStateFenced   = contracts.LeaseStateFenced

	SessionStateOpen     SessionState = "open"
	SessionStateDetached SessionState = "detached"
	SessionStateClosing  SessionState = "closing"
	SessionStateClosed   SessionState = "closed"
)

type Timestamp string
type SessionState string
type FileKind string
type InfrastructureFailureKind string
type SpawnFailureKind string

type ShellCommand struct {
	Command string `json:"command"`
	Mode    string `json:"mode"`
}

type ArgvCommand struct {
	Arguments  []string `json:"arguments"`
	Executable string   `json:"executable"`
	Mode       string   `json:"mode"`
}

type Command struct {
	ShellCommand *ShellCommand `json:"-"`
	ArgvCommand  *ArgvCommand  `json:"-"`
}

func (value Command) MarshalJSON() ([]byte, error) {
	if value.ShellCommand != nil && value.ArgvCommand == nil {
		return json.Marshal(value.ShellCommand)
	}
	if value.ArgvCommand != nil && value.ShellCommand == nil {
		return json.Marshal(value.ArgvCommand)
	}
	return nil, fmt.Errorf("SecondBox Command requires exactly one variant")
}

func (value *Command) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	switch discriminator.Mode {
	case "shell":
		var decoded ShellCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = Command{ShellCommand: &decoded}
	case "argv":
		var decoded ArgvCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = Command{ArgvCommand: &decoded}
	default:
		return fmt.Errorf("SecondBox Command has unsupported discriminator %q", discriminator.Mode)
	}
	return nil
}

type BufferedExecRequest struct {
	Command              Command        `json:"command"`
	Cwd                  *WorkspacePath `json:"cwd,omitempty"`
	Environment          StringMap      `json:"environment"`
	StdinBase64          *string        `json:"stdinBase64,omitempty"`
	DeadlineMilliseconds int64          `json:"deadlineMilliseconds"`
	MaximumOutputBytes   int64          `json:"maximumOutputBytes"`
}

type StreamingExecRequest struct {
	Command              Command        `json:"command"`
	Cwd                  *WorkspacePath `json:"cwd,omitempty"`
	Environment          StringMap      `json:"environment"`
	DeadlineMilliseconds int64          `json:"deadlineMilliseconds"`
	MaximumOutputBytes   int64          `json:"maximumOutputBytes"`
	WindowBytes          int64          `json:"windowBytes"`
}

type CreateTerminalRequest struct {
	Command              Command        `json:"command"`
	Cwd                  *WorkspacePath `json:"cwd,omitempty"`
	Environment          StringMap      `json:"environment"`
	Rows                 int            `json:"rows"`
	Columns              int            `json:"columns"`
	DeadlineMilliseconds int64          `json:"deadlineMilliseconds"`
	Detachable           bool           `json:"detachable"`
}

type WaitSandboxRequest struct {
	DeadlineMilliseconds int64          `json:"deadlineMilliseconds"`
	States               []SandboxState `json:"states"`
}

type ExecStreamSession struct {
	ExpiresAt    Timestamp    `json:"expiresAt"`
	Generation   int64        `json:"generation"`
	ID           OpaqueID     `json:"id"`
	SandboxID    OpaqueID     `json:"sandboxId"`
	State        SessionState `json:"state"`
	Subprotocol  string       `json:"subprotocol"`
	WebsocketURL string       `json:"websocketUrl"`
}

type TerminalSession struct {
	ExpiresAt          Timestamp    `json:"expiresAt"`
	Generation         int64        `json:"generation"`
	ID                 OpaqueID     `json:"id"`
	NextClientSequence int64        `json:"nextClientSequence"`
	SandboxID          OpaqueID     `json:"sandboxId"`
	State              SessionState `json:"state"`
	Subprotocol        string       `json:"subprotocol"`
	WebsocketURL       string       `json:"websocketUrl"`
}

type ExecOutput struct {
	StderrBase64 string `json:"stderrBase64"`
	StdoutBase64 string `json:"stdoutBase64"`
}

type ExecExited struct {
	ElapsedMilliseconds int64      `json:"elapsedMilliseconds"`
	ExitCode            int        `json:"exitCode"`
	Kind                string     `json:"kind"`
	Output              ExecOutput `json:"output"`
	Signal              *int       `json:"signal,omitempty"`
}

type ExecSpawnFailed struct {
	Kind    string           `json:"kind"`
	Message string           `json:"message"`
	Reason  SpawnFailureKind `json:"reason"`
}

type ExecDeadlineExceeded struct {
	ElapsedMilliseconds int64      `json:"elapsedMilliseconds"`
	Kind                string     `json:"kind"`
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
	Kind      string                    `json:"kind"`
	Message   string                    `json:"message"`
	Reason    InfrastructureFailureKind `json:"reason"`
	Retryable bool                      `json:"retryable"`
}

type ExecOutcome struct {
	ExecExited               *ExecExited               `json:"-"`
	ExecSpawnFailed          *ExecSpawnFailed          `json:"-"`
	ExecDeadlineExceeded     *ExecDeadlineExceeded     `json:"-"`
	ExecCancelled            *ExecCancelled            `json:"-"`
	ExecOutputExhausted      *ExecOutputExhausted      `json:"-"`
	ExecInfrastructureFailed *ExecInfrastructureFailed `json:"-"`
}

func (value ExecOutcome) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.ExecExited, value.ExecSpawnFailed, value.ExecDeadlineExceeded,
		value.ExecCancelled, value.ExecOutputExhausted, value.ExecInfrastructureFailed,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox ExecOutcome requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *ExecOutcome) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	switch discriminator.Kind {
	case "exited":
		var decoded ExecExited
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecExited: &decoded}
	case "spawn_failed":
		var decoded ExecSpawnFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecSpawnFailed: &decoded}
	case "deadline_exceeded":
		var decoded ExecDeadlineExceeded
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecDeadlineExceeded: &decoded}
	case "cancelled":
		var decoded ExecCancelled
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecCancelled: &decoded}
	case "output_exhausted":
		var decoded ExecOutputExhausted
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecOutputExhausted: &decoded}
	case "infrastructure_failed":
		var decoded ExecInfrastructureFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecInfrastructureFailed: &decoded}
	default:
		return fmt.Errorf("SecondBox ExecOutcome has unsupported discriminator %q", discriminator.Kind)
	}
	return nil
}

func isNilPointer(value any) bool {
	return value == nil || reflect.ValueOf(value).IsNil()
}

type StreamInputFrame struct {
	DataBase64 string `json:"dataBase64"`
	EndOfInput bool   `json:"endOfInput"`
	Sequence   int64  `json:"sequence"`
	Type       string `json:"type"`
}

type StreamOutputFrame struct {
	DataBase64 string `json:"dataBase64"`
	Sequence   int64  `json:"sequence"`
	Stream     string `json:"stream"`
	Type       string `json:"type"`
}

type StreamCreditFrame struct {
	Bytes    int64  `json:"bytes"`
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
}

type StreamSignalFrame struct {
	Sequence int64  `json:"sequence"`
	Signal   int    `json:"signal"`
	Type     string `json:"type"`
}

type StreamCancelFrame struct {
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
}

type StreamOutcomeFrame struct {
	Outcome  ExecOutcome `json:"outcome"`
	Sequence int64       `json:"sequence"`
	Type     string      `json:"type"`
}

type ExecStreamFrame struct {
	StreamInputFrame   *StreamInputFrame   `json:"-"`
	StreamOutputFrame  *StreamOutputFrame  `json:"-"`
	StreamCreditFrame  *StreamCreditFrame  `json:"-"`
	StreamSignalFrame  *StreamSignalFrame  `json:"-"`
	StreamCancelFrame  *StreamCancelFrame  `json:"-"`
	StreamOutcomeFrame *StreamOutcomeFrame `json:"-"`
}

func (value ExecStreamFrame) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.StreamInputFrame, value.StreamOutputFrame, value.StreamCreditFrame,
		value.StreamSignalFrame, value.StreamCancelFrame, value.StreamOutcomeFrame,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox ExecStreamFrame requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *ExecStreamFrame) UnmarshalJSON(data []byte) error {
	return unmarshalStreamFrame(data, func(kind string, decoded any) {
		switch kind {
		case "stdin":
			value.StreamInputFrame = decoded.(*StreamInputFrame)
		case "output":
			value.StreamOutputFrame = decoded.(*StreamOutputFrame)
		case "credit":
			value.StreamCreditFrame = decoded.(*StreamCreditFrame)
		case "signal":
			value.StreamSignalFrame = decoded.(*StreamSignalFrame)
		case "cancel":
			value.StreamCancelFrame = decoded.(*StreamCancelFrame)
		case "outcome":
			value.StreamOutcomeFrame = decoded.(*StreamOutcomeFrame)
		}
	})
}

type TerminalInputFrame struct {
	DataBase64 string `json:"dataBase64"`
	Sequence   int64  `json:"sequence"`
	Type       string `json:"type"`
}

type TerminalOutputFrame struct {
	DataBase64 string `json:"dataBase64"`
	Sequence   int64  `json:"sequence"`
	Type       string `json:"type"`
}

type TerminalResizeFrame struct {
	Columns  int    `json:"columns"`
	Rows     int    `json:"rows"`
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
}

type TerminalFrame struct {
	TerminalInputFrame  *TerminalInputFrame  `json:"-"`
	TerminalOutputFrame *TerminalOutputFrame `json:"-"`
	TerminalResizeFrame *TerminalResizeFrame `json:"-"`
	StreamCreditFrame   *StreamCreditFrame   `json:"-"`
	StreamCancelFrame   *StreamCancelFrame   `json:"-"`
	StreamOutcomeFrame  *StreamOutcomeFrame  `json:"-"`
}

func (value TerminalFrame) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.TerminalInputFrame, value.TerminalOutputFrame, value.TerminalResizeFrame,
		value.StreamCreditFrame, value.StreamCancelFrame, value.StreamOutcomeFrame,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox TerminalFrame requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *TerminalFrame) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	var decoded any
	switch discriminator.Type {
	case "terminal_input":
		decoded = &TerminalInputFrame{}
	case "terminal_output":
		decoded = &TerminalOutputFrame{}
	case "resize":
		decoded = &TerminalResizeFrame{}
	case "credit":
		decoded = &StreamCreditFrame{}
	case "cancel":
		decoded = &StreamCancelFrame{}
	case "outcome":
		decoded = &StreamOutcomeFrame{}
	default:
		return fmt.Errorf("SecondBox TerminalFrame has unsupported discriminator %q", discriminator.Type)
	}
	if err := json.Unmarshal(data, decoded); err != nil {
		return err
	}
	switch typed := decoded.(type) {
	case *TerminalInputFrame:
		value.TerminalInputFrame = typed
	case *TerminalOutputFrame:
		value.TerminalOutputFrame = typed
	case *TerminalResizeFrame:
		value.TerminalResizeFrame = typed
	case *StreamCreditFrame:
		value.StreamCreditFrame = typed
	case *StreamCancelFrame:
		value.StreamCancelFrame = typed
	case *StreamOutcomeFrame:
		value.StreamOutcomeFrame = typed
	}
	return nil
}

func unmarshalStreamFrame(data []byte, set func(string, any)) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	var decoded any
	switch discriminator.Type {
	case "stdin":
		decoded = &StreamInputFrame{}
	case "output":
		decoded = &StreamOutputFrame{}
	case "credit":
		decoded = &StreamCreditFrame{}
	case "signal":
		decoded = &StreamSignalFrame{}
	case "cancel":
		decoded = &StreamCancelFrame{}
	case "outcome":
		decoded = &StreamOutcomeFrame{}
	default:
		return fmt.Errorf("SecondBox stream frame has unsupported discriminator %q", discriminator.Type)
	}
	if err := json.Unmarshal(data, decoded); err != nil {
		return err
	}
	set(discriminator.Type, decoded)
	return nil
}

type FileStat struct {
	Kind       FileKind      `json:"kind"`
	ModifiedAt Timestamp     `json:"modifiedAt"`
	Path       WorkspacePath `json:"path"`
	SizeBytes  int64         `json:"sizeBytes"`
}

type DirectoryListing struct {
	Entries []FileStat    `json:"entries"`
	Path    WorkspacePath `json:"path"`
}

type FileExistsResult struct {
	Exists bool          `json:"exists"`
	Path   WorkspacePath `json:"path"`
}

type FileWriteResult struct {
	Path      WorkspacePath `json:"path"`
	SHA256    string        `json:"sha256"`
	SizeBytes int64         `json:"sizeBytes"`
}

type CreateDirectoryRequest struct {
	Path      WorkspacePath `json:"path"`
	Recursive bool          `json:"recursive"`
}

type RemovePathRequest struct {
	Force     bool          `json:"force"`
	Path      WorkspacePath `json:"path"`
	Recursive bool          `json:"recursive"`
}
