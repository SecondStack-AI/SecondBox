package store

import (
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestRelocateSandboxAdmitsOnlyStoppedSnapshotFreeWorkspaceToCompatibleRunner(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 3, 16, 0, 0, 0, time.UTC)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET capabilities_json='["compute","local-workspace","workspace-relocation"]',
		    protocol_versions_json='["4"]',state='ready',
		    active_connection_id='connection-home',drain_phase='active'
		WHERE id='runner-home'`,
	); err != nil {
		t.Fatal(err)
	}
	seedWorkspaceRelocationTarget(t, store, "runner-relocation-target", "ready", now)

	t.Run("happy path", func(t *testing.T) {
		workspaceID, sandboxID := seedLocalWorkspace(t, store, "relocation-happy", now)
		operation, err := store.RelocateSandbox(t.Context(), workspaceRelocationInput(
			sandboxID, "happy", "runner-relocation-target", now,
		))
		if err != nil {
			t.Fatal(err)
		}
		if operation.Kind != "relocate" || operation.State != contracts.OperationStatePending {
			t.Fatalf("relocation Operation = %#v", operation)
		}
		var homeRunnerID, mutationState, relocationState, targetRunnerID string
		if err := store.pool.QueryRow(t.Context(), `
			SELECT workspace.home_runner_id,workspace.mutation_state,
			       relocation.state,relocation.target_runner_id
			FROM secondbox.workspaces AS workspace
			JOIN secondbox.workspace_relocations AS relocation
			  ON relocation.workspace_id=workspace.id
			WHERE workspace.id=$1`, workspaceID,
		).Scan(&homeRunnerID, &mutationState, &relocationState, &targetRunnerID); err != nil {
			t.Fatal(err)
		}
		if homeRunnerID != "runner-home" || mutationState != "queued" ||
			relocationState != "queued" || targetRunnerID != "runner-relocation-target" {
			t.Fatalf(
				"relocation authority home=%q mutation=%q state=%q target=%q",
				homeRunnerID, mutationState, relocationState, targetRunnerID,
			)
		}
	})

	t.Run("running Sandbox", func(t *testing.T) {
		_, sandboxID := seedLocalWorkspace(t, store, "relocation-running", now)
		if _, err := store.pool.Exec(t.Context(), `
			UPDATE secondbox.sandboxes
			SET state='ready',desired_state='running',current_instance_id='instance-running'
			WHERE id=$1`, sandboxID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RelocateSandbox(t.Context(), workspaceRelocationInput(
			sandboxID, "running", "runner-relocation-target", now,
		)); !errors.Is(err, ports.ErrSandboxNotStopped) {
			t.Fatalf("running Sandbox relocation error = %v", err)
		}
	})

	t.Run("Snapshot present", func(t *testing.T) {
		workspaceID, sandboxID := seedLocalWorkspace(t, store, "relocation-snapshot", now)
		seedReadyLocalSnapshot(t, store, "relocation-snapshot", sandboxID, workspaceID, now)
		if _, err := store.RelocateSandbox(t.Context(), workspaceRelocationInput(
			sandboxID, "snapshot", "runner-relocation-target", now,
		)); !errors.Is(err, ports.ErrRelocationSnapshotsPresent) {
			t.Fatalf("Snapshot relocation error = %v", err)
		}
	})

	t.Run("unhealthy target", func(t *testing.T) {
		_, sandboxID := seedLocalWorkspace(t, store, "relocation-unhealthy", now)
		seedWorkspaceRelocationTarget(t, store, "runner-relocation-unhealthy", "offline", now)
		if _, err := store.RelocateSandbox(t.Context(), workspaceRelocationInput(
			sandboxID, "unhealthy", "runner-relocation-unhealthy", now,
		)); !errors.Is(err, ports.ErrRelocationTargetUnavailable) {
			t.Fatalf("unhealthy target relocation error = %v", err)
		}
	})

	t.Run("incompatible target", func(t *testing.T) {
		_, sandboxID := seedLocalWorkspace(t, store, "relocation-incompatible", now)
		seedWorkspaceRelocationTarget(t, store, "runner-relocation-incompatible", "ready", now)
		if _, err := store.pool.Exec(t.Context(), `
			UPDATE secondbox.runners
			SET capabilities_json='["compute","local-workspace","storage"]'
			WHERE id='runner-relocation-incompatible'`,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RelocateSandbox(t.Context(), workspaceRelocationInput(
			sandboxID, "incompatible", "runner-relocation-incompatible", now,
		)); !errors.Is(err, ports.ErrRelocationTargetUnavailable) {
			t.Fatalf("incompatible target relocation error = %v", err)
		}
	})
}

func workspaceRelocationInput(
	sandboxID string,
	suffix string,
	targetRunnerID string,
	now time.Time,
) ports.WorkspaceRelocationInput {
	return ports.WorkspaceRelocationInput{
		Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"},
		SandboxID: sandboxID, TargetRunnerID: targetRunnerID,
		Operation:       localTestOperation("operation-relocation-"+suffix, "relocate", now),
		RelocationID:    "relocation-" + suffix,
		ExportCommandID: "command-relocation-export-" + suffix,
		FencingToken:    []byte("01234567890123456789012345678901"),
		IdempotencyKey:  "idempotency-relocation-" + suffix,
		RequestHash:     "hash-relocation-" + suffix,
		IdempotencyEnds: now.Add(time.Hour), ExpectedRevision: 1, Now: now,
	}
}

func seedWorkspaceRelocationTarget(
	t *testing.T,
	store *PostgresControlPlaneStore,
	runnerID string,
	state string,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,backend_kind,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			$1,'pool-local',$1,$2,'["amd64"]',
			'["compute","local-workspace","storage","workspace-relocation"]',
			'{"VCPUCount":8000,"MemoryBytes":17179869184,"DiskBytes":17179869184,"Instances":8,"Operations":32}',
			'["2"]',1,1,'test','connection-' || $1,0,'active','{}','` + placementTestCacheJSON + `','firecracker',0,0,$3,1,$3,$3
		) ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state,
			active_connection_id=EXCLUDED.active_connection_id,
			capabilities_json=EXCLUDED.capabilities_json,
			capacity_json=EXCLUDED.capacity_json,
			protocol_versions_json=EXCLUDED.protocol_versions_json,
			drain_phase=EXCLUDED.drain_phase,updated_at=EXCLUDED.updated_at`,
		runnerID, state, now,
	); err != nil {
		t.Fatal(err)
	}
}
