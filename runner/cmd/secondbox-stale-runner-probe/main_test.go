package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

func TestLoadStaleAssignmentProbeInputRejectsUnknownAuthority(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(inputPath, []byte(`{
		"assignmentId":"assignment-1",
		"sandboxId":"sandbox-1",
		"instanceId":"instance-1",
		"generation":7,
		"fencingTokenBase64":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"requestId":"request-1",
		"operationId":"operation-1",
		"unexpected":"authority"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadStaleAssignmentProbeInput(inputPath); err == nil {
		t.Fatal("stale Runner probe accepted unknown Assignment authority")
	}
}

func TestStaleProbeAssignmentResultPreservesOldFence(t *testing.T) {
	token := make([]byte, 32)
	for index := range token {
		token[index] = byte(index + 1)
	}
	input := staleAssignmentProbeInput{
		AssignmentID: "assignment-old",
		SandboxID:    "sandbox-1",
		InstanceID:   "instance-old",
		Generation:   41,
		FencingToken: base64.StdEncoding.EncodeToString(token),
		RequestID:    "request-old",
		OperationID:  "operation-old",
	}

	frame := staleProbeAssignmentResult("runner-a", input)
	result := frame.GetAssignmentResult()
	if result == nil {
		t.Fatal("stale Runner probe did not create an Assignment result")
	}
	if result.Fence.AssignmentId != input.AssignmentID ||
		result.Fence.SandboxGeneration != input.Generation ||
		string(result.Fence.FencingToken) != string(token) {
		t.Fatalf("stale Assignment fence changed: %#v", result.Fence)
	}
	if result.Correlation.RunnerId != "runner-a" ||
		result.Correlation.OperationId != input.OperationID {
		t.Fatalf("stale Assignment correlation changed: %#v", result.Correlation)
	}
}

func TestStaleProbeRestoresDrainedAdmissionBeforeSubmittingOldFence(t *testing.T) {
	frame := staleProbeDrainedHeartbeat("runner-a", "connection-probe")
	heartbeat := frame.GetHeartbeat()
	if heartbeat == nil {
		t.Fatal("stale Runner probe did not create a drained Heartbeat")
	}
	if heartbeat.RunnerId != "runner-a" ||
		heartbeat.ConnectionId != "connection-probe" ||
		heartbeat.DrainPhase.String() != "DRAIN_PHASE_DRAINED" ||
		heartbeat.Sequence != 2 {
		t.Fatalf("stale Runner probe changed drained admission evidence: %#v", heartbeat)
	}
}

func TestStaleProbeRegistrationIsDistinguishableFromPackagedRunner(t *testing.T) {
	registration := staleProbeRegistration(
		runnercontrol.RunnerProtocolConfig{
			RunnerID: "runner-a", RunnerPoolID: "pool-a",
			SoftwareVersion: "1.2.3", ProtocolMinimum: 1, ProtocolMaximum: 1,
		},
		"connection-probe",
	).GetRegistration()
	if registration == nil ||
		registration.SoftwareVersion != "1.2.3-qualification-stale-probe" {
		t.Fatalf("stale probe registration is not distinguishable: %#v", registration)
	}
}
