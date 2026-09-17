package runnercontrol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

func TestImagePreparationResultAuthorityAndReplay(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.lifecycle_effects
		(id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
		 command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
		 claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,created_at,updated_at)
		VALUES ('prepare-op','sandbox',1,'prepare_image','queued','','','runner-authorized',
		 'prepare-command','','',0,0,$1,'',$2,'','','{}','{}',$2,$2)`, now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	write := func(runner string, result *runnerv1.PrepareImageResult, at time.Time) error {
		tx, err := store.pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err := recordImagePreparationResult(t.Context(), tx, runner, result, at); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	result := &runnerv1.PrepareImageResult{OperationId: "prepare-op", ResolvedDigest: "sha256:" + strings.Repeat("a", 64), Manifest: []byte("signed manifest"), Signature: []byte("signature")}
	if err := write("runner-other", result, now); err == nil {
		t.Fatal("unassigned Runner supplied preparation evidence")
	}
	invalid := proto.Clone(result).(*runnerv1.PrepareImageResult)
	invalid.ResolvedDigest = "invalid" + strings.Repeat("a", 64)
	if err := write("runner-authorized", invalid, now); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	if err := write("runner-authorized", result, now); err != nil {
		t.Fatal(err)
	}
	if err := write("runner-authorized", &runnerv1.PrepareImageResult{OperationId: "prepare-op", Failure: "late failure"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var state string
	var evidence []byte
	if err := store.pool.QueryRow(t.Context(), `SELECT state,evidence_json FROM secondbox.lifecycle_effects WHERE id='prepare-op'`).Scan(&state, &evidence); err != nil {
		t.Fatal(err)
	}
	var stored runnerv1.PrepareImageResult
	if err := json.Unmarshal(evidence, &stored); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || stored.ResolvedDigest != result.ResolvedDigest || stored.Failure != "" {
		t.Fatalf("terminal preparation was changed: %s %s", state, evidence)
	}
}

// TestReadySandboxExecutionImagePinKeepsRequestedReference covers the pin the
// public Sandbox projection reports: the reference stays as the client selected
// it while the digest follows the Instance that became ready.
func TestReadySandboxExecutionImagePinKeepsRequestedReference(t *testing.T) {
	const selectedTag = "registry.example/secondbox/runner-control-test:stable"
	const replacementTag = "registry.example/secondbox/runner-control-test:next"
	const replacementDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	const replacementReference = "registry.example/secondbox/runner-control-test@" + replacementDigest
	now := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	readySandboxImagePin := func(
		t *testing.T, suffix, operationReference, instanceReference, resolvedDigest,
		pinnedReference, pinnedDigest string,
	) (string, string) {
		t.Helper()
		store := openRunnerControlDatabase(t)
		fence := seedStartingAssignment(t, store, suffix, "starting", now)
		insertDeliveredAssignmentCommand(t, store, fence, "assignment-command-"+suffix, now)
		if _, err := store.pool.Exec(t.Context(),
			`UPDATE secondbox.instances SET requested_image_reference=$2 WHERE id=$1`,
			fence.InstanceId, instanceReference); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(t.Context(),
			`UPDATE secondbox.sandboxes SET execution_image_reference=$2,execution_image_digest=$3 WHERE id=$1`,
			fence.SandboxId, pinnedReference, pinnedDigest); err != nil {
			t.Fatal(err)
		}
		if operationReference != "" {
			if _, err := store.pool.Exec(t.Context(),
				`UPDATE secondbox.operations SET request_metadata_json=jsonb_build_object('executionImageReference',$2::text) WHERE id='operation-start' AND sandbox_id=$1`,
				fence.SandboxId, operationReference); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := store.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if err := recordAssignmentEvent(t.Context(), tx, "runner-home", &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_AssignmentResult{AssignmentResult: &runnerv1.AssignmentResult{
				Fence:                   fence,
				Terminal:                runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind:             "firecracker",
				BackendReference:        "fc-" + suffix,
				RequestedImageReference: instanceReference,
				ResolvedImageDigest:     resolvedDigest,
				Correlation: &runnerv1.Correlation{
					RequestId: "request-start", OperationId: "operation-start",
					SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
					SandboxGeneration: fence.SandboxGeneration,
					AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
				},
			}},
		}, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		var reference, digest string
		if err := store.pool.QueryRow(t.Context(),
			`SELECT execution_image_reference,execution_image_digest FROM secondbox.sandboxes WHERE id=$1`,
			fence.SandboxId).Scan(&reference, &digest); err != nil {
			t.Fatal(err)
		}
		return reference, digest
	}
	for _, expectation := range []struct {
		name               string
		operationReference string
		instanceReference  string
		resolvedDigest     string
		pinnedReference    string
		pinnedDigest       string
		wantReference      string
		wantDigest         string
	}{
		{
			name: "first-selection", operationReference: selectedTag,
			instanceReference: runnerControlTestExecutionImageReference,
			resolvedDigest:    runnerControlTestExecutionImageDigest,
			wantReference:     selectedTag, wantDigest: runnerControlTestExecutionImageDigest,
		},
		{
			name: "inherited-restart", operationReference: runnerControlTestExecutionImageReference,
			instanceReference: runnerControlTestExecutionImageReference,
			resolvedDigest:    runnerControlTestExecutionImageDigest,
			pinnedReference:   selectedTag, pinnedDigest: runnerControlTestExecutionImageDigest,
			wantReference: selectedTag, wantDigest: runnerControlTestExecutionImageDigest,
		},
		{
			name: "explicit-replacement", operationReference: replacementTag,
			instanceReference: replacementReference, resolvedDigest: replacementDigest,
			pinnedReference: selectedTag, pinnedDigest: runnerControlTestExecutionImageDigest,
			wantReference: replacementTag, wantDigest: replacementDigest,
		},
		{
			name:            "fixed-profile-keeps-pin",
			pinnedReference: selectedTag, pinnedDigest: runnerControlTestExecutionImageDigest,
			wantReference: selectedTag, wantDigest: runnerControlTestExecutionImageDigest,
		},
	} {
		t.Run(expectation.name, func(t *testing.T) {
			reference, digest := readySandboxImagePin(
				t, "image-pin-"+expectation.name, expectation.operationReference,
				expectation.instanceReference, expectation.resolvedDigest,
				expectation.pinnedReference, expectation.pinnedDigest,
			)
			if reference != expectation.wantReference || digest != expectation.wantDigest {
				t.Fatalf("Sandbox execution image pin = %q %q, want %q %q",
					reference, digest, expectation.wantReference, expectation.wantDigest)
			}
		})
	}
}
