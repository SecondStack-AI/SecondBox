package runnerevidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLifecycleEvidenceRequiresFixedDimensions(t *testing.T) {
	record := NewRecord(EventLifecycleStage, "completed", "ready", time.Now())
	record.RunnerID = "runner-1"
	record.RequestID = "request-1"
	record.OperationID = "operation-1"
	record.SandboxID = "sandbox-1"
	record.InstanceID = "instance-1"
	record.SandboxGeneration = 1
	record.AssignmentID = "assignment-1"
	if err := record.Validate(); err == nil {
		t.Fatal("incomplete lifecycle dimensions were accepted")
	}
	record.BackendKind = "microsandbox"
	record.HostPlatform = "linux-amd64"
	record.BackendVersion = "microsandbox-0.6.8/msb_krun-0.1.30"
	record.Materialization = "sha256:" + strings.Repeat("a", 64)
	record.Stage = "ready"
	record.StreamID = "control:1"
	record.HelperPID = 42
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	record.HelperReason = strings.Repeat("x", 257)
	if err := record.Validate(); err == nil {
		t.Fatal("unbounded lifecycle classification was accepted")
	}
}

func TestSlogSinkRejectsIncompleteOperationCorrelation(t *testing.T) {
	record := NewRecord(EventExecTerminal, "completed", "exited", time.Now().UTC())
	record.RunnerID = "runner-1"
	if err := (SlogSink{}).Emit(context.Background(), record); err == nil {
		t.Fatal("operation evidence without request, operation, and assignment correlation was accepted")
	}
}

func TestRecordJSONShapeCannotRepresentPayloadsOrSecrets(t *testing.T) {
	record := NewRecord(EventExecTerminal, "completed", "exited", time.Now().UTC())
	record.RequestID = "request-1"
	record.OperationID = "operation-1"
	record.SandboxID = "sandbox-1"
	record.InstanceID = "instance-1"
	record.SandboxGeneration = 7
	record.AssignmentID = "assignment-1"
	record.LeaseID = "lease-1"
	record.RunnerID = "runner-1"
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{
		"schemaVersion":     true,
		"event":             true,
		"outcome":           true,
		"terminalKind":      true,
		"requestId":         true,
		"operationId":       true,
		"sandboxId":         true,
		"instanceId":        true,
		"sandboxGeneration": true,
		"assignmentId":      true,
		"leaseId":           true,
		"runnerId":          true,
		"observedAtUnixMs":  true,
	}
	if len(fields) != len(expected) {
		t.Fatalf("evidence JSON fields = %v", fields)
	}
	for field := range fields {
		if !expected[field] {
			t.Fatalf("unexpected evidence JSON field %q", field)
		}
		lower := strings.ToLower(field)
		for _, forbidden := range []string{
			"token", "secret", "credential", "payload", "content", "command",
			"environment", "stdin", "stdout", "stderr", "path", "destination",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("evidence JSON field %q can carry %s", field, forbidden)
			}
		}
	}
}
