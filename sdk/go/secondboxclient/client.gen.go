// Code generated from contracts/openapi/v1/secondbox.openapi.json (sha256 aa3f6969ea9655b88d5a6e12969986c30bc51f2fce20b98e2b359816dce40811); DO NOT EDIT.

package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContractVersion is the OpenAPI info.version represented by this generated transport.
const ContractVersion = "1.0.0"

// APIKey is the wire representation of the api key schema.
type APIKey struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt *Timestamp `json:"expiresAt,omitempty"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// LastUsedAt carries the lastUsedAt JSON field.
	LastUsedAt *Timestamp `json:"lastUsedAt,omitempty"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// Prefix carries the prefix JSON field.
	Prefix string `json:"prefix"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// RevokedAt carries the revokedAt JSON field.
	RevokedAt *Timestamp `json:"revokedAt,omitempty"`
	// Scopes carries the scopes JSON field.
	Scopes []ServiceAccountScope `json:"scopes"`
	// ServiceAccountID carries the serviceAccountId JSON field.
	ServiceAccountID OpaqueID `json:"serviceAccountId"`
	// State carries the state JSON field.
	State APIKeyState `json:"state"`
}

// APIKeyPage is the wire representation of the api key page schema.
type APIKeyPage struct {
	// Items carries the items JSON field.
	Items []APIKey `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// APIKeyState is the wire representation of the api key state schema.
type APIKeyState string

const (
	// APIKeyStateActive is the "active" api key state value.
	APIKeyStateActive APIKeyState = "active"
	// APIKeyStateRevoked is the "revoked" api key state value.
	APIKeyStateRevoked APIKeyState = "revoked"
	// APIKeyStateExpired is the "expired" api key state value.
	APIKeyStateExpired APIKeyState = "expired"
)

// AcquireLeaseRequest is the wire representation of the acquire lease request schema.
type AcquireLeaseRequest struct {
	// DurationSeconds carries the durationSeconds JSON field.
	DurationSeconds int `json:"durationSeconds"`
}

// ArgvCommand is the wire representation of the argv command schema.
type ArgvCommand struct {
	// Arguments carries the arguments JSON field.
	Arguments []string `json:"arguments"`
	// Executable carries the executable JSON field.
	Executable string `json:"executable"`
	// Mode carries the mode JSON field.
	Mode string `json:"mode"`
}

// Artifact is the wire representation of the artifact schema.
type Artifact struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// MediaType carries the mediaType JSON field.
	MediaType string `json:"mediaType"`
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// SHA256 carries the sha256 JSON field.
	SHA256 string `json:"sha256"`
	// SizeBytes carries the sizeBytes JSON field.
	SizeBytes int64 `json:"sizeBytes"`
}

// ArtifactPage is the wire representation of the artifact page schema.
type ArtifactPage struct {
	// Items carries the items JSON field.
	Items []Artifact `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// BufferedExecRequest is the wire representation of the buffered exec request schema.
type BufferedExecRequest struct {
	// Command carries the command JSON field.
	Command Command `json:"command"`
	// Cwd carries the cwd JSON field.
	Cwd *WorkspacePath `json:"cwd,omitempty"`
	// DeadlineMilliseconds carries the deadlineMilliseconds JSON field.
	DeadlineMilliseconds int64 `json:"deadlineMilliseconds"`
	// Environment carries the environment JSON field.
	Environment StringMap `json:"environment"`
	// MaximumOutputBytes carries the maximumOutputBytes JSON field.
	MaximumOutputBytes int64 `json:"maximumOutputBytes"`
	// StdinBase64 carries the stdinBase64 JSON field.
	StdinBase64 *string `json:"stdinBase64,omitempty"`
}

// CheckpointPolicy is the wire representation of the checkpoint policy schema.
type CheckpointPolicy struct {
	// ArtifactRetentionSeconds carries the artifactRetentionSeconds JSON field.
	ArtifactRetentionSeconds int `json:"artifactRetentionSeconds"`
	// OnStop carries the onStop JSON field.
	OnStop bool `json:"onStop"`
	// RetentionSeconds carries the retentionSeconds JSON field.
	RetentionSeconds int `json:"retentionSeconds"`
	// SnapshotLimit carries the snapshotLimit JSON field.
	SnapshotLimit int `json:"snapshotLimit"`
}

// CheckpointSandboxRequest is the wire representation of the checkpoint sandbox request schema.
type CheckpointSandboxRequest struct {
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
}

// Command is the wire representation of the command schema.
type Command struct {
	// ShellCommand contains the shell command variant when selected.
	ShellCommand *ShellCommand `json:"-"`
	// ArgvCommand contains the argv command variant when selected.
	ArgvCommand *ArgvCommand `json:"-"`
}

// MarshalJSON encodes exactly one selected Command variant.
func (value Command) MarshalJSON() ([]byte, error) {
	selected := 0
	var encoded []byte
	var encodeErr error
	if value.ShellCommand != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ShellCommand)
	}
	if value.ArgvCommand != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ArgvCommand)
	}
	if encodeErr != nil {
		return nil, fmt.Errorf("SecondBox Command encode variant: %w", encodeErr)
	}
	if selected != 1 {
		return nil, fmt.Errorf("SecondBox Command requires exactly one variant, found %d", selected)
	}
	return encoded, nil
}

// UnmarshalJSON decodes a Command by its "mode" discriminator.
func (value *Command) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Value string `json:"mode"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return fmt.Errorf("SecondBox Command decode discriminator: %w", err)
	}
	switch discriminator.Value {
	case "shell":
		var decoded ShellCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox Command decode ShellCommand: %w", err)
		}
		*value = Command{ShellCommand: &decoded}
		return nil
	case "argv":
		var decoded ArgvCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox Command decode ArgvCommand: %w", err)
		}
		*value = Command{ArgvCommand: &decoded}
		return nil
	default:
		return fmt.Errorf("SecondBox Command has unsupported discriminator %q", discriminator.Value)
	}
}

// CorrelationID is the wire representation of the correlation id schema.
type CorrelationID string

// CreateAPIKeyRequest is the wire representation of the create api key request schema.
type CreateAPIKeyRequest struct {
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt *Timestamp `json:"expiresAt,omitempty"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// Scopes carries the scopes JSON field.
	Scopes []ServiceAccountScope `json:"scopes"`
}

// CreateAPIKeyResponse is the wire representation of the create api key response schema.
type CreateAPIKeyResponse struct {
	// APIKey carries the apiKey JSON field.
	APIKey APIKey `json:"apiKey"`
	// Credential carries the credential JSON field.
	Credential string `json:"credential"`
}

// CreateDirectoryRequest is the wire representation of the create directory request schema.
type CreateDirectoryRequest struct {
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
	// Recursive carries the recursive JSON field.
	Recursive bool `json:"recursive"`
}

// CreatePortSessionRequest is the wire representation of the create port session request schema.
type CreatePortSessionRequest struct {
	// DurationSeconds carries the durationSeconds JSON field.
	DurationSeconds int `json:"durationSeconds"`
	// Name carries the name JSON field.
	Name string `json:"name"`
}

// CreateProfileRequest is the wire representation of the create profile request schema.
type CreateProfileRequest struct {
	// Name carries the name JSON field.
	Name ProfileName `json:"name"`
	// Spec carries the spec JSON field.
	Spec ProfileRevisionSpec `json:"spec"`
}

// CreateProjectRequest is the wire representation of the create project request schema.
type CreateProjectRequest struct {
	// Name carries the name JSON field.
	Name string `json:"name"`
}

// CreateRunnerPoolRequest is the wire representation of the create runner pool request schema.
type CreateRunnerPoolRequest struct {
	// Architectures carries the architectures JSON field.
	Architectures RunnerArchitectureList `json:"architectures"`
	// Capabilities carries the capabilities JSON field.
	Capabilities RunnerCapabilityList `json:"capabilities"`
	// CapacityPolicy carries the capacityPolicy JSON field.
	CapacityPolicy RunnerCapacityPolicy `json:"capacityPolicy"`
	// Name carries the name JSON field.
	Name ProfileName `json:"name"`
	// State carries the state JSON field.
	State RunnerPoolState `json:"state"`
}

// CreateSandboxRequest is the wire representation of the create sandbox request schema.
type CreateSandboxRequest struct {
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Profile carries the profile JSON field.
	Profile ProfileName `json:"profile"`
}

// CreateServiceAccountRequest is the wire representation of the create service account request schema.
type CreateServiceAccountRequest struct {
	// Name carries the name JSON field.
	Name string `json:"name"`
	// ProfileGrants carries the profileGrants JSON field.
	ProfileGrants []ProfileName `json:"profileGrants"`
	// Scopes carries the scopes JSON field.
	Scopes []ServiceAccountScope `json:"scopes"`
}

// CreateSnapshotRequest is the wire representation of the create snapshot request schema.
type CreateSnapshotRequest struct {
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Name carries the name JSON field.
	Name string `json:"name"`
}

// CreateTerminalRequest is the wire representation of the create terminal request schema.
type CreateTerminalRequest struct {
	// Columns carries the columns JSON field.
	Columns int `json:"columns"`
	// Command carries the command JSON field.
	Command Command `json:"command"`
	// Cwd carries the cwd JSON field.
	Cwd *WorkspacePath `json:"cwd,omitempty"`
	// DeadlineMilliseconds carries the deadlineMilliseconds JSON field.
	DeadlineMilliseconds int64 `json:"deadlineMilliseconds"`
	// Detachable carries the detachable JSON field.
	Detachable bool `json:"detachable"`
	// Environment carries the environment JSON field.
	Environment StringMap `json:"environment"`
	// Rows carries the rows JSON field.
	Rows int `json:"rows"`
}

// DirectoryListing is the wire representation of the directory listing schema.
type DirectoryListing struct {
	// Entries carries the entries JSON field.
	Entries []FileStat `json:"entries"`
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
}

// ExecCancelled is the wire representation of the exec cancelled schema.
type ExecCancelled struct {
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// Output carries the output JSON field.
	Output ExecOutput `json:"output"`
}

// ExecDeadlineExceeded is the wire representation of the exec deadline exceeded schema.
type ExecDeadlineExceeded struct {
	// ElapsedMilliseconds carries the elapsedMilliseconds JSON field.
	ElapsedMilliseconds int64 `json:"elapsedMilliseconds"`
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// Output carries the output JSON field.
	Output ExecOutput `json:"output"`
}

// ExecExited is the wire representation of the exec exited schema.
type ExecExited struct {
	// ExitCode carries the exitCode JSON field.
	ExitCode int `json:"exitCode"`
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// Output carries the output JSON field.
	Output ExecOutput `json:"output"`
	// Signal carries the signal JSON field.
	Signal *int `json:"signal,omitempty"`
}

// ExecInfrastructureFailed is the wire representation of the exec infrastructure failed schema.
type ExecInfrastructureFailed struct {
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// Message carries the message JSON field.
	Message string `json:"message"`
	// Reason carries the reason JSON field.
	Reason InfrastructureFailureKind `json:"reason"`
	// Retryable carries the retryable JSON field.
	Retryable bool `json:"retryable"`
}

// ExecOutcome is the wire representation of the exec outcome schema.
type ExecOutcome struct {
	// ExecExited contains the exec exited variant when selected.
	ExecExited *ExecExited `json:"-"`
	// ExecSpawnFailed contains the exec spawn failed variant when selected.
	ExecSpawnFailed *ExecSpawnFailed `json:"-"`
	// ExecDeadlineExceeded contains the exec deadline exceeded variant when selected.
	ExecDeadlineExceeded *ExecDeadlineExceeded `json:"-"`
	// ExecCancelled contains the exec cancelled variant when selected.
	ExecCancelled *ExecCancelled `json:"-"`
	// ExecOutputExhausted contains the exec output exhausted variant when selected.
	ExecOutputExhausted *ExecOutputExhausted `json:"-"`
	// ExecInfrastructureFailed contains the exec infrastructure failed variant when selected.
	ExecInfrastructureFailed *ExecInfrastructureFailed `json:"-"`
}

// MarshalJSON encodes exactly one selected ExecOutcome variant.
func (value ExecOutcome) MarshalJSON() ([]byte, error) {
	selected := 0
	var encoded []byte
	var encodeErr error
	if value.ExecExited != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecExited)
	}
	if value.ExecSpawnFailed != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecSpawnFailed)
	}
	if value.ExecDeadlineExceeded != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecDeadlineExceeded)
	}
	if value.ExecCancelled != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecCancelled)
	}
	if value.ExecOutputExhausted != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecOutputExhausted)
	}
	if value.ExecInfrastructureFailed != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.ExecInfrastructureFailed)
	}
	if encodeErr != nil {
		return nil, fmt.Errorf("SecondBox ExecOutcome encode variant: %w", encodeErr)
	}
	if selected != 1 {
		return nil, fmt.Errorf("SecondBox ExecOutcome requires exactly one variant, found %d", selected)
	}
	return encoded, nil
}

// UnmarshalJSON decodes a ExecOutcome by its "kind" discriminator.
func (value *ExecOutcome) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Value string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return fmt.Errorf("SecondBox ExecOutcome decode discriminator: %w", err)
	}
	switch discriminator.Value {
	case "exited":
		var decoded ExecExited
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecExited: %w", err)
		}
		*value = ExecOutcome{ExecExited: &decoded}
		return nil
	case "spawn_failed":
		var decoded ExecSpawnFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecSpawnFailed: %w", err)
		}
		*value = ExecOutcome{ExecSpawnFailed: &decoded}
		return nil
	case "deadline_exceeded":
		var decoded ExecDeadlineExceeded
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecDeadlineExceeded: %w", err)
		}
		*value = ExecOutcome{ExecDeadlineExceeded: &decoded}
		return nil
	case "cancelled":
		var decoded ExecCancelled
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecCancelled: %w", err)
		}
		*value = ExecOutcome{ExecCancelled: &decoded}
		return nil
	case "output_exhausted":
		var decoded ExecOutputExhausted
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecOutputExhausted: %w", err)
		}
		*value = ExecOutcome{ExecOutputExhausted: &decoded}
		return nil
	case "infrastructure_failed":
		var decoded ExecInfrastructureFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecOutcome decode ExecInfrastructureFailed: %w", err)
		}
		*value = ExecOutcome{ExecInfrastructureFailed: &decoded}
		return nil
	default:
		return fmt.Errorf("SecondBox ExecOutcome has unsupported discriminator %q", discriminator.Value)
	}
}

// ExecOutput is the wire representation of the exec output schema.
type ExecOutput struct {
	// StderrBase64 carries the stderrBase64 JSON field.
	StderrBase64 string `json:"stderrBase64"`
	// StdoutBase64 carries the stdoutBase64 JSON field.
	StdoutBase64 string `json:"stdoutBase64"`
}

// ExecOutputExhausted is the wire representation of the exec output exhausted schema.
type ExecOutputExhausted struct {
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// LimitBytes carries the limitBytes JSON field.
	LimitBytes int64 `json:"limitBytes"`
	// Output carries the output JSON field.
	Output ExecOutput `json:"output"`
}

// ExecSpawnFailed is the wire representation of the exec spawn failed schema.
type ExecSpawnFailed struct {
	// Kind carries the kind JSON field.
	Kind string `json:"kind"`
	// Message carries the message JSON field.
	Message string `json:"message"`
	// Reason carries the reason JSON field.
	Reason SpawnFailureKind `json:"reason"`
}

// ExecStreamFrame is the wire representation of the exec stream frame schema.
type ExecStreamFrame struct {
	// StreamInputFrame contains the stream input frame variant when selected.
	StreamInputFrame *StreamInputFrame `json:"-"`
	// StreamOutputFrame contains the stream output frame variant when selected.
	StreamOutputFrame *StreamOutputFrame `json:"-"`
	// StreamCreditFrame contains the stream credit frame variant when selected.
	StreamCreditFrame *StreamCreditFrame `json:"-"`
	// StreamSignalFrame contains the stream signal frame variant when selected.
	StreamSignalFrame *StreamSignalFrame `json:"-"`
	// StreamCancelFrame contains the stream cancel frame variant when selected.
	StreamCancelFrame *StreamCancelFrame `json:"-"`
	// StreamOutcomeFrame contains the stream outcome frame variant when selected.
	StreamOutcomeFrame *StreamOutcomeFrame `json:"-"`
}

// MarshalJSON encodes exactly one selected ExecStreamFrame variant.
func (value ExecStreamFrame) MarshalJSON() ([]byte, error) {
	selected := 0
	var encoded []byte
	var encodeErr error
	if value.StreamInputFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamInputFrame)
	}
	if value.StreamOutputFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamOutputFrame)
	}
	if value.StreamCreditFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamCreditFrame)
	}
	if value.StreamSignalFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamSignalFrame)
	}
	if value.StreamCancelFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamCancelFrame)
	}
	if value.StreamOutcomeFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamOutcomeFrame)
	}
	if encodeErr != nil {
		return nil, fmt.Errorf("SecondBox ExecStreamFrame encode variant: %w", encodeErr)
	}
	if selected != 1 {
		return nil, fmt.Errorf("SecondBox ExecStreamFrame requires exactly one variant, found %d", selected)
	}
	return encoded, nil
}

// UnmarshalJSON decodes a ExecStreamFrame by its "type" discriminator.
func (value *ExecStreamFrame) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Value string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return fmt.Errorf("SecondBox ExecStreamFrame decode discriminator: %w", err)
	}
	switch discriminator.Value {
	case "stdin":
		var decoded StreamInputFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamInputFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamInputFrame: &decoded}
		return nil
	case "output":
		var decoded StreamOutputFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamOutputFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamOutputFrame: &decoded}
		return nil
	case "credit":
		var decoded StreamCreditFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamCreditFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamCreditFrame: &decoded}
		return nil
	case "signal":
		var decoded StreamSignalFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamSignalFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamSignalFrame: &decoded}
		return nil
	case "cancel":
		var decoded StreamCancelFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamCancelFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamCancelFrame: &decoded}
		return nil
	case "outcome":
		var decoded StreamOutcomeFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox ExecStreamFrame decode StreamOutcomeFrame: %w", err)
		}
		*value = ExecStreamFrame{StreamOutcomeFrame: &decoded}
		return nil
	default:
		return fmt.Errorf("SecondBox ExecStreamFrame has unsupported discriminator %q", discriminator.Value)
	}
}

// ExecStreamSession is the wire representation of the exec stream session schema.
type ExecStreamSession struct {
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// State carries the state JSON field.
	State SessionState `json:"state"`
	// Subprotocol carries the subprotocol JSON field.
	Subprotocol string `json:"subprotocol"`
	// WebsocketURL carries the websocketUrl JSON field.
	WebsocketURL string `json:"websocketUrl"`
}

// ExecutionPolicy is the wire representation of the execution policy schema.
type ExecutionPolicy struct {
	// MaximumBufferedOutputBytes carries the maximumBufferedOutputBytes JSON field.
	MaximumBufferedOutputBytes int64 `json:"maximumBufferedOutputBytes"`
	// MaximumDeadlineMilliseconds carries the maximumDeadlineMilliseconds JSON field.
	MaximumDeadlineMilliseconds int64 `json:"maximumDeadlineMilliseconds"`
	// MaximumTransferBytes carries the maximumTransferBytes JSON field.
	MaximumTransferBytes int64 `json:"maximumTransferBytes"`
	// StreamWindowBytes carries the streamWindowBytes JSON field.
	StreamWindowBytes int64 `json:"streamWindowBytes"`
	// TerminalDetachSeconds carries the terminalDetachSeconds JSON field.
	TerminalDetachSeconds int `json:"terminalDetachSeconds"`
}

// FileExistsResult is the wire representation of the file exists result schema.
type FileExistsResult struct {
	// Exists carries the exists JSON field.
	Exists bool `json:"exists"`
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
}

// FileKind is the wire representation of the file kind schema.
type FileKind string

const (
	// FileKindFile is the "file" file kind value.
	FileKindFile FileKind = "file"
	// FileKindDirectory is the "directory" file kind value.
	FileKindDirectory FileKind = "directory"
	// FileKindSymbolicLink is the "symbolic_link" file kind value.
	FileKindSymbolicLink FileKind = "symbolic_link"
)

// FileStat is the wire representation of the file stat schema.
type FileStat struct {
	// Kind carries the kind JSON field.
	Kind FileKind `json:"kind"`
	// ModifiedAt carries the modifiedAt JSON field.
	ModifiedAt Timestamp `json:"modifiedAt"`
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
	// SizeBytes carries the sizeBytes JSON field.
	SizeBytes int64 `json:"sizeBytes"`
}

// FileWriteResult is the wire representation of the file write result schema.
type FileWriteResult struct {
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
	// SHA256 carries the sha256 JSON field.
	SHA256 string `json:"sha256"`
	// SizeBytes carries the sizeBytes JSON field.
	SizeBytes int64 `json:"sizeBytes"`
}

// InfrastructureFailureKind is the wire representation of the infrastructure failure kind schema.
type InfrastructureFailureKind string

const (
	// InfrastructureFailureKindTransport is the "transport" infrastructure failure kind value.
	InfrastructureFailureKindTransport InfrastructureFailureKind = "transport"
	// InfrastructureFailureKindAdmission is the "admission" infrastructure failure kind value.
	InfrastructureFailureKindAdmission InfrastructureFailureKind = "admission"
	// InfrastructureFailureKindGenerationFenced is the "generation_fenced" infrastructure failure kind value.
	InfrastructureFailureKindGenerationFenced InfrastructureFailureKind = "generation_fenced"
	// InfrastructureFailureKindLeaseFenced is the "lease_fenced" infrastructure failure kind value.
	InfrastructureFailureKindLeaseFenced InfrastructureFailureKind = "lease_fenced"
	// InfrastructureFailureKindGuestAgent is the "guest_agent" infrastructure failure kind value.
	InfrastructureFailureKindGuestAgent InfrastructureFailureKind = "guest_agent"
	// InfrastructureFailureKindExecutionNode is the "execution_node" infrastructure failure kind value.
	InfrastructureFailureKindExecutionNode InfrastructureFailureKind = "execution_node"
	// InfrastructureFailureKindService is the "service" infrastructure failure kind value.
	InfrastructureFailureKindService InfrastructureFailureKind = "service"
)

// Instance is the wire representation of the instance schema.
type Instance struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// ReadyAt carries the readyAt JSON field.
	ReadyAt *Timestamp `json:"readyAt,omitempty"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// State carries the state JSON field.
	State InstanceState `json:"state"`
	// StoppedAt carries the stoppedAt JSON field.
	StoppedAt *Timestamp `json:"stoppedAt,omitempty"`
	// TerminationReason carries the terminationReason JSON field.
	TerminationReason *InstanceTerminationReason `json:"terminationReason,omitempty"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// InstanceState is the wire representation of the instance state schema.
type InstanceState string

const (
	// InstanceStateStarting is the "starting" instance state value.
	InstanceStateStarting InstanceState = "starting"
	// InstanceStateReady is the "ready" instance state value.
	InstanceStateReady InstanceState = "ready"
	// InstanceStateDraining is the "draining" instance state value.
	InstanceStateDraining InstanceState = "draining"
	// InstanceStateStopping is the "stopping" instance state value.
	InstanceStateStopping InstanceState = "stopping"
	// InstanceStateStopped is the "stopped" instance state value.
	InstanceStateStopped InstanceState = "stopped"
	// InstanceStateLost is the "lost" instance state value.
	InstanceStateLost InstanceState = "lost"
	// InstanceStateFailed is the "failed" instance state value.
	InstanceStateFailed InstanceState = "failed"
)

// InstanceTerminationReason is the wire representation of the instance termination reason schema.
type InstanceTerminationReason string

const (
	// InstanceTerminationReasonRequestedDrain is the "requested_drain" instance termination reason value.
	InstanceTerminationReasonRequestedDrain InstanceTerminationReason = "requested_drain"
	// InstanceTerminationReasonRequestedStop is the "requested_stop" instance termination reason value.
	InstanceTerminationReasonRequestedStop InstanceTerminationReason = "requested_stop"
	// InstanceTerminationReasonIdleTimeout is the "idle_timeout" instance termination reason value.
	InstanceTerminationReasonIdleTimeout InstanceTerminationReason = "idle_timeout"
	// InstanceTerminationReasonMaximumDuration is the "maximum_duration" instance termination reason value.
	InstanceTerminationReasonMaximumDuration InstanceTerminationReason = "maximum_duration"
	// InstanceTerminationReasonGuestShutdown is the "guest_shutdown" instance termination reason value.
	InstanceTerminationReasonGuestShutdown InstanceTerminationReason = "guest_shutdown"
	// InstanceTerminationReasonResourceExhaustion is the "resource_exhaustion" instance termination reason value.
	InstanceTerminationReasonResourceExhaustion InstanceTerminationReason = "resource_exhaustion"
	// InstanceTerminationReasonGuestAgentLost is the "guest_agent_lost" instance termination reason value.
	InstanceTerminationReasonGuestAgentLost InstanceTerminationReason = "guest_agent_lost"
	// InstanceTerminationReasonRunnerLost is the "runner_lost" instance termination reason value.
	InstanceTerminationReasonRunnerLost InstanceTerminationReason = "runner_lost"
	// InstanceTerminationReasonStartupFailed is the "startup_failed" instance termination reason value.
	InstanceTerminationReasonStartupFailed InstanceTerminationReason = "startup_failed"
	// InstanceTerminationReasonFenced is the "fenced" instance termination reason value.
	InstanceTerminationReasonFenced InstanceTerminationReason = "fenced"
	// InstanceTerminationReasonInternalFailure is the "internal_failure" instance termination reason value.
	InstanceTerminationReasonInternalFailure InstanceTerminationReason = "internal_failure"
)

// Lease is the wire representation of the lease schema.
type Lease struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// State carries the state JSON field.
	State LeaseState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// LeaseState is the wire representation of the lease state schema.
type LeaseState string

const (
	// LeaseStateActive is the "active" lease state value.
	LeaseStateActive LeaseState = "active"
	// LeaseStateReleased is the "released" lease state value.
	LeaseStateReleased LeaseState = "released"
	// LeaseStateExpired is the "expired" lease state value.
	LeaseStateExpired LeaseState = "expired"
	// LeaseStateFenced is the "fenced" lease state value.
	LeaseStateFenced LeaseState = "fenced"
)

// LifecyclePolicy is the wire representation of the lifecycle policy schema.
type LifecyclePolicy struct {
	// DrainGraceSeconds carries the drainGraceSeconds JSON field.
	DrainGraceSeconds int `json:"drainGraceSeconds"`
	// IdleSeconds carries the idleSeconds JSON field.
	IdleSeconds int `json:"idleSeconds"`
	// InitialState carries the initialState JSON field.
	InitialState string `json:"initialState"`
	// LeaseSeconds carries the leaseSeconds JSON field.
	LeaseSeconds int `json:"leaseSeconds"`
	// MaximumDurationSeconds carries the maximumDurationSeconds JSON field.
	MaximumDurationSeconds int `json:"maximumDurationSeconds"`
}

// Metadata is the wire representation of the metadata schema.
type Metadata map[string]string

// NetworkDestination is the wire representation of the network destination schema.
type NetworkDestination struct {
	// Cidr carries the cidr JSON field.
	Cidr *string `json:"cidr,omitempty"`
	// Domain carries the domain JSON field.
	Domain *string `json:"domain,omitempty"`
	// Port carries the port JSON field.
	Port int `json:"port"`
	// Protocol carries the protocol JSON field.
	Protocol string `json:"protocol"`
}

// NetworkPolicy is the wire representation of the network policy schema.
type NetworkPolicy struct {
	// Destinations carries the destinations JSON field.
	Destinations []NetworkDestination `json:"destinations"`
	// Mode carries the mode JSON field.
	Mode string `json:"mode"`
}

// OpaqueID is the wire representation of the opaque id schema.
type OpaqueID string

// Operation is the wire representation of the operation schema.
type Operation struct {
	// CompletedAt carries the completedAt JSON field.
	CompletedAt *Timestamp `json:"completedAt,omitempty"`
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// Error carries the error JSON field.
	Error *Problem `json:"error,omitempty"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Kind carries the kind JSON field.
	Kind OperationKind `json:"kind"`
	// RequestID carries the requestId JSON field.
	RequestID CorrelationID `json:"requestId"`
	// Sandbox carries the sandbox JSON field.
	Sandbox *Sandbox `json:"sandbox,omitempty"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// StartedAt carries the startedAt JSON field.
	StartedAt *Timestamp `json:"startedAt,omitempty"`
	// State carries the state JSON field.
	State OperationState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// OperationKind is the wire representation of the operation kind schema.
type OperationKind string

const (
	// OperationKindCreate is the "create" operation kind value.
	OperationKindCreate OperationKind = "create"
	// OperationKindStart is the "start" operation kind value.
	OperationKindStart OperationKind = "start"
	// OperationKindDrain is the "drain" operation kind value.
	OperationKindDrain OperationKind = "drain"
	// OperationKindStop is the "stop" operation kind value.
	OperationKindStop OperationKind = "stop"
	// OperationKindCheckpoint is the "checkpoint" operation kind value.
	OperationKindCheckpoint OperationKind = "checkpoint"
	// OperationKindDelete is the "delete" operation kind value.
	OperationKindDelete OperationKind = "delete"
	// OperationKindCancelExec is the "cancel_exec" operation kind value.
	OperationKindCancelExec OperationKind = "cancel_exec"
	// OperationKindCancelTerminal is the "cancel_terminal" operation kind value.
	OperationKindCancelTerminal OperationKind = "cancel_terminal"
)

// OperationState is the wire representation of the operation state schema.
type OperationState string

const (
	// OperationStatePending is the "pending" operation state value.
	OperationStatePending OperationState = "pending"
	// OperationStateRunning is the "running" operation state value.
	OperationStateRunning OperationState = "running"
	// OperationStateSucceeded is the "succeeded" operation state value.
	OperationStateSucceeded OperationState = "succeeded"
	// OperationStateFailed is the "failed" operation state value.
	OperationStateFailed OperationState = "failed"
	// OperationStateCancelled is the "cancelled" operation state value.
	OperationStateCancelled OperationState = "cancelled"
)

// PingResult is the wire representation of the ping result schema.
type PingResult struct {
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// Healthy carries the healthy JSON field.
	Healthy bool `json:"healthy"`
	// ObservedAt carries the observedAt JSON field.
	ObservedAt Timestamp `json:"observedAt"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
}

// PortPolicy is the wire representation of the port policy schema.
type PortPolicy struct {
	// MaximumSessionSeconds carries the maximumSessionSeconds JSON field.
	MaximumSessionSeconds int `json:"maximumSessionSeconds"`
	// MaximumSessions carries the maximumSessions JSON field.
	MaximumSessions int `json:"maximumSessions"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// Port carries the port JSON field.
	Port int `json:"port"`
	// Protocol carries the protocol JSON field.
	Protocol string `json:"protocol"`
}

// PortSession is the wire representation of the port session schema.
type PortSession struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// Endpoint carries the endpoint JSON field.
	Endpoint string `json:"endpoint"`
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// Protocol carries the protocol JSON field.
	Protocol string `json:"protocol"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// State carries the state JSON field.
	State string `json:"state"`
}

// Problem is the wire representation of the problem schema.
type Problem struct {
	// Code carries the code JSON field.
	Code ProblemCode `json:"code"`
	// Details carries the details JSON field.
	Details *[]ProblemDetail `json:"details,omitempty"`
	// RequestID carries the requestId JSON field.
	RequestID CorrelationID `json:"requestId"`
	// Retryable carries the retryable JSON field.
	Retryable bool `json:"retryable"`
	// Status carries the status JSON field.
	Status int `json:"status"`
	// Title carries the title JSON field.
	Title string `json:"title"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// ProblemCode is the wire representation of the problem code schema.
type ProblemCode string

const (
	// ProblemCodeInvalidRequest is the "invalid_request" problem code value.
	ProblemCodeInvalidRequest ProblemCode = "invalid_request"
	// ProblemCodeAuthenticationFailed is the "authentication_failed" problem code value.
	ProblemCodeAuthenticationFailed ProblemCode = "authentication_failed"
	// ProblemCodeAuthorizationFailed is the "authorization_failed" problem code value.
	ProblemCodeAuthorizationFailed ProblemCode = "authorization_failed"
	// ProblemCodeNotFound is the "not_found" problem code value.
	ProblemCodeNotFound ProblemCode = "not_found"
	// ProblemCodeIdempotencyConflict is the "idempotency_conflict" problem code value.
	ProblemCodeIdempotencyConflict ProblemCode = "idempotency_conflict"
	// ProblemCodePreconditionFailed is the "precondition_failed" problem code value.
	ProblemCodePreconditionFailed ProblemCode = "precondition_failed"
	// ProblemCodeStateConflict is the "state_conflict" problem code value.
	ProblemCodeStateConflict ProblemCode = "state_conflict"
	// ProblemCodeGenerationFenced is the "generation_fenced" problem code value.
	ProblemCodeGenerationFenced ProblemCode = "generation_fenced"
	// ProblemCodeLeaseFenced is the "lease_fenced" problem code value.
	ProblemCodeLeaseFenced ProblemCode = "lease_fenced"
	// ProblemCodeProfileUnavailable is the "profile_unavailable" problem code value.
	ProblemCodeProfileUnavailable ProblemCode = "profile_unavailable"
	// ProblemCodeQuotaExceeded is the "quota_exceeded" problem code value.
	ProblemCodeQuotaExceeded ProblemCode = "quota_exceeded"
	// ProblemCodeLimitExceeded is the "limit_exceeded" problem code value.
	ProblemCodeLimitExceeded ProblemCode = "limit_exceeded"
	// ProblemCodeGuestUnavailable is the "guest_unavailable" problem code value.
	ProblemCodeGuestUnavailable ProblemCode = "guest_unavailable"
	// ProblemCodeExecutionNodeUnavailable is the "execution_node_unavailable" problem code value.
	ProblemCodeExecutionNodeUnavailable ProblemCode = "execution_node_unavailable"
	// ProblemCodeDependencyUnavailable is the "dependency_unavailable" problem code value.
	ProblemCodeDependencyUnavailable ProblemCode = "dependency_unavailable"
	// ProblemCodeInternalError is the "internal_error" problem code value.
	ProblemCodeInternalError ProblemCode = "internal_error"
	// ProblemCodeWaitExpired is the "wait_expired" problem code value.
	ProblemCodeWaitExpired ProblemCode = "wait_expired"
)

// ProblemDetail is the wire representation of the problem detail schema.
type ProblemDetail struct {
	// Field carries the field JSON field.
	Field string `json:"field"`
	// Reason carries the reason JSON field.
	Reason string `json:"reason"`
}

// Profile is the wire representation of the profile schema.
type Profile struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// CurrentRevision carries the currentRevision JSON field.
	CurrentRevision ProfileRevision `json:"currentRevision"`
	// Name carries the name JSON field.
	Name ProfileName `json:"name"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// State carries the state JSON field.
	State ProfileState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// ProfileName is the wire representation of the profile name schema.
type ProfileName string

// ProfilePage is the wire representation of the profile page schema.
type ProfilePage struct {
	// Items carries the items JSON field.
	Items []Profile `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// ProfileRevision is the wire representation of the profile revision schema.
type ProfileRevision struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Number carries the number JSON field.
	Number int `json:"number"`
	// Spec carries the spec JSON field.
	Spec ProfileRevisionSpec `json:"spec"`
}

// ProfileRevisionSpec is the wire representation of the profile revision spec schema.
type ProfileRevisionSpec struct {
	// Architecture carries the architecture JSON field.
	Architecture string `json:"architecture"`
	// Backend carries the backend JSON field.
	Backend string `json:"backend"`
	// Checkpoint carries the checkpoint JSON field.
	Checkpoint CheckpointPolicy `json:"checkpoint"`
	// Execution carries the execution JSON field.
	Execution ExecutionPolicy `json:"execution"`
	// Lifecycle carries the lifecycle JSON field.
	Lifecycle LifecyclePolicy `json:"lifecycle"`
	// Network carries the network JSON field.
	Network NetworkPolicy `json:"network"`
	// Pool carries the pool JSON field.
	Pool string `json:"pool"`
	// Ports carries the ports JSON field.
	Ports []PortPolicy `json:"ports"`
	// Resources carries the resources JSON field.
	Resources ResourcePolicy `json:"resources"`
	// RuntimeBundleDigest carries the runtimeBundleDigest JSON field.
	RuntimeBundleDigest string `json:"runtimeBundleDigest"`
	// ToolchainBundleDigest carries the toolchainBundleDigest JSON field.
	ToolchainBundleDigest string `json:"toolchainBundleDigest"`
}

// ProfileState is the wire representation of the profile state schema.
type ProfileState string

const (
	// ProfileStateEnabled is the "enabled" profile state value.
	ProfileStateEnabled ProfileState = "enabled"
	// ProfileStateDisabled is the "disabled" profile state value.
	ProfileStateDisabled ProfileState = "disabled"
)

// Project is the wire representation of the project schema.
type Project struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// State carries the state JSON field.
	State ProjectState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// ProjectPage is the wire representation of the project page schema.
type ProjectPage struct {
	// Items carries the items JSON field.
	Items []Project `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// ProjectState is the wire representation of the project state schema.
type ProjectState string

const (
	// ProjectStateActive is the "active" project state value.
	ProjectStateActive ProjectState = "active"
	// ProjectStateDisabled is the "disabled" project state value.
	ProjectStateDisabled ProjectState = "disabled"
)

// RemovePathRequest is the wire representation of the remove path request schema.
type RemovePathRequest struct {
	// Force carries the force JSON field.
	Force bool `json:"force"`
	// Path carries the path JSON field.
	Path WorkspacePath `json:"path"`
	// Recursive carries the recursive JSON field.
	Recursive bool `json:"recursive"`
}

// RenewLeaseRequest is the wire representation of the renew lease request schema.
type RenewLeaseRequest struct {
	// DurationSeconds carries the durationSeconds JSON field.
	DurationSeconds int `json:"durationSeconds"`
}

// ResourcePolicy is the wire representation of the resource policy schema.
type ResourcePolicy struct {
	// ConcurrentOperations carries the concurrentOperations JSON field.
	ConcurrentOperations int `json:"concurrentOperations"`
	// CPUMillis carries the cpuMillis JSON field.
	CPUMillis int `json:"cpuMillis"`
	// MemoryBytes carries the memoryBytes JSON field.
	MemoryBytes int64 `json:"memoryBytes"`
	// ProcessLimit carries the processLimit JSON field.
	ProcessLimit int `json:"processLimit"`
	// WorkspaceBytes carries the workspaceBytes JSON field.
	WorkspaceBytes int64 `json:"workspaceBytes"`
}

// ReviseProfileRequest is the wire representation of the revise profile request schema.
type ReviseProfileRequest struct {
	// Spec carries the spec JSON field.
	Spec ProfileRevisionSpec `json:"spec"`
}

// Runner is the wire representation of the runner schema.
type Runner struct {
	// Architectures carries the architectures JSON field.
	Architectures []string `json:"architectures"`
	// Capabilities carries the capabilities JSON field.
	Capabilities []string `json:"capabilities"`
	// Capacity carries the capacity JSON field.
	Capacity map[string]int64 `json:"capacity"`
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// CredentialState carries the credentialState JSON field.
	CredentialState string `json:"credentialState"`
	// ID carries the id JSON field.
	ID RunnerID `json:"id"`
	// LastSeenAt carries the lastSeenAt JSON field.
	LastSeenAt *Timestamp `json:"lastSeenAt,omitempty"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// PoolName carries the poolName JSON field.
	PoolName ProfileName `json:"poolName"`
	// ProtocolVersions carries the protocolVersions JSON field.
	ProtocolVersions []string `json:"protocolVersions"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// State carries the state JSON field.
	State string `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// RunnerArchitectureList is the wire representation of the runner architecture list schema.
type RunnerArchitectureList []string

// RunnerCapabilityList is the wire representation of the runner capability list schema.
type RunnerCapabilityList []string

// RunnerCapacityPolicy is the wire representation of the runner capacity policy schema.
type RunnerCapacityPolicy map[string]int64

// RunnerID is the wire representation of the runner id schema.
type RunnerID string

// RunnerPage is the wire representation of the runner page schema.
type RunnerPage struct {
	// Items carries the items JSON field.
	Items []Runner `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// RunnerPool is the wire representation of the runner pool schema.
type RunnerPool struct {
	// Architectures carries the architectures JSON field.
	Architectures RunnerArchitectureList `json:"architectures"`
	// Capabilities carries the capabilities JSON field.
	Capabilities RunnerCapabilityList `json:"capabilities"`
	// CapacityPolicy carries the capacityPolicy JSON field.
	CapacityPolicy RunnerCapacityPolicy `json:"capacityPolicy"`
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// Name carries the name JSON field.
	Name ProfileName `json:"name"`
	// ReadyRunnerCount carries the readyRunnerCount JSON field.
	ReadyRunnerCount int64 `json:"readyRunnerCount"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// State carries the state JSON field.
	State RunnerPoolState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// RunnerPoolPage is the wire representation of the runner pool page schema.
type RunnerPoolPage struct {
	// Items carries the items JSON field.
	Items []RunnerPool `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// RunnerPoolState is the wire representation of the runner pool state schema.
type RunnerPoolState string

const (
	// RunnerPoolStateReady is the "ready" runner pool state value.
	RunnerPoolStateReady RunnerPoolState = "ready"
	// RunnerPoolStateDraining is the "draining" runner pool state value.
	RunnerPoolStateDraining RunnerPoolState = "draining"
	// RunnerPoolStateOffline is the "offline" runner pool state value.
	RunnerPoolStateOffline RunnerPoolState = "offline"
)

// Sandbox is the wire representation of the sandbox schema.
type Sandbox struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// DeletedAt carries the deletedAt JSON field.
	DeletedAt *Timestamp `json:"deletedAt,omitempty"`
	// DesiredState carries the desiredState JSON field.
	DesiredState SandboxDesiredState `json:"desiredState"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Instance carries the instance JSON field.
	Instance *Instance `json:"instance,omitempty"`
	// LastActivityAt carries the lastActivityAt JSON field.
	LastActivityAt *Timestamp `json:"lastActivityAt,omitempty"`
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Profile carries the profile JSON field.
	Profile ProfileName `json:"profile"`
	// ProfileRevisionID carries the profileRevisionId JSON field.
	ProfileRevisionID OpaqueID `json:"profileRevisionId"`
	// ProjectID carries the projectId JSON field.
	ProjectID OpaqueID `json:"projectId"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// State carries the state JSON field.
	State SandboxState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
	// Workspace carries the workspace JSON field.
	Workspace Workspace `json:"workspace"`
}

// SandboxDesiredState is the wire representation of the sandbox desired state schema.
type SandboxDesiredState string

const (
	// SandboxDesiredStateRunning is the "running" sandbox desired state value.
	SandboxDesiredStateRunning SandboxDesiredState = "running"
	// SandboxDesiredStateStopped is the "stopped" sandbox desired state value.
	SandboxDesiredStateStopped SandboxDesiredState = "stopped"
	// SandboxDesiredStateDeleted is the "deleted" sandbox desired state value.
	SandboxDesiredStateDeleted SandboxDesiredState = "deleted"
)

// SandboxInspection is the wire representation of the sandbox inspection schema.
type SandboxInspection struct {
	// ActiveSessions carries the activeSessions JSON field.
	ActiveSessions int `json:"activeSessions"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// GuestHealthy carries the guestHealthy JSON field.
	GuestHealthy bool `json:"guestHealthy"`
	// ObservedAt carries the observedAt JSON field.
	ObservedAt Timestamp `json:"observedAt"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
}

// SandboxPage is the wire representation of the sandbox page schema.
type SandboxPage struct {
	// Items carries the items JSON field.
	Items []Sandbox `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// SandboxState is the wire representation of the sandbox state schema.
type SandboxState string

const (
	// SandboxStateCreating is the "creating" sandbox state value.
	SandboxStateCreating SandboxState = "creating"
	// SandboxStateStopped is the "stopped" sandbox state value.
	SandboxStateStopped SandboxState = "stopped"
	// SandboxStateStarting is the "starting" sandbox state value.
	SandboxStateStarting SandboxState = "starting"
	// SandboxStateReady is the "ready" sandbox state value.
	SandboxStateReady SandboxState = "ready"
	// SandboxStateDraining is the "draining" sandbox state value.
	SandboxStateDraining SandboxState = "draining"
	// SandboxStateStopping is the "stopping" sandbox state value.
	SandboxStateStopping SandboxState = "stopping"
	// SandboxStateCheckpointing is the "checkpointing" sandbox state value.
	SandboxStateCheckpointing SandboxState = "checkpointing"
	// SandboxStateFailed is the "failed" sandbox state value.
	SandboxStateFailed SandboxState = "failed"
	// SandboxStateDeleting is the "deleting" sandbox state value.
	SandboxStateDeleting SandboxState = "deleting"
	// SandboxStateDeleted is the "deleted" sandbox state value.
	SandboxStateDeleted SandboxState = "deleted"
)

// ServiceAccount is the wire representation of the service account schema.
type ServiceAccount struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// ProfileGrants carries the profileGrants JSON field.
	ProfileGrants []ProfileName `json:"profileGrants"`
	// ProjectID carries the projectId JSON field.
	ProjectID OpaqueID `json:"projectId"`
	// Revision carries the revision JSON field.
	Revision int64 `json:"revision"`
	// Scopes carries the scopes JSON field.
	Scopes []ServiceAccountScope `json:"scopes"`
	// State carries the state JSON field.
	State ServiceAccountState `json:"state"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// ServiceAccountPage is the wire representation of the service account page schema.
type ServiceAccountPage struct {
	// Items carries the items JSON field.
	Items []ServiceAccount `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// ServiceAccountScope is the wire representation of the service account scope schema.
type ServiceAccountScope string

const (
	// ServiceAccountScopeSandboxRead is the "sandbox:read" service account scope value.
	ServiceAccountScopeSandboxRead ServiceAccountScope = "sandbox:read"
	// ServiceAccountScopeSandboxLifecycle is the "sandbox:lifecycle" service account scope value.
	ServiceAccountScopeSandboxLifecycle ServiceAccountScope = "sandbox:lifecycle"
	// ServiceAccountScopeSandboxExec is the "sandbox:exec" service account scope value.
	ServiceAccountScopeSandboxExec ServiceAccountScope = "sandbox:exec"
	// ServiceAccountScopeSandboxFiles is the "sandbox:files" service account scope value.
	ServiceAccountScopeSandboxFiles ServiceAccountScope = "sandbox:files"
	// ServiceAccountScopeSandboxArtifacts is the "sandbox:artifacts" service account scope value.
	ServiceAccountScopeSandboxArtifacts ServiceAccountScope = "sandbox:artifacts"
	// ServiceAccountScopeSandboxPorts is the "sandbox:ports" service account scope value.
	ServiceAccountScopeSandboxPorts ServiceAccountScope = "sandbox:ports"
)

// ServiceAccountState is the wire representation of the service account state schema.
type ServiceAccountState string

const (
	// ServiceAccountStateActive is the "active" service account state value.
	ServiceAccountStateActive ServiceAccountState = "active"
	// ServiceAccountStateDisabled is the "disabled" service account state value.
	ServiceAccountStateDisabled ServiceAccountState = "disabled"
)

// SessionState is the wire representation of the session state schema.
type SessionState string

const (
	// SessionStateOpen is the "open" session state value.
	SessionStateOpen SessionState = "open"
	// SessionStateDetached is the "detached" session state value.
	SessionStateDetached SessionState = "detached"
	// SessionStateClosing is the "closing" session state value.
	SessionStateClosing SessionState = "closing"
	// SessionStateClosed is the "closed" session state value.
	SessionStateClosed SessionState = "closed"
)

// ShellCommand is the wire representation of the shell command schema.
type ShellCommand struct {
	// Command carries the command JSON field.
	Command string `json:"command"`
	// Mode carries the mode JSON field.
	Mode string `json:"mode"`
}

// Snapshot is the wire representation of the snapshot schema.
type Snapshot struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// SHA256 carries the sha256 JSON field.
	SHA256 string `json:"sha256"`
	// SizeBytes carries the sizeBytes JSON field.
	SizeBytes int64 `json:"sizeBytes"`
}

// SnapshotPage is the wire representation of the snapshot page schema.
type SnapshotPage struct {
	// Items carries the items JSON field.
	Items []Snapshot `json:"items"`
	// NextCursor carries the nextCursor JSON field.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// SpawnFailureKind is the wire representation of the spawn failure kind schema.
type SpawnFailureKind string

const (
	// SpawnFailureKindNotFound is the "not_found" spawn failure kind value.
	SpawnFailureKindNotFound SpawnFailureKind = "not_found"
	// SpawnFailureKindPermissionDenied is the "permission_denied" spawn failure kind value.
	SpawnFailureKindPermissionDenied SpawnFailureKind = "permission_denied"
	// SpawnFailureKindInvalidCwd is the "invalid_cwd" spawn failure kind value.
	SpawnFailureKindInvalidCwd SpawnFailureKind = "invalid_cwd"
	// SpawnFailureKindMalformedExecutable is the "malformed_executable" spawn failure kind value.
	SpawnFailureKindMalformedExecutable SpawnFailureKind = "malformed_executable"
)

// StreamCancelFrame is the wire representation of the stream cancel frame schema.
type StreamCancelFrame struct {
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamCreditFrame is the wire representation of the stream credit frame schema.
type StreamCreditFrame struct {
	// Bytes carries the bytes JSON field.
	Bytes int64 `json:"bytes"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamInputFrame is the wire representation of the stream input frame schema.
type StreamInputFrame struct {
	// DataBase64 Ordered process standard-input bytes; empty only when endOfInput is true.
	DataBase64 string `json:"dataBase64"`
	// EndOfInput Closes process standard input after dataBase64; subsequent stdin frames are invalid.
	EndOfInput bool `json:"endOfInput"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamOutcomeFrame is the wire representation of the stream outcome frame schema.
type StreamOutcomeFrame struct {
	// Outcome carries the outcome JSON field.
	Outcome ExecOutcome `json:"outcome"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamOutputFrame is the wire representation of the stream output frame schema.
type StreamOutputFrame struct {
	// DataBase64 carries the dataBase64 JSON field.
	DataBase64 string `json:"dataBase64"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Stream carries the stream JSON field.
	Stream string `json:"stream"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamSignalFrame is the wire representation of the stream signal frame schema.
type StreamSignalFrame struct {
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Signal carries the signal JSON field.
	Signal int `json:"signal"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// StreamingExecRequest is the wire representation of the streaming exec request schema.
type StreamingExecRequest struct {
	// Command carries the command JSON field.
	Command Command `json:"command"`
	// Cwd carries the cwd JSON field.
	Cwd *WorkspacePath `json:"cwd,omitempty"`
	// DeadlineMilliseconds carries the deadlineMilliseconds JSON field.
	DeadlineMilliseconds int64 `json:"deadlineMilliseconds"`
	// Environment carries the environment JSON field.
	Environment StringMap `json:"environment"`
	// MaximumOutputBytes carries the maximumOutputBytes JSON field.
	MaximumOutputBytes int64 `json:"maximumOutputBytes"`
	// WindowBytes carries the windowBytes JSON field.
	WindowBytes int64 `json:"windowBytes"`
}

// StringMap is the wire representation of the string map schema.
type StringMap map[string]string

// TerminalFrame is the wire representation of the terminal frame schema.
type TerminalFrame struct {
	// TerminalInputFrame contains the terminal input frame variant when selected.
	TerminalInputFrame *TerminalInputFrame `json:"-"`
	// TerminalOutputFrame contains the terminal output frame variant when selected.
	TerminalOutputFrame *TerminalOutputFrame `json:"-"`
	// TerminalResizeFrame contains the terminal resize frame variant when selected.
	TerminalResizeFrame *TerminalResizeFrame `json:"-"`
	// StreamCreditFrame contains the stream credit frame variant when selected.
	StreamCreditFrame *StreamCreditFrame `json:"-"`
	// StreamCancelFrame contains the stream cancel frame variant when selected.
	StreamCancelFrame *StreamCancelFrame `json:"-"`
	// StreamOutcomeFrame contains the stream outcome frame variant when selected.
	StreamOutcomeFrame *StreamOutcomeFrame `json:"-"`
}

// MarshalJSON encodes exactly one selected TerminalFrame variant.
func (value TerminalFrame) MarshalJSON() ([]byte, error) {
	selected := 0
	var encoded []byte
	var encodeErr error
	if value.TerminalInputFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.TerminalInputFrame)
	}
	if value.TerminalOutputFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.TerminalOutputFrame)
	}
	if value.TerminalResizeFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.TerminalResizeFrame)
	}
	if value.StreamCreditFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamCreditFrame)
	}
	if value.StreamCancelFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamCancelFrame)
	}
	if value.StreamOutcomeFrame != nil {
		selected++
		encoded, encodeErr = json.Marshal(value.StreamOutcomeFrame)
	}
	if encodeErr != nil {
		return nil, fmt.Errorf("SecondBox TerminalFrame encode variant: %w", encodeErr)
	}
	if selected != 1 {
		return nil, fmt.Errorf("SecondBox TerminalFrame requires exactly one variant, found %d", selected)
	}
	return encoded, nil
}

// UnmarshalJSON decodes a TerminalFrame by its "type" discriminator.
func (value *TerminalFrame) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Value string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return fmt.Errorf("SecondBox TerminalFrame decode discriminator: %w", err)
	}
	switch discriminator.Value {
	case "terminal_input":
		var decoded TerminalInputFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode TerminalInputFrame: %w", err)
		}
		*value = TerminalFrame{TerminalInputFrame: &decoded}
		return nil
	case "terminal_output":
		var decoded TerminalOutputFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode TerminalOutputFrame: %w", err)
		}
		*value = TerminalFrame{TerminalOutputFrame: &decoded}
		return nil
	case "resize":
		var decoded TerminalResizeFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode TerminalResizeFrame: %w", err)
		}
		*value = TerminalFrame{TerminalResizeFrame: &decoded}
		return nil
	case "credit":
		var decoded StreamCreditFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode StreamCreditFrame: %w", err)
		}
		*value = TerminalFrame{StreamCreditFrame: &decoded}
		return nil
	case "cancel":
		var decoded StreamCancelFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode StreamCancelFrame: %w", err)
		}
		*value = TerminalFrame{StreamCancelFrame: &decoded}
		return nil
	case "outcome":
		var decoded StreamOutcomeFrame
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("SecondBox TerminalFrame decode StreamOutcomeFrame: %w", err)
		}
		*value = TerminalFrame{StreamOutcomeFrame: &decoded}
		return nil
	default:
		return fmt.Errorf("SecondBox TerminalFrame has unsupported discriminator %q", discriminator.Value)
	}
}

// TerminalInputFrame is the wire representation of the terminal input frame schema.
type TerminalInputFrame struct {
	// DataBase64 carries the dataBase64 JSON field.
	DataBase64 string `json:"dataBase64"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// TerminalOutputFrame is the wire representation of the terminal output frame schema.
type TerminalOutputFrame struct {
	// DataBase64 carries the dataBase64 JSON field.
	DataBase64 string `json:"dataBase64"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// TerminalResizeFrame is the wire representation of the terminal resize frame schema.
type TerminalResizeFrame struct {
	// Columns carries the columns JSON field.
	Columns int `json:"columns"`
	// Rows carries the rows JSON field.
	Rows int `json:"rows"`
	// Sequence carries the sequence JSON field.
	Sequence int64 `json:"sequence"`
	// Type carries the type JSON field.
	Type string `json:"type"`
}

// TerminalSession is the wire representation of the terminal session schema.
type TerminalSession struct {
	// ExpiresAt carries the expiresAt JSON field.
	ExpiresAt Timestamp `json:"expiresAt"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// NextClientSequence carries the nextClientSequence JSON field.
	NextClientSequence int64 `json:"nextClientSequence"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
	// State carries the state JSON field.
	State SessionState `json:"state"`
	// Subprotocol carries the subprotocol JSON field.
	Subprotocol string `json:"subprotocol"`
	// WebsocketURL carries the websocketUrl JSON field.
	WebsocketURL string `json:"websocketUrl"`
}

// Timestamp is the wire representation of the timestamp schema.
type Timestamp string

// TouchResult is the wire representation of the touch result schema.
type TouchResult struct {
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// LastActivityAt carries the lastActivityAt JSON field.
	LastActivityAt Timestamp `json:"lastActivityAt"`
	// SandboxID carries the sandboxId JSON field.
	SandboxID OpaqueID `json:"sandboxId"`
}

// UpdateProjectRequest is the wire representation of the update project request schema.
type UpdateProjectRequest struct {
	// Name carries the name JSON field.
	Name *string `json:"name,omitempty"`
	// State carries the state JSON field.
	State *ProjectState `json:"state,omitempty"`
}

// UpdateRunnerPoolRequest is the wire representation of the update runner pool request schema.
type UpdateRunnerPoolRequest struct {
	// Architectures carries the architectures JSON field.
	Architectures *RunnerArchitectureList `json:"architectures,omitempty"`
	// Capabilities carries the capabilities JSON field.
	Capabilities *RunnerCapabilityList `json:"capabilities,omitempty"`
	// CapacityPolicy carries the capacityPolicy JSON field.
	CapacityPolicy *RunnerCapacityPolicy `json:"capacityPolicy,omitempty"`
	// State carries the state JSON field.
	State *RunnerPoolState `json:"state,omitempty"`
}

// UpdateServiceAccountRequest is the wire representation of the update service account request schema.
type UpdateServiceAccountRequest struct {
	// Name carries the name JSON field.
	Name *string `json:"name,omitempty"`
	// ProfileGrants carries the profileGrants JSON field.
	ProfileGrants *[]ProfileName `json:"profileGrants,omitempty"`
	// Scopes carries the scopes JSON field.
	Scopes *[]ServiceAccountScope `json:"scopes,omitempty"`
	// State carries the state JSON field.
	State *ServiceAccountState `json:"state,omitempty"`
}

// UploadArtifactRequest is the wire representation of the upload artifact request schema.
type UploadArtifactRequest struct {
	// Content carries the content JSON field.
	Content string `json:"content"`
	// MediaType carries the mediaType JSON field.
	MediaType string `json:"mediaType"`
	// Metadata carries the metadata JSON field.
	Metadata Metadata `json:"metadata"`
	// Name carries the name JSON field.
	Name string `json:"name"`
	// SHA256 carries the sha256 JSON field.
	SHA256 string `json:"sha256"`
}

// WaitSandboxRequest is the wire representation of the wait sandbox request schema.
type WaitSandboxRequest struct {
	// DeadlineMilliseconds carries the deadlineMilliseconds JSON field.
	DeadlineMilliseconds int `json:"deadlineMilliseconds"`
	// States carries the states JSON field.
	States []SandboxState `json:"states"`
}

// Workspace is the wire representation of the workspace schema.
type Workspace struct {
	// CreatedAt carries the createdAt JSON field.
	CreatedAt Timestamp `json:"createdAt"`
	// CurrentSnapshotID carries the currentSnapshotId JSON field.
	CurrentSnapshotID *OpaqueID `json:"currentSnapshotId,omitempty"`
	// Generation carries the generation JSON field.
	Generation int64 `json:"generation"`
	// ID carries the id JSON field.
	ID OpaqueID `json:"id"`
	// RetainUntil carries the retainUntil JSON field.
	RetainUntil *Timestamp `json:"retainUntil,omitempty"`
	// RetainedBytes carries the retainedBytes JSON field.
	RetainedBytes int64 `json:"retainedBytes"`
	// UpdatedAt carries the updatedAt JSON field.
	UpdatedAt Timestamp `json:"updatedAt"`
}

// WorkspacePath is the wire representation of the workspace path schema.
type WorkspacePath string

// OperationParameter describes one canonical path, query, or header parameter.
type OperationParameter struct {
	// Name is the parameter's exact wire name.
	Name string
	// Location is path, query, or header.
	Location string
	// Required reports whether the contract requires the parameter.
	Required bool
	// Schema is the component schema name or primitive wire type.
	Schema string
}

// OperationMediaType describes one request body representation.
type OperationMediaType struct {
	// ContentType is the exact HTTP media type.
	ContentType string
	// Schema is the component schema name or primitive wire type.
	Schema string
}

// OperationResponse describes one declared response representation.
type OperationResponse struct {
	// StatusCode is the OpenAPI response status or default.
	StatusCode string
	// ContentType is empty for responses without a body.
	ContentType string
	// Schema is empty for responses without a body.
	Schema string
}

// OperationMetadata is the canonical transport description for one OpenAPI operation.
type OperationMetadata struct {
	// OperationID is the stable OpenAPI operationId.
	OperationID string
	// Method is the uppercase HTTP method.
	Method string
	// PathTemplate is the versioned path with named placeholders.
	PathTemplate string
	// Parameters lists the operation's path, query, and header inputs.
	Parameters []OperationParameter
	// RequestBody lists every accepted request representation.
	RequestBody []OperationMediaType
	// RequestBodyRequired reports whether the operation requires a body.
	RequestBodyRequired bool
	// Responses lists every declared status and representation.
	Responses []OperationResponse
}

// AcquireSandboxLeaseOperation describes the acquireSandboxLease OpenAPI operation.
var AcquireSandboxLeaseOperation = OperationMetadata{
	OperationID:  "acquireSandboxLease",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/leases",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "AcquireLeaseRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "Lease"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CancelSandboxExecStreamOperation describes the cancelSandboxExecStream OpenAPI operation.
var CancelSandboxExecStreamOperation = OperationMetadata{
	OperationID:  "cancelSandboxExecStream",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/exec-streams/{execSessionId}:cancel",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "execSessionId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "ExecStreamSession"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CancelSandboxTerminalOperation describes the cancelSandboxTerminal OpenAPI operation.
var CancelSandboxTerminalOperation = OperationMetadata{
	OperationID:  "cancelSandboxTerminal",
	Method:       "DELETE",
	PathTemplate: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "terminalSessionId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "TerminalSession"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CheckpointSandboxOperation describes the checkpointSandbox OpenAPI operation.
var CheckpointSandboxOperation = OperationMetadata{
	OperationID:  "checkpointSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:checkpoint",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CheckpointSandboxRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// CloseSandboxPortSessionOperation describes the closeSandboxPortSession OpenAPI operation.
var CloseSandboxPortSessionOperation = OperationMetadata{
	OperationID:  "closeSandboxPortSession",
	Method:       "DELETE",
	PathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "portSessionId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "204", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// CreateAPIKeyOperation describes the createAPIKey OpenAPI operation.
var CreateAPIKeyOperation = OperationMetadata{
	OperationID:  "createAPIKey",
	Method:       "POST",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateAPIKeyRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "CreateAPIKeyResponse"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CreateProfileOperation describes the createProfile OpenAPI operation.
var CreateProfileOperation = OperationMetadata{
	OperationID:  "createProfile",
	Method:       "POST",
	PathTemplate: "/v1/profiles",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateProfileRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "Profile"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CreateProjectOperation describes the createProject OpenAPI operation.
var CreateProjectOperation = OperationMetadata{
	OperationID:  "createProject",
	Method:       "POST",
	PathTemplate: "/v1/projects",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateProjectRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "Project"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CreateRunnerPoolOperation describes the createRunnerPool OpenAPI operation.
var CreateRunnerPoolOperation = OperationMetadata{
	OperationID:  "createRunnerPool",
	Method:       "POST",
	PathTemplate: "/v1/runner-pools",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateRunnerPoolRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "RunnerPool"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CreateSandboxOperation describes the createSandbox OpenAPI operation.
var CreateSandboxOperation = OperationMetadata{
	OperationID:  "createSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateSandboxRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// CreateSandboxDirectoryOperation describes the createSandboxDirectory OpenAPI operation.
var CreateSandboxDirectoryOperation = OperationMetadata{
	OperationID:  "createSandboxDirectory",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/directories",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateDirectoryRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "204", ContentType: "", Schema: ""},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// CreateSandboxExecStreamOperation describes the createSandboxExecStream OpenAPI operation.
var CreateSandboxExecStreamOperation = OperationMetadata{
	OperationID:  "createSandboxExecStream",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/exec-streams",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "StreamingExecRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "ExecStreamSession"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// CreateSandboxPortSessionOperation describes the createSandboxPortSession OpenAPI operation.
var CreateSandboxPortSessionOperation = OperationMetadata{
	OperationID:  "createSandboxPortSession",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreatePortSessionRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "PortSession"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// CreateSandboxSnapshotOperation describes the createSandboxSnapshot OpenAPI operation.
var CreateSandboxSnapshotOperation = OperationMetadata{
	OperationID:  "createSandboxSnapshot",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/snapshots",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateSnapshotRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "Snapshot"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// CreateSandboxTerminalOperation describes the createSandboxTerminal OpenAPI operation.
var CreateSandboxTerminalOperation = OperationMetadata{
	OperationID:  "createSandboxTerminal",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/terminals",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateTerminalRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "TerminalSession"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// CreateServiceAccountOperation describes the createServiceAccount OpenAPI operation.
var CreateServiceAccountOperation = OperationMetadata{
	OperationID:  "createServiceAccount",
	Method:       "POST",
	PathTemplate: "/v1/projects/{projectId}/service-accounts",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "CreateServiceAccountRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "ServiceAccount"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// DeleteArtifactOperation describes the deleteArtifact OpenAPI operation.
var DeleteArtifactOperation = OperationMetadata{
	OperationID:  "deleteArtifact",
	Method:       "DELETE",
	PathTemplate: "/v1/artifacts/{artifactId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "artifactId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "204", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// DeleteSandboxOperation describes the deleteSandbox OpenAPI operation.
var DeleteSandboxOperation = OperationMetadata{
	OperationID:  "deleteSandbox",
	Method:       "DELETE",
	PathTemplate: "/v1/sandboxes/{sandboxId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// DeleteSnapshotOperation describes the deleteSnapshot OpenAPI operation.
var DeleteSnapshotOperation = OperationMetadata{
	OperationID:  "deleteSnapshot",
	Method:       "DELETE",
	PathTemplate: "/v1/snapshots/{snapshotId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "snapshotId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "204", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// DisableProfileOperation describes the disableProfile OpenAPI operation.
var DisableProfileOperation = OperationMetadata{
	OperationID:  "disableProfile",
	Method:       "POST",
	PathTemplate: "/v1/profiles/{profileName}:disable",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "profileName", Location: "path", Required: true, Schema: "ProfileName"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Profile"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// DownloadArtifactContentOperation describes the downloadArtifactContent OpenAPI operation.
var DownloadArtifactContentOperation = OperationMetadata{
	OperationID:  "downloadArtifactContent",
	Method:       "GET",
	PathTemplate: "/v1/artifacts/{artifactId}/content",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "artifactId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/octet-stream", Schema: "string"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// DrainSandboxOperation describes the drainSandbox OpenAPI operation.
var DrainSandboxOperation = OperationMetadata{
	OperationID:  "drainSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:drain",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// ExecuteSandboxCommandOperation describes the executeSandboxCommand OpenAPI operation.
var ExecuteSandboxCommandOperation = OperationMetadata{
	OperationID:  "executeSandboxCommand",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/exec",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "BufferedExecRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ExecOutcome"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "413", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// GetArtifactOperation describes the getArtifact OpenAPI operation.
var GetArtifactOperation = OperationMetadata{
	OperationID:  "getArtifact",
	Method:       "GET",
	PathTemplate: "/v1/artifacts/{artifactId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "artifactId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Artifact"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetOperationOperation describes the getOperation OpenAPI operation.
var GetOperationOperation = OperationMetadata{
	OperationID:  "getOperation",
	Method:       "GET",
	PathTemplate: "/v1/operations/{operationId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "operationId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetProfileOperation describes the getProfile OpenAPI operation.
var GetProfileOperation = OperationMetadata{
	OperationID:  "getProfile",
	Method:       "GET",
	PathTemplate: "/v1/profiles/{profileName}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "profileName", Location: "path", Required: true, Schema: "ProfileName"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Profile"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetProjectOperation describes the getProject OpenAPI operation.
var GetProjectOperation = OperationMetadata{
	OperationID:  "getProject",
	Method:       "GET",
	PathTemplate: "/v1/projects/{projectId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Project"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetRunnerOperation describes the getRunner OpenAPI operation.
var GetRunnerOperation = OperationMetadata{
	OperationID:  "getRunner",
	Method:       "GET",
	PathTemplate: "/v1/runners/{runnerId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "runnerId", Location: "path", Required: true, Schema: "RunnerID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Runner"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetRunnerPoolOperation describes the getRunnerPool OpenAPI operation.
var GetRunnerPoolOperation = OperationMetadata{
	OperationID:  "getRunnerPool",
	Method:       "GET",
	PathTemplate: "/v1/runner-pools/{runnerPoolName}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "runnerPoolName", Location: "path", Required: true, Schema: "ProfileName"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "RunnerPool"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetSandboxOperation describes the getSandbox OpenAPI operation.
var GetSandboxOperation = OperationMetadata{
	OperationID:  "getSandbox",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Sandbox"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetSandboxLeaseOperation describes the getSandboxLease OpenAPI operation.
var GetSandboxLeaseOperation = OperationMetadata{
	OperationID:  "getSandboxLease",
	Method:       "GET",
	PathTemplate: "/v1/leases/{leaseId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "leaseId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Lease"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetSandboxPortSessionOperation describes the getSandboxPortSession OpenAPI operation.
var GetSandboxPortSessionOperation = OperationMetadata{
	OperationID:  "getSandboxPortSession",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "portSessionId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "PortSession"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetServiceAccountOperation describes the getServiceAccount OpenAPI operation.
var GetServiceAccountOperation = OperationMetadata{
	OperationID:  "getServiceAccount",
	Method:       "GET",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ServiceAccount"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// GetSnapshotOperation describes the getSnapshot OpenAPI operation.
var GetSnapshotOperation = OperationMetadata{
	OperationID:  "getSnapshot",
	Method:       "GET",
	PathTemplate: "/v1/snapshots/{snapshotId}",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "snapshotId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Snapshot"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// InspectSandboxOperation describes the inspectSandbox OpenAPI operation.
var InspectSandboxOperation = OperationMetadata{
	OperationID:  "inspectSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:inspect",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "SandboxInspection"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// ListAPIKeysOperation describes the listAPIKeys OpenAPI operation.
var ListAPIKeysOperation = OperationMetadata{
	OperationID:  "listAPIKeys",
	Method:       "GET",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "APIKeyPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// ListProfilesOperation describes the listProfiles OpenAPI operation.
var ListProfilesOperation = OperationMetadata{
	OperationID:  "listProfiles",
	Method:       "GET",
	PathTemplate: "/v1/profiles",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ProfilePage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
	},
}

// ListProjectsOperation describes the listProjects OpenAPI operation.
var ListProjectsOperation = OperationMetadata{
	OperationID:  "listProjects",
	Method:       "GET",
	PathTemplate: "/v1/projects",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ProjectPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
	},
}

// ListRunnerPoolsOperation describes the listRunnerPools OpenAPI operation.
var ListRunnerPoolsOperation = OperationMetadata{
	OperationID:  "listRunnerPools",
	Method:       "GET",
	PathTemplate: "/v1/runner-pools",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "RunnerPoolPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
	},
}

// ListRunnersOperation describes the listRunners OpenAPI operation.
var ListRunnersOperation = OperationMetadata{
	OperationID:  "listRunners",
	Method:       "GET",
	PathTemplate: "/v1/runners",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
		{Name: "pool", Location: "query", Required: false, Schema: "ProfileName"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "RunnerPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
	},
}

// ListSandboxArtifactsOperation describes the listSandboxArtifacts OpenAPI operation.
var ListSandboxArtifactsOperation = OperationMetadata{
	OperationID:  "listSandboxArtifacts",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/artifacts",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ArtifactPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// ListSandboxDirectoryOperation describes the listSandboxDirectory OpenAPI operation.
var ListSandboxDirectoryOperation = OperationMetadata{
	OperationID:  "listSandboxDirectory",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/directories",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "path", Location: "query", Required: true, Schema: "WorkspacePath"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "DirectoryListing"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// ListSandboxSnapshotsOperation describes the listSandboxSnapshots OpenAPI operation.
var ListSandboxSnapshotsOperation = OperationMetadata{
	OperationID:  "listSandboxSnapshots",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/snapshots",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "SnapshotPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// ListSandboxesOperation describes the listSandboxes OpenAPI operation.
var ListSandboxesOperation = OperationMetadata{
	OperationID:  "listSandboxes",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "SandboxPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
	},
}

// ListServiceAccountsOperation describes the listServiceAccounts OpenAPI operation.
var ListServiceAccountsOperation = OperationMetadata{
	OperationID:  "listServiceAccounts",
	Method:       "GET",
	PathTemplate: "/v1/projects/{projectId}/service-accounts",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "cursor", Location: "query", Required: false, Schema: "string"},
		{Name: "limit", Location: "query", Required: false, Schema: "integer"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ServiceAccountPage"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// PingSandboxOperation describes the pingSandbox OpenAPI operation.
var PingSandboxOperation = OperationMetadata{
	OperationID:  "pingSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:ping",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "PingResult"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// ReadSandboxFileOperation describes the readSandboxFile OpenAPI operation.
var ReadSandboxFileOperation = OperationMetadata{
	OperationID:  "readSandboxFile",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/files",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "path", Location: "query", Required: true, Schema: "WorkspacePath"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/octet-stream", Schema: "string"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "413", ContentType: "", Schema: ""},
	},
}

// ReconnectSandboxTerminalOperation describes the reconnectSandboxTerminal OpenAPI operation.
var ReconnectSandboxTerminalOperation = OperationMetadata{
	OperationID:  "reconnectSandboxTerminal",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "terminalSessionId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "TerminalSession"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// ReleaseSandboxLeaseOperation describes the releaseSandboxLease OpenAPI operation.
var ReleaseSandboxLeaseOperation = OperationMetadata{
	OperationID:  "releaseSandboxLease",
	Method:       "DELETE",
	PathTemplate: "/v1/leases/{leaseId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "leaseId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Lease"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
	},
}

// RemoveSandboxPathOperation describes the removeSandboxPath OpenAPI operation.
var RemoveSandboxPathOperation = OperationMetadata{
	OperationID:  "removeSandboxPath",
	Method:       "DELETE",
	PathTemplate: "/v1/sandboxes/{sandboxId}/directories",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "RemovePathRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "204", ContentType: "", Schema: ""},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// RenewSandboxLeaseOperation describes the renewSandboxLease OpenAPI operation.
var RenewSandboxLeaseOperation = OperationMetadata{
	OperationID:  "renewSandboxLease",
	Method:       "POST",
	PathTemplate: "/v1/leases/{leaseId}:renew",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "leaseId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "RenewLeaseRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Lease"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// ReviseProfileOperation describes the reviseProfile OpenAPI operation.
var ReviseProfileOperation = OperationMetadata{
	OperationID:  "reviseProfile",
	Method:       "POST",
	PathTemplate: "/v1/profiles/{profileName}:revise",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "profileName", Location: "path", Required: true, Schema: "ProfileName"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "ReviseProfileRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Profile"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// RevokeAPIKeyOperation describes the revokeAPIKey OpenAPI operation.
var RevokeAPIKeyOperation = OperationMetadata{
	OperationID:  "revokeAPIKey",
	Method:       "POST",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys/{apiKeyId}:revoke",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "apiKeyId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "APIKey"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// RotateAPIKeyOperation describes the rotateAPIKey OpenAPI operation.
var RotateAPIKeyOperation = OperationMetadata{
	OperationID:  "rotateAPIKey",
	Method:       "POST",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys/{apiKeyId}:rotate",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "apiKeyId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "CreateAPIKeyResponse"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// SandboxFileExistsOperation describes the sandboxFileExists OpenAPI operation.
var SandboxFileExistsOperation = OperationMetadata{
	OperationID:  "sandboxFileExists",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/files:exists",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "path", Location: "query", Required: true, Schema: "WorkspacePath"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "FileExistsResult"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// StartSandboxOperation describes the startSandbox OpenAPI operation.
var StartSandboxOperation = OperationMetadata{
	OperationID:  "startSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:start",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
		{StatusCode: "429", ContentType: "", Schema: ""},
	},
}

// StatSandboxFileOperation describes the statSandboxFile OpenAPI operation.
var StatSandboxFileOperation = OperationMetadata{
	OperationID:  "statSandboxFile",
	Method:       "GET",
	PathTemplate: "/v1/sandboxes/{sandboxId}/files:stat",
	Parameters: []OperationParameter{
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "path", Location: "query", Required: true, Schema: "WorkspacePath"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "FileStat"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// StopSandboxOperation describes the stopSandbox OpenAPI operation.
var StopSandboxOperation = OperationMetadata{
	OperationID:  "stopSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:stop",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "202", ContentType: "application/json", Schema: "Operation"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// TouchSandboxOperation describes the touchSandbox OpenAPI operation.
var TouchSandboxOperation = OperationMetadata{
	OperationID:  "touchSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:touch",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBodyRequired: false,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "TouchResult"},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
	},
}

// UpdateProjectOperation describes the updateProject OpenAPI operation.
var UpdateProjectOperation = OperationMetadata{
	OperationID:  "updateProject",
	Method:       "PATCH",
	PathTemplate: "/v1/projects/{projectId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "UpdateProjectRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Project"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// UpdateRunnerPoolOperation describes the updateRunnerPool OpenAPI operation.
var UpdateRunnerPoolOperation = OperationMetadata{
	OperationID:  "updateRunnerPool",
	Method:       "PATCH",
	PathTemplate: "/v1/runner-pools/{runnerPoolName}",
	Parameters: []OperationParameter{
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "runnerPoolName", Location: "path", Required: true, Schema: "ProfileName"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "UpdateRunnerPoolRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "RunnerPool"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// UpdateServiceAccountOperation describes the updateServiceAccount OpenAPI operation.
var UpdateServiceAccountOperation = OperationMetadata{
	OperationID:  "updateServiceAccount",
	Method:       "PATCH",
	PathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "If-Match", Location: "header", Required: true, Schema: "string"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "projectId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "serviceAccountId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "UpdateServiceAccountRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "ServiceAccount"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "412", ContentType: "", Schema: ""},
	},
}

// UploadSandboxArtifactOperation describes the uploadSandboxArtifact OpenAPI operation.
var UploadSandboxArtifactOperation = OperationMetadata{
	OperationID:  "uploadSandboxArtifact",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}/artifacts",
	Parameters: []OperationParameter{
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "multipart/form-data", Schema: "UploadArtifactRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "201", ContentType: "application/json", Schema: "Artifact"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "413", ContentType: "", Schema: ""},
	},
}

// WaitForSandboxOperation describes the waitForSandbox OpenAPI operation.
var WaitForSandboxOperation = OperationMetadata{
	OperationID:  "waitForSandbox",
	Method:       "POST",
	PathTemplate: "/v1/sandboxes/{sandboxId}:wait",
	Parameters: []OperationParameter{
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/json", Schema: "WaitSandboxRequest"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "Sandbox"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "408", ContentType: "", Schema: ""},
	},
}

// WriteSandboxFileOperation describes the writeSandboxFile OpenAPI operation.
var WriteSandboxFileOperation = OperationMetadata{
	OperationID:  "writeSandboxFile",
	Method:       "PUT",
	PathTemplate: "/v1/sandboxes/{sandboxId}/files",
	Parameters: []OperationParameter{
		{Name: "Digest", Location: "header", Required: true, Schema: "string"},
		{Name: "Idempotency-Key", Location: "header", Required: true, Schema: "string"},
		{Name: "SecondBox-Generation", Location: "header", Required: true, Schema: "integer"},
		{Name: "SecondBox-Lease-ID", Location: "header", Required: false, Schema: "OpaqueID"},
		{Name: "X-Request-ID", Location: "header", Required: false, Schema: "CorrelationID"},
		{Name: "sandboxId", Location: "path", Required: true, Schema: "OpaqueID"},
		{Name: "path", Location: "query", Required: true, Schema: "WorkspacePath"},
	},
	RequestBody: []OperationMediaType{
		{ContentType: "application/octet-stream", Schema: "string"},
	},
	RequestBodyRequired: true,
	Responses: []OperationResponse{
		{StatusCode: "200", ContentType: "application/json", Schema: "FileWriteResult"},
		{StatusCode: "400", ContentType: "", Schema: ""},
		{StatusCode: "401", ContentType: "", Schema: ""},
		{StatusCode: "403", ContentType: "", Schema: ""},
		{StatusCode: "404", ContentType: "", Schema: ""},
		{StatusCode: "409", ContentType: "", Schema: ""},
		{StatusCode: "413", ContentType: "", Schema: ""},
	},
}

// LookupOperation returns immutable-by-value metadata for a stable OpenAPI operationId.
func LookupOperation(operationID string) (OperationMetadata, bool) {
	switch operationID {
	case "acquireSandboxLease":
		return AcquireSandboxLeaseOperation, true
	case "cancelSandboxExecStream":
		return CancelSandboxExecStreamOperation, true
	case "cancelSandboxTerminal":
		return CancelSandboxTerminalOperation, true
	case "checkpointSandbox":
		return CheckpointSandboxOperation, true
	case "closeSandboxPortSession":
		return CloseSandboxPortSessionOperation, true
	case "createAPIKey":
		return CreateAPIKeyOperation, true
	case "createProfile":
		return CreateProfileOperation, true
	case "createProject":
		return CreateProjectOperation, true
	case "createRunnerPool":
		return CreateRunnerPoolOperation, true
	case "createSandbox":
		return CreateSandboxOperation, true
	case "createSandboxDirectory":
		return CreateSandboxDirectoryOperation, true
	case "createSandboxExecStream":
		return CreateSandboxExecStreamOperation, true
	case "createSandboxPortSession":
		return CreateSandboxPortSessionOperation, true
	case "createSandboxSnapshot":
		return CreateSandboxSnapshotOperation, true
	case "createSandboxTerminal":
		return CreateSandboxTerminalOperation, true
	case "createServiceAccount":
		return CreateServiceAccountOperation, true
	case "deleteArtifact":
		return DeleteArtifactOperation, true
	case "deleteSandbox":
		return DeleteSandboxOperation, true
	case "deleteSnapshot":
		return DeleteSnapshotOperation, true
	case "disableProfile":
		return DisableProfileOperation, true
	case "downloadArtifactContent":
		return DownloadArtifactContentOperation, true
	case "drainSandbox":
		return DrainSandboxOperation, true
	case "executeSandboxCommand":
		return ExecuteSandboxCommandOperation, true
	case "getArtifact":
		return GetArtifactOperation, true
	case "getOperation":
		return GetOperationOperation, true
	case "getProfile":
		return GetProfileOperation, true
	case "getProject":
		return GetProjectOperation, true
	case "getRunner":
		return GetRunnerOperation, true
	case "getRunnerPool":
		return GetRunnerPoolOperation, true
	case "getSandbox":
		return GetSandboxOperation, true
	case "getSandboxLease":
		return GetSandboxLeaseOperation, true
	case "getSandboxPortSession":
		return GetSandboxPortSessionOperation, true
	case "getServiceAccount":
		return GetServiceAccountOperation, true
	case "getSnapshot":
		return GetSnapshotOperation, true
	case "inspectSandbox":
		return InspectSandboxOperation, true
	case "listAPIKeys":
		return ListAPIKeysOperation, true
	case "listProfiles":
		return ListProfilesOperation, true
	case "listProjects":
		return ListProjectsOperation, true
	case "listRunnerPools":
		return ListRunnerPoolsOperation, true
	case "listRunners":
		return ListRunnersOperation, true
	case "listSandboxArtifacts":
		return ListSandboxArtifactsOperation, true
	case "listSandboxDirectory":
		return ListSandboxDirectoryOperation, true
	case "listSandboxSnapshots":
		return ListSandboxSnapshotsOperation, true
	case "listSandboxes":
		return ListSandboxesOperation, true
	case "listServiceAccounts":
		return ListServiceAccountsOperation, true
	case "pingSandbox":
		return PingSandboxOperation, true
	case "readSandboxFile":
		return ReadSandboxFileOperation, true
	case "reconnectSandboxTerminal":
		return ReconnectSandboxTerminalOperation, true
	case "releaseSandboxLease":
		return ReleaseSandboxLeaseOperation, true
	case "removeSandboxPath":
		return RemoveSandboxPathOperation, true
	case "renewSandboxLease":
		return RenewSandboxLeaseOperation, true
	case "reviseProfile":
		return ReviseProfileOperation, true
	case "revokeAPIKey":
		return RevokeAPIKeyOperation, true
	case "rotateAPIKey":
		return RotateAPIKeyOperation, true
	case "sandboxFileExists":
		return SandboxFileExistsOperation, true
	case "startSandbox":
		return StartSandboxOperation, true
	case "statSandboxFile":
		return StatSandboxFileOperation, true
	case "stopSandbox":
		return StopSandboxOperation, true
	case "touchSandbox":
		return TouchSandboxOperation, true
	case "updateProject":
		return UpdateProjectOperation, true
	case "updateRunnerPool":
		return UpdateRunnerPoolOperation, true
	case "updateServiceAccount":
		return UpdateServiceAccountOperation, true
	case "uploadSandboxArtifact":
		return UploadSandboxArtifactOperation, true
	case "waitForSandbox":
		return WaitForSandboxOperation, true
	case "writeSandboxFile":
		return WriteSandboxFileOperation, true
	default:
		return OperationMetadata{}, false
	}
}

// RequestOptions supplies wire values for a generated operation.
type RequestOptions struct {
	// PathParameters replaces named placeholders in OperationMetadata.PathTemplate.
	PathParameters map[string]string
	// QueryParameters supplies already typed query values.
	QueryParameters url.Values
	// Headers supplies operation headers such as Idempotency-Key and If-Match.
	Headers http.Header
	// Body is the encoded request body, if any.
	Body io.Reader
	// ContentType selects one declared request media type.
	ContentType string
}

// APIError is a non-successful SecondBox HTTP response with its structured problem when available.
type APIError struct {
	// StatusCode is the HTTP response status.
	StatusCode int
	// Problem is the RFC 9457-compatible SecondBox problem body when decoding succeeded.
	Problem *Problem
	// Body contains the bounded raw error body.
	Body []byte
}

// Error returns a greppable SecondBox transport error.
func (failure *APIError) Error() string {
	if failure.Problem != nil {
		return fmt.Sprintf("SecondBox API request failed: status=%d code=%s title=%s", failure.StatusCode, failure.Problem.Code, failure.Problem.Title)
	}
	return fmt.Sprintf("SecondBox API request failed: status=%d", failure.StatusCode)
}

// Client is the dependency-free HTTP transport for generated SecondBox operations.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

// NewSecondBoxClient validates transport dependencies without inventing lifecycle behavior.
func NewSecondBoxClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("SecondBox client URL must be an absolute HTTP endpoint without query or fragment")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("SecondBox client URL scheme must be http or https")
	}
	if token == "" {
		return nil, errors.New("SecondBox client service-account token is required")
	}
	if httpClient == nil {
		return nil, errors.New("SecondBox client HTTP client is required")
	}
	return &Client{baseURL: baseURL, token: token, httpClient: httpClient}, nil
}

// Do sends one generated operation and leaves successful response decoding to the typed caller.
func (client *Client) Do(ctx context.Context, operation OperationMetadata, options RequestOptions) (*http.Response, error) {
	path := operation.PathTemplate
	for _, parameter := range operation.Parameters {
		if parameter.Location != "path" {
			continue
		}
		value, exists := options.PathParameters[parameter.Name]
		if parameter.Required && (!exists || value == "") {
			return nil, fmt.Errorf("SecondBox client missing required path parameter %q for %s", parameter.Name, operation.OperationID)
		}
		path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(value))
	}
	if strings.Contains(path, "{") {
		return nil, fmt.Errorf("SecondBox client has unresolved path template %q for %s", path, operation.OperationID)
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: path})
	endpoint.RawQuery = options.QueryParameters.Encode()

	contentType := options.ContentType
	if options.Body != nil && contentType == "" && len(operation.RequestBody) == 1 {
		contentType = operation.RequestBody[0].ContentType
	}
	if contentType != "" && !operationAcceptsContentType(operation, contentType) {
		return nil, fmt.Errorf("SecondBox client content type %q is not declared for %s", contentType, operation.OperationID)
	}
	request, err := http.NewRequestWithContext(ctx, operation.Method, endpoint.String(), options.Body)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client create %s request: %w", operation.OperationID, err)
	}
	request.Header = options.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client send %s request: %w", operation.OperationID, err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}
	defer response.Body.Close()
	const maximumProblemBytes = 4 << 20
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumProblemBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("SecondBox client read %s error response: %w", operation.OperationID, readErr)
	}
	if len(body) > maximumProblemBytes {
		return nil, fmt.Errorf("SecondBox client %s error response exceeds %d bytes", operation.OperationID, maximumProblemBytes)
	}
	failure := &APIError{StatusCode: response.StatusCode, Body: body}
	var problem Problem
	if json.Unmarshal(body, &problem) == nil {
		failure.Problem = &problem
	}
	return nil, failure
}

func operationAcceptsContentType(operation OperationMetadata, contentType string) bool {
	for _, representation := range operation.RequestBody {
		if representation.ContentType == contentType {
			return true
		}
	}
	return false
}

// EncodeJSONBody serializes a strongly typed generated request for RequestOptions.Body.
func EncodeJSONBody(value interface{}) (io.Reader, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client encode JSON request: %w", err)
	}
	return bytes.NewReader(encoded), nil
}
