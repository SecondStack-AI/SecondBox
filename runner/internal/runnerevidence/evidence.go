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
