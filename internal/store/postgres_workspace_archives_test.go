package store

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

type archiveTransferTestProvider struct{ methods []string }

func (provider *archiveTransferTestProvider) Transfer(_ context.Context, _ string, method string, descriptor *runnerv1.WorkspaceArchiveDescriptor) (*runnerv1.WorkspaceArchiveTransfer, error) {
	provider.methods = append(provider.methods, method)
	return &runnerv1.WorkspaceArchiveTransfer{Url: "https://archive.example/workspace", ExpiresAtUnixMs: uint64(time.Now().Add(time.Hour).UnixMilli()), ArchiveDescriptor: descriptor}, nil
}

func (*archiveTransferTestProvider) Delete(context.Context, string) error { return nil }

func archiveTestInput(sandboxID, suffix string, now time.Time) ports.WorkspaceArchiveInput {
	return ports.WorkspaceArchiveInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, AuditEvent: testLifecycleAudit(), SandboxID: sandboxID, Operation: localTestOperation("operation-archive-"+suffix, "archive", now), ArchiveID: "archive-" + suffix, CommandID: "command-archive-" + suffix, FencingToken: []byte("01234567890123456789012345678901"), ExpectedRevision: 1, IdempotencyKey: "archive-" + suffix, RequestHash: "hash-archive-" + suffix, IdempotencyEnds: now.Add(time.Hour), Now: now}
}

func TestWorkspaceArchiveAdmissionAndDirectTransferCommand(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='draining',drain_phase='draining' WHERE id='runner-home'`); err != nil {
		t.Fatal(err)
	}
	provider := &archiveTransferTestProvider{}
	storage.ConfigureWorkspaceArchives(provider)
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-admission", now)
	input := archiveTestInput(sandboxID, "admission", now)
	operation, err := storage.ArchiveSandbox(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := storage.ArchiveSandbox(t.Context(), input)
	if err != nil || replayed.ID != operation.ID {
		t.Fatalf("archive replay=%#v err=%v", replayed, err)
	}
	if operation.SandboxID != sandboxID {
		t.Fatalf("operation Sandbox=%q", operation.SandboxID)
	}
	var record workspaceArchiveCommand
	if err := storage.pool.QueryRow(t.Context(), `SELECT id,operation_id,sandbox_id,workspace_id,source_runner_id,target_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,descriptor_json FROM secondbox.workspace_archives WHERE id=$1`, input.ArchiveID).Scan(&record.id, &record.operationID, &record.sandboxID, &record.workspaceID, &record.source, &record.target, &record.generation, &record.capacity, &record.key, &record.state, &record.commandID, &record.fence, &record.descriptorJSON); err != nil {
		t.Fatal(err)
	}
	if err := storage.queueWorkspaceArchiveCommand(t.Context(), record, now); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	var home, state string
	if err := storage.pool.QueryRow(t.Context(), `SELECT command.payload,workspace.home_runner_id,archive.state FROM secondbox.runner_commands command,secondbox.workspaces workspace,secondbox.workspace_archives archive WHERE command.id=$1 AND workspace.id=$2 AND archive.id=$3`, input.CommandID, workspaceID, input.ArchiveID).Scan(&payload, &home, &state); err != nil {
		t.Fatal(err)
	}
	envelope := new(runnerv1.ControlPlaneToRunner)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	command := envelope.GetLocalWorkspace()
	if home != "runner-home" || state != "queued_sent" || command.GetKind() != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT || command.GetArchiveTransfer().GetUrl() == "" || len(provider.methods) != 1 || provider.methods[0] != http.MethodPut {
		t.Fatalf("archive command=%#v home=%q state=%q methods=%v", command, home, state, provider.methods)
	}
	command.ArchiveTransfer.ExpiresAtUnixMs = uint64(now.Add(-time.Minute).UnixMilli())
	expiredPayload, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runner_commands SET payload=$2 WHERE id=$1`, input.CommandID, expiredPayload); err != nil {
		t.Fatal(err)
	}
	if err := storage.ReconcileWorkspaceArchives(t.Context(), now, nil); err != nil {
		t.Fatal(err)
	}
	if len(provider.methods) != 2 {
		t.Fatalf("expired pending transfer was not renewed: methods=%v", provider.methods)
	}

	t.Run("retained Snapshot refuses offload", func(t *testing.T) {
		workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-snapshot", now)
		seedReadyLocalSnapshot(t, storage, "archive-snapshot", sandboxID, workspaceID, now)
		_, err := storage.ArchiveSandbox(t.Context(), archiveTestInput(sandboxID, "snapshot", now))
		if !errors.Is(err, ports.ErrRelocationSnapshotsPresent) {
			t.Fatalf("archive Snapshot error=%v", err)
		}
	})
}

func TestArchivedWorkspaceStartCanBeCancelledBeforePlacement(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-cancel", now)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='archived',home_runner_id='',archive_key='workspaces/test' WHERE id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	intent := ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning, Operation: localTestOperation("archive-cancel-start", "start", now), AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: "archive-cancel-start", RequestHash: "archive-cancel-start", IdempotencyEnds: now.Add(time.Hour), Now: now}
	if _, err := storage.SetSandboxDesiredState(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	intent.DesiredState = contracts.SandboxDesiredStateStopped
	intent.Operation = localTestOperation("archive-cancel-stop", "stop", now)
	intent.AuditEvent = testLifecycleAudit()
	intent.ExpectedRevision = 2
	intent.IdempotencyKey = "archive-cancel-stop"
	intent.RequestHash = "archive-cancel-stop"
	if _, err := storage.SetSandboxDesiredState(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	var mutation, desired, operationState string
	if err := storage.pool.QueryRow(t.Context(), `SELECT workspace.mutation_kind,sandbox.desired_state,operation.state FROM secondbox.workspaces workspace JOIN secondbox.sandboxes sandbox ON sandbox.workspace_id=workspace.id JOIN secondbox.operations operation ON operation.id='archive-cancel-start' WHERE workspace.id=$1`, workspaceID).Scan(&mutation, &desired, &operationState); err != nil {
		t.Fatal(err)
	}
	if mutation != "" || desired != "stopped" || operationState != "cancelled" {
		t.Fatalf("cancelled start mutation=%q desired=%q operation=%q", mutation, desired, operationState)
	}
}

func TestArchivedWorkspaceRestorePlacementAndCapacityDeadline(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	seedWorkspaceRelocationTarget(t, storage, "runner-archive-restore", "ready", now)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET protocol_versions_json='["8"]' WHERE id='runner-home'`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"placed", "expired"} {
		t.Run(suffix, func(t *testing.T) {
			workspaceID, sandboxID := seedLocalWorkspace(t, storage, "restore-"+suffix, now)
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='archived',home_runner_id='',archive_key='workspaces/restore',archive_descriptor_json='{"archive_bytes":4096}' WHERE id=$1`, workspaceID); err != nil {
				t.Fatal(err)
			}
			operation := localTestOperation("archive-restore-"+suffix, "start", now)
			deadline := now.Add(time.Minute)
			if suffix == "expired" {
				deadline = now.Add(-time.Minute)
			}
			operation.CapacityWaitDeadline = &deadline
			if _, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning, Operation: operation, AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: operation.ID, RequestHash: operation.ID, IdempotencyEnds: now.Add(time.Hour), Now: now}); err != nil {
				t.Fatal(err)
			}
			if err := storage.prepareArchivedWorkspaceStart(t.Context(), sandboxID, now); err != nil {
				t.Fatal(err)
			}
			var state, home, mutation, desired, operationState string
			if err := storage.pool.QueryRow(t.Context(), `SELECT workspace.state,workspace.home_runner_id,workspace.mutation_kind,sandbox.desired_state,operation.state FROM secondbox.workspaces workspace JOIN secondbox.sandboxes sandbox ON sandbox.id=workspace.sandbox_id JOIN secondbox.operations operation ON operation.id=$2 WHERE workspace.id=$1`, workspaceID, operation.ID).Scan(&state, &home, &mutation, &desired, &operationState); err != nil {
				t.Fatal(err)
			}
			if suffix == "expired" {
				if state != "archived" || home != "" || mutation != "" || desired != "stopped" || operationState != "failed" {
					t.Fatalf("expired restore state=%q home=%q mutation=%q desired=%q operation=%q", state, home, mutation, desired, operationState)
				}
				return
			}
			if state != "restoring" || home != "" || mutation != "archive_restore" || desired != "running" || operationState != "pending" {
				t.Fatalf("placed restore state=%q home=%q mutation=%q desired=%q operation=%q", state, home, mutation, desired, operationState)
			}
			var target string
			if err := storage.pool.QueryRow(t.Context(), `SELECT target_runner_id FROM secondbox.workspace_archives WHERE operation_id=$1`, operation.ID).Scan(&target); err != nil {
				t.Fatal(err)
			}
			tx, err := storage.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			reservation, err := durableHomeReservation(t.Context(), tx, target)
			if err != nil || reservation.DiskBytes < 8<<30 {
				t.Fatalf("restore reservation=%#v err=%v", reservation, err)
			}
		})
	}
}
