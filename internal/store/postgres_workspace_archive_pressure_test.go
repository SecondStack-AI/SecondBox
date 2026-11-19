package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestWorkspaceArchivePressureOffloadsYoungStoppedCodeOnly(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, map[string]int64{"profile-local": 3600}); err != nil {
		t.Fatal(err)
	}
	var sandboxIDs, runnerIDs []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_, err := storage.pool.Exec(ctx, `
 DELETE FROM secondbox.runner_commands WHERE assignment_id IN (SELECT id FROM secondbox.workspace_archives WHERE sandbox_id=ANY($1::text[]));
 DELETE FROM secondbox.audit_events WHERE resource_id=ANY($1::text[]);
 DELETE FROM secondbox.idempotency_records WHERE target_id=ANY($1::text[]);
 DELETE FROM secondbox.operation_stage_timings WHERE operation_id IN (SELECT id FROM secondbox.operations WHERE sandbox_id=ANY($1::text[]));
 DELETE FROM secondbox.workspace_archives WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.snapshots WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.operations WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.workspaces WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.sandboxes WHERE id=ANY($1::text[]);
 DELETE FROM secondbox.runners WHERE id=ANY($2::text[]);`, pgx.QueryExecModeSimpleProtocol, sandboxIDs, runnerIDs)
		if err != nil {
			t.Errorf("pressure fixture cleanup: %v", err)
		}
	})
	type candidate struct {
		suffix, status                                   string
		active, snapshot, mutation, agent, logical, want bool
		runner                                           string
	}
	cases := []candidate{
		{suffix: "a-warning-young", status: "warning", want: true, runner: "pressure-warning-source"},
		{suffix: "b-warning-young", status: "warning", runner: "pressure-warning-source"},
		{suffix: "denied-young", status: "admission_denied", want: true},
		{suffix: "healthy-young", status: "healthy"},
		{suffix: "unavailable-young", status: "unavailable"},
		{suffix: "active", status: "warning", active: true},
		{suffix: "snapshot", status: "warning", snapshot: true},
		{suffix: "mutation", status: "warning", mutation: true},
		{suffix: "agent-unconfigured", status: "warning", agent: true},
		{suffix: "logical-admission", status: "warning", logical: true},
	}
	for index := range cases {
		candidate := &cases[index]
		if candidate.runner == "" {
			candidate.runner = "pressure-source-" + candidate.suffix
		}
		seedWorkspaceRelocationTarget(t, storage, candidate.runner, "ready", now)
		runnerIDs = append(runnerIDs, candidate.runner)
		capabilities := `["compute","local-workspace","physical-storage-admission"]`
		if candidate.logical {
			capabilities = `["compute","local-workspace"]`
		}
		if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET capabilities_json=$2,storage_pressure_json=jsonb_build_object('status',$3::text,'observedAt',$4::timestamptz) WHERE id=$1`, candidate.runner, capabilities, candidate.status, now); err != nil {
			t.Fatal(err)
		}
		workspaceID, sandboxID := seedLocalWorkspace(t, storage, "pressure-"+candidate.suffix, now)
		sandboxIDs = append(sandboxIDs, sandboxID)
		if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET home_runner_id=$2 WHERE id=$1`, workspaceID, candidate.runner); err != nil {
			t.Fatal(err)
		}
		if candidate.active {
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET state='ready',desired_state='running',current_instance_id='pressure-active-instance' WHERE id=$1`, sandboxID); err != nil {
				t.Fatal(err)
			}
		}
		if candidate.snapshot {
			seedReadyLocalSnapshot(t, storage, "pressure-"+candidate.suffix, sandboxID, workspaceID, now)
		}
		if candidate.mutation {
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_kind='snapshot_create',mutation_state='queued' WHERE id=$1`, workspaceID); err != nil {
				t.Fatal(err)
			}
		}
		if candidate.agent {
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET profile_name='agent-pressure-unconfigured' WHERE id=$1`, sandboxID); err != nil {
				t.Fatal(err)
			}
		}
	}
	profiles := map[string]int64{"profile-local": 3600}
	if err := storage.archivePressureWorkspaces(t.Context(), now, profiles); err != nil {
		t.Fatal(err)
	}
	for index, candidate := range cases {
		var archived bool
		if err := storage.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM secondbox.workspace_archives WHERE sandbox_id=$1)`, sandboxIDs[index]).Scan(&archived); err != nil {
			t.Fatal(err)
		}
		if archived != candidate.want {
			t.Errorf("pressure candidate %s archive=%t want=%t", candidate.suffix, archived, candidate.want)
		}
	}
	if err := storage.archivePressureWorkspaces(t.Context(), now, profiles); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := storage.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.workspace_archives WHERE source_runner_id='pressure-warning-source'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("pressure staging burst queued %d archives on one Runner", queued)
	}
}
