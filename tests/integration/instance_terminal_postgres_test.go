package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"google.golang.org/protobuf/proto"
)

func TestPostgresInstanceTerminalIsCurrentFencedEvidenceWithoutEarlyRelease(t *testing.T) {
	for _, test := range []struct {
		name           string
		protocolReason runnerv1.InstanceObservedTerminationReason
		stableReason   string
	}{
		{
			name:           "guest_shutdown",
			protocolReason: runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN,
			stableReason:   "guest_shutdown",
		},
		{
			name:           "resource_exhaustion",
			protocolReason: runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_RESOURCE_EXHAUSTION,
			stableReason:   "resource_exhaustion",
		},
		{
			name:           "internal_failure",
			protocolReason: runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE,
			stableReason:   "internal_failure",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPostgresInstanceTerminalReason(t, test.protocolReason, test.stableReason)
		})
	}
}

func testPostgresInstanceTerminalReason(
	t *testing.T,
	protocolReason runnerv1.InstanceObservedTerminationReason,
	stableReason string,
) {
	now := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	boundary := newPostgresConformanceBoundary(t, now).(*postgresConformanceBoundary)
	connectionID := task4ID("terminal-connection")
	welcome, err := boundary.Connect(
		t.Context(),
		"runner-conformance",
		connectionID,
		helloFrameForIntegration("runner-conformance"),
	)
	if err != nil || welcome.GetWelcome() == nil {
		t.Fatalf("connect welcome = %#v, %v", welcome, err)
	}
	registration := task4Registration(
		"runner-conformance", connectionID, "pool-conformance",
	)
	if duplicate, err := boundary.stateStore.RecordRegistration(
		t.Context(), registration, now,
	); err != nil || duplicate {
		t.Fatalf("registration duplicate, error = %t, %v", duplicate, err)
	}

	sandboxID := task4ID("terminal-sandbox")
	instanceID := task4ID("terminal-instance")
	assignmentID := task4ID("terminal-assignment")
	fence := &runnerv1.AssignmentFence{
		AssignmentId: assignmentID, SandboxId: sandboxID, InstanceId: instanceID,
		SandboxGeneration: 1,
		FencingToken:      []byte("01234567890123456789012345678901"),
	}
	if err := boundary.SeedAssignment(t.Context(), fence, now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, cleanup := range []struct {
			statement string
			value     string
		}{
			{`DELETE FROM secondbox.instance_terminal_events WHERE instance_id=$1`, instanceID},
			{`DELETE FROM secondbox.runner_commands WHERE assignment_id=$1`, assignmentID},
			{`DELETE FROM secondbox.workspace_materializations WHERE assignment_id=$1`, assignmentID},
			{`DELETE FROM secondbox.assignments WHERE id=$1`, assignmentID},
			{`DELETE FROM secondbox.instances WHERE id=$1`, instanceID},
			{`DELETE FROM secondbox.sandboxes WHERE id=$1`, sandboxID},
			{`DELETE FROM secondbox.workspaces WHERE sandbox_id=$1`, sandboxID},
		} {
			if _, err := boundary.pool.Exec(context.Background(), cleanup.statement, cleanup.value); err != nil {
				t.Errorf("instance terminal cleanup failed: %v", err)
			}
		}
	})
	correlation := &runnerv1.Correlation{
		RequestId: "request-terminal", OperationId: "operation-terminal", LeaseId: "lease-terminal",
		SandboxId: sandboxID, InstanceId: instanceID, SandboxGeneration: 1,
		AssignmentId: assignmentID, RunnerId: "runner-conformance",
	}
	ready := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: "terminal-ready-2", Sequence: 2, Fence: fence,
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "fc-terminal",
				Correlation: correlation,
			},
		},
	}
	if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: "runner-conformance",
		ConnectionID: connectionID, Message: ready,
	}, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := boundary.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',next_reconcile_at=$2,revision=revision+1,updated_at=$2
		WHERE id=$1`,
		sandboxID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	terminal := &runnerv1.InstanceTerminal{
		MessageId: "instance-terminal-3", Sequence: 3, Fence: fence,
		Reason:                    protocolReason,
		ObservedAtUnixMs:          uint64(now.Add(2 * time.Millisecond).UnixMilli()),
		TerminationEvidenceDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Correlation:               correlation,
	}
	event := runnercontrol.Event{
		Kind: runnercontrol.EventInstanceTerminal, RunnerID: "runner-conformance",
		ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_InstanceTerminal{InstanceTerminal: terminal},
		},
	}
	if duplicate, err := boundary.stateStore.RecordEvent(
		t.Context(), event, now.Add(3*time.Millisecond),
	); err != nil || duplicate {
		t.Fatalf("terminal event duplicate, error = %t, %v", duplicate, err)
	}
	if duplicate, err := boundary.stateStore.RecordEvent(
		t.Context(), event, now.Add(4*time.Millisecond),
	); err != nil || !duplicate {
		t.Fatalf("exact terminal replay duplicate, error = %t, %v", duplicate, err)
	}

	var (
		instanceState, guestLiveness, reason                string
		assignmentState, materializationState, sandboxState string
		currentInstance                                     string
		generation, terminalRows                            int64
		nextReconcileAt                                     time.Time
	)
	if err := boundary.pool.QueryRow(t.Context(), `
		SELECT instance.state,instance.guest_liveness,instance.termination_reason,
		       assignment.state,materialization.state,sandbox.state,
		       sandbox.current_instance_id,sandbox.generation,sandbox.next_reconcile_at,
		       (SELECT count(*) FROM secondbox.instance_terminal_events
		        WHERE instance_id=instance.id)
		FROM secondbox.instances AS instance
		JOIN secondbox.assignments AS assignment ON assignment.instance_id=instance.id
		JOIN secondbox.workspace_materializations AS materialization
		  ON materialization.assignment_id=assignment.id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=instance.sandbox_id
		WHERE instance.id=$1`,
		instanceID,
	).Scan(
		&instanceState, &guestLiveness, &reason,
		&assignmentState, &materializationState, &sandboxState,
		&currentInstance, &generation, &nextReconcileAt, &terminalRows,
	); err != nil {
		t.Fatal(err)
	}
	if instanceState != "stopped" || guestLiveness != "stopped" ||
		reason != stableReason ||
		assignmentState != "ready" || materializationState != "ready" ||
		sandboxState != "ready" || currentInstance != instanceID || generation != 1 ||
		nextReconcileAt.After(now.Add(3*time.Millisecond)) || terminalRows != 1 {
		t.Fatalf(
			"terminal authority = instance %q/%q/%q assignment %q materialization %q sandbox %q current %q generation %d due %s rows %d",
			instanceState, guestLiveness, reason, assignmentState, materializationState,
			sandboxState, currentInstance, generation, nextReconcileAt, terminalRows,
		)
	}

	changed := proto.Clone(event.Message).(*runnerv1.RunnerToControlPlane)
	changed.GetInstanceTerminal().Reason =
		runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_UNSPECIFIED
	if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventInstanceTerminal, RunnerID: "runner-conformance",
		ConnectionID: connectionID, Message: changed,
	}, now.Add(5*time.Millisecond)); err == nil {
		t.Fatal("same-message changed terminal reason was accepted")
	}
	changed = proto.Clone(event.Message).(*runnerv1.RunnerToControlPlane)
	changed.GetInstanceTerminal().MessageId = "instance-terminal-changed-4"
	changed.GetInstanceTerminal().Sequence = 4
	changed.GetInstanceTerminal().TerminationEvidenceDigest =
		"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventInstanceTerminal, RunnerID: "runner-conformance",
		ConnectionID: connectionID, Message: changed,
	}, now.Add(5*time.Millisecond)); err == nil {
		t.Fatal("changed terminal digest was accepted")
	}
	stale := proto.Clone(event.Message).(*runnerv1.RunnerToControlPlane)
	stale.GetInstanceTerminal().MessageId = "instance-terminal-stale-4"
	stale.GetInstanceTerminal().Sequence = 4
	stale.GetInstanceTerminal().Fence.FencingToken = []byte("stale-stale-stale-stale-stale-stale")
	if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventInstanceTerminal, RunnerID: "runner-conformance",
		ConnectionID: connectionID, Message: stale,
	}, now.Add(5*time.Millisecond)); !errors.Is(err, runnercontrol.ErrStaleAssignmentEvidence) {
		t.Fatalf("stale terminal fence error = %v, want ErrStaleAssignmentEvidence", err)
	}
}

func helloFrameForIntegration(runnerID string) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{Hello: &runnerv1.RunnerHello{
			RunnerId: runnerID, ConnectionNonce: []byte("01234567890123456789012345678901"),
			SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			MandatoryFeatures: []runnerv1.RunnerFeature{
				runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
			},
		}},
	}
}

func TestPostgresFenceResultPreservesStableCausalTerminationReasons(t *testing.T) {
	now := time.Date(2026, 7, 28, 19, 0, 0, 0, time.UTC)
	boundary := newPostgresConformanceBoundary(t, now).(*postgresConformanceBoundary)
	connectionID := task4ID("reason-connection")
	if _, err := boundary.Connect(
		t.Context(), "runner-conformance", connectionID,
		helloFrameForIntegration("runner-conformance"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := boundary.stateStore.RecordRegistration(
		t.Context(),
		task4Registration("runner-conformance", connectionID, "pool-conformance"),
		now,
	); err != nil {
		t.Fatal(err)
	}
	causes := []struct {
		reason                 string
		sandboxLifecycle       bool
		preexistingReason      bool
		assignmentFailureClass string
	}{
		{"requested_drain", true, false, ""},
		{"requested_stop", true, false, ""},
		{"idle_timeout", true, false, ""},
		{"maximum_duration", true, false, ""},
		{"guest_shutdown", false, true, ""},
		{"resource_exhaustion", false, true, ""},
		{"guest_agent_lost", true, false, ""},
		{"runner_lost", false, false, "fencing"},
		{"startup_failed", false, false, "startup_timeout"},
		{"fenced", false, false, ""},
		{"internal_failure", false, true, ""},
	}
	sequence := uint64(2)
	for _, cause := range causes {
		t.Run(cause.reason, func(t *testing.T) {
			sandboxID := task4ID("reason-sandbox")
			instanceID := task4ID("reason-instance")
			assignmentID := task4ID("reason-assignment")
			fence := &runnerv1.AssignmentFence{
				AssignmentId: assignmentID, SandboxId: sandboxID, InstanceId: instanceID,
				SandboxGeneration: 1,
				FencingToken:      []byte("01234567890123456789012345678901"),
			}
			if err := boundary.SeedAssignment(t.Context(), fence, now); err != nil {
				t.Fatal(err)
			}
			correlation := &runnerv1.Correlation{
				RequestId: "request-" + assignmentID, OperationId: "operation-" + assignmentID,
				SandboxId: sandboxID, InstanceId: instanceID, SandboxGeneration: 1,
				AssignmentId: assignmentID, RunnerId: "runner-conformance",
			}
			readySequence := sequence
			sequence++
			if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
				Kind: runnercontrol.EventAssignment, RunnerID: "runner-conformance",
				ConnectionID: connectionID,
				Message: &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
						AssignmentResult: &runnerv1.AssignmentResult{
							MessageId: "ready-" + assignmentID, Sequence: readySequence,
							Fence:       fence,
							Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
							BackendKind: "firecracker", BackendReference: "fc-" + instanceID,
							Correlation: correlation,
						},
					},
				},
			}, now); err != nil {
				t.Fatal(err)
			}
			if cause.sandboxLifecycle {
				if _, err := boundary.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET lifecycle_termination_reason=$2,current_instance_id=$3
					WHERE id=$1`,
					sandboxID, cause.reason, instanceID,
				); err != nil {
					t.Fatal(err)
				}
			} else if cause.preexistingReason {
				if _, err := boundary.pool.Exec(t.Context(), `
					UPDATE secondbox.instances SET termination_reason=$2 WHERE id=$1`,
					instanceID, cause.reason,
				); err != nil {
					t.Fatal(err)
				}
			}
			if cause.assignmentFailureClass != "" {
				if _, err := boundary.pool.Exec(t.Context(), `
					UPDATE secondbox.assignments SET failure_class=$2 WHERE id=$1`,
					assignmentID, cause.assignmentFailureClass,
				); err != nil {
					t.Fatal(err)
				}
			}
			fenceSequence := sequence
			sequence++
			if _, err := boundary.stateStore.RecordEvent(t.Context(), runnercontrol.Event{
				Kind: runnercontrol.EventFence, RunnerID: "runner-conformance",
				ConnectionID: connectionID,
				Message: &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_FenceResult{
						FenceResult: &runnerv1.FenceResult{
							MessageId: "fence-" + assignmentID, Sequence: fenceSequence,
							Fence:                     fence,
							Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
							TerminationEvidenceDigest: "sha256:release-" + assignmentID,
							Correlation:               correlation,
						},
					},
				},
			}, now.Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			var reason, assignmentState, materializationState string
			if err := boundary.pool.QueryRow(t.Context(), `
				SELECT instance.termination_reason,assignment.state,materialization.state
				FROM secondbox.instances AS instance
				JOIN secondbox.assignments AS assignment ON assignment.instance_id=instance.id
				JOIN secondbox.workspace_materializations AS materialization
				  ON materialization.assignment_id=assignment.id
				WHERE instance.id=$1`,
				instanceID,
			).Scan(&reason, &assignmentState, &materializationState); err != nil {
				t.Fatal(err)
			}
			if reason != cause.reason ||
				assignmentState != "fenced" ||
				materializationState != "released" {
				t.Fatalf(
					"reason propagation = %q, assignment %q, materialization %q",
					reason, assignmentState, materializationState,
				)
			}
			for _, cleanup := range []struct {
				query string
				value string
			}{
				{`DELETE FROM secondbox.runner_commands WHERE assignment_id=$1`, assignmentID},
				{`DELETE FROM secondbox.workspace_materializations WHERE assignment_id=$1`, assignmentID},
				{`DELETE FROM secondbox.assignments WHERE id=$1`, assignmentID},
				{`DELETE FROM secondbox.instances WHERE id=$1`, instanceID},
				{`DELETE FROM secondbox.sandboxes WHERE id=$1`, sandboxID},
				{`DELETE FROM secondbox.workspaces WHERE sandbox_id=$1`, sandboxID},
			} {
				if _, err := boundary.pool.Exec(t.Context(), cleanup.query, cleanup.value); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := boundary.pool.Exec(t.Context(), `
				UPDATE secondbox.runners SET reserved_capacity_json='{}'
				WHERE id='runner-conformance'`,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}
