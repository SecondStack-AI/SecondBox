// Package runnerevidence defines the fixed-shape, payload-free Runner audit record.
package runnerevidence

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type Event string

const (
	EventAssignmentTerminal Event = "assignment_terminal"
	EventExecTerminal       Event = "exec_terminal"
	EventFileTerminal       Event = "file_terminal"
	EventPortOpen           Event = "port_open"
	EventPortTerminal       Event = "port_terminal"
	EventFenceTerminal      Event = "fence_terminal"
	EventNetworkFailure     Event = "network_failure"
	EventStoragePressure    Event = "storage_pressure"
	EventTeardownTerminal   Event = "teardown_terminal"
	EventInstanceTerminal   Event = "instance_terminal"
	EventLifecycleStage     Event = "lifecycle_stage"
)

// Record intentionally contains only bounded classifications and correlation
// identifiers. Payload-bearing fields cannot be represented.
type Record struct {
	SchemaVersion     uint32 `json:"schemaVersion"`
	Event             Event  `json:"event"`
	Outcome           string `json:"outcome"`
	TerminalKind      string `json:"terminalKind"`
	RequestID         string `json:"requestId"`
	OperationID       string `json:"operationId"`
	SandboxID         string `json:"sandboxId"`
	InstanceID        string `json:"instanceId"`
	SandboxGeneration uint64 `json:"sandboxGeneration"`
	AssignmentID      string `json:"assignmentId"`
	LeaseID           string `json:"leaseId"`
	RunnerID          string `json:"runnerId"`
	ObservedAtUnixMs  uint64 `json:"observedAtUnixMs"`
	BackendKind       string `json:"backendKind,omitempty"`
	HostPlatform      string `json:"hostPlatform,omitempty"`
	BackendVersion    string `json:"backendVersion,omitempty"`
	Materialization   string `json:"materializationDigest,omitempty"`
	Stage             string `json:"stage,omitempty"`
	StreamID          string `json:"streamId,omitempty"`
	// HelperPID is the backend's local compute supervisor process: the
	// Microsandbox helper for that backend and the supervised runsc process
	// for gVisor. Lifecycle evidence always requires a live local identity.
	HelperPID       int    `json:"helperPid,omitempty"`
	ExitCode        int    `json:"exitCode,omitempty"`
	Signal          int    `json:"signal,omitempty"`
	HelperReason    string `json:"helperReason,omitempty"`
	StderrDigest    string `json:"stderrDigest,omitempty"`
	EventTailDigest string `json:"eventTailDigest,omitempty"`
}

type Sink interface {
	Emit(context.Context, Record) error
}

type SlogSink struct{}

func (SlogSink) Emit(_ context.Context, record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	slog.Info(
		"runner operation evidence",
		"schemaVersion", record.SchemaVersion,
		"event", record.Event,
		"outcome", record.Outcome,
		"terminalKind", record.TerminalKind,
		"requestId", record.RequestID,
		"operationId", record.OperationID,
		"sandboxId", record.SandboxID,
		"instanceId", record.InstanceID,
		"sandboxGeneration", record.SandboxGeneration,
		"assignmentId", record.AssignmentID,
		"leaseId", record.LeaseID,
		"runnerId", record.RunnerID,
		"observedAtUnixMs", record.ObservedAtUnixMs,
		"backendKind", record.BackendKind,
		"hostPlatform", record.HostPlatform,
		"backendVersion", record.BackendVersion,
		"materializationDigest", record.Materialization,
		"stage", record.Stage,
		"streamId", record.StreamID,
		"helperPid", record.HelperPID,
		"exitCode", record.ExitCode,
		"signal", record.Signal,
		"helperReason", record.HelperReason,
		"stderrDigest", record.StderrDigest,
		"eventTailDigest", record.EventTailDigest,
	)
	return nil
}

// Validate rejects incomplete or unbounded evidence before it reaches a sink.
func (record Record) Validate() error {
	if record.SchemaVersion != 1 ||
		record.Event == "" ||
		record.Outcome == "" ||
		record.TerminalKind == "" ||
		record.RunnerID == "" ||
		record.ObservedAtUnixMs == 0 {
		return fmt.Errorf("SecondBox runner evidence record is incomplete")
	}
	if record.Event == EventStoragePressure {
		return nil
	}
	if record.RequestID == "" ||
		record.OperationID == "" ||
		record.SandboxID == "" ||
		record.InstanceID == "" ||
		record.SandboxGeneration == 0 ||
		record.AssignmentID == "" {
		return fmt.Errorf("SecondBox runner operation evidence correlation is incomplete")
	}
	if record.Event == EventLifecycleStage && (record.BackendKind == "" || record.HostPlatform == "" ||
		record.BackendVersion == "" || record.Materialization == "" || record.Stage == "" ||
		record.StreamID == "" || record.HelperPID <= 0) {
		return fmt.Errorf("SecondBox runner lifecycle evidence is incomplete")
	}
	for name, value := range map[string]string{
		"backend kind": record.BackendKind, "host platform": record.HostPlatform,
		"backend version": record.BackendVersion, "materialization": record.Materialization,
		"stage": record.Stage, "stream ID": record.StreamID, "helper reason": record.HelperReason,
		"stderr digest": record.StderrDigest, "event-tail digest": record.EventTailDigest,
	} {
		if len(value) > 256 {
			return fmt.Errorf("SecondBox runner evidence %s is unbounded", name)
		}
	}
	return nil
}

func NewRecord(event Event, outcome, terminalKind string, observedAt time.Time) Record {
	return Record{
		SchemaVersion:    1,
		Event:            event,
		Outcome:          outcome,
		TerminalKind:     terminalKind,
		ObservedAtUnixMs: uint64(observedAt.UTC().UnixMilli()),
	}
}
