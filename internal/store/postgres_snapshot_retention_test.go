package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestUnlimitedSnapshotRetentionPersistsReadsClonesAndSurvivesExpiry(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "unlimited-retention", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	var specJSON []byte
	if err := store.pool.QueryRow(t.Context(), `
		SELECT spec_json FROM secondbox.profile_revisions WHERE id='revision-local'`).Scan(&specJSON); err != nil {
		t.Fatal(err)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		t.Fatal(err)
	}
	spec.Pool = "pool-unlimited-retention"
	spec.Retention.SnapshotRetentionSeconds = contracts.Unlimited
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	seedPlacementRunner(t, store, spec.Pool, "runner-unlimited-retention", now)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (id,profile_name,revision_number,spec_json,created_at)
		VALUES ('revision-unlimited-retention','profile-unlimited-retention',1,$1,$2);
		UPDATE secondbox.sandboxes SET profile_name='profile-unlimited-retention',
		    profile_revision_id='revision-unlimited-retention' WHERE id=$3;
		UPDATE secondbox.workspaces SET home_runner_id='runner-unlimited-retention' WHERE id=$4;
		UPDATE secondbox.runners
		SET capacity_json='{"VCPUCount":8,"MemoryBytes":8589934592,"DiskBytes":34359738368,"Instances":8,"Operations":32}'
		WHERE id='runner-unlimited-retention'`,
		pgx.QueryExecModeSimpleProtocol, string(specJSON), now, sandboxID, workspaceID); err != nil {
		t.Fatal(err)
	}
	create := localSnapshotCreateInput(sandboxID, "snapshot-unlimited", "unlimited-retention", now)
	create.Snapshot.RetainUntil = nil
	operation, err := store.CreateSnapshot(t.Context(), create)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Snapshot == nil || operation.Snapshot.RetainUntil != nil {
		t.Fatalf("unlimited Snapshot creation = %#v", operation.Snapshot)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.snapshots SET state='ready' WHERE id=$1;
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state=''
		WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, create.Snapshot.ID, workspaceID); err != nil {
		t.Fatal(err)
	}
	later := now.Add(365 * 24 * time.Hour)
	snapshot, err := store.GetSnapshot(t.Context(), "tenant-local", "subject-local", create.Snapshot.ID, later)
	if err != nil || snapshot.RetainUntil != nil || snapshot.State != "ready" {
		t.Fatalf("unlimited Snapshot retrieval = %#v, %v", snapshot, err)
	}
	page, err := store.ListSnapshots(t.Context(), "tenant-local", "subject-local", sandboxID, 10, "", later)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != snapshot.ID || page.Items[0].RetainUntil != nil {
		t.Fatalf("unlimited Snapshot listing = %#v, %v", page, err)
	}
	_, err = store.QueueExpiredSnapshotDelete(t.Context(), ports.SnapshotRetentionInput{
		OperationID: "expiry-operation", EffectID: "expiry-effect", CommandID: "expiry-command",
		RequestID: "expiry-request", FencingToken: create.FencingToken, Now: later,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.GetSnapshot(t.Context(), "tenant-local", "subject-local", snapshot.ID, later)
	if err != nil || snapshot.State != "ready" || snapshot.RetainUntil != nil {
		t.Fatalf("unlimited Snapshot after expiry processing = %#v, %v", snapshot, err)
	}
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	home, err := selectSnapshotCloneHomeRunner(t.Context(), tx, contracts.Principal{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
	}, snapshot.ID, spec, nil, later)
	if err != nil || home != "runner-unlimited-retention" {
		t.Fatalf("unlimited Snapshot clone home = %q, %v", home, err)
	}
}
