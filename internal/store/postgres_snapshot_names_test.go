package store

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestSnapshotNameConflictIsScopedToReadySandboxSnapshots(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	workspace, sandbox := seedLocalWorkspace(t, store, "name-conflict", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	existing := seedReadyLocalSnapshot(t, store, "held-name", sandbox, workspace, now)
	input := localSnapshotCreateInput(sandbox, "snapshot-name-new", "name-conflict", now)
	input.Snapshot.Name = "held-name"
	if _, err := store.CreateSnapshot(t.Context(), input); !errors.Is(err, ports.ErrSnapshotNameConflict) {
		t.Fatalf("duplicate error=%v", err)
	}
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.operations WHERE id=$1`, input.Operation.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("conflict allocated an Operation")
	}
	_, other := seedLocalWorkspace(t, store, "name-other-sandbox", now)
	otherInput := localSnapshotCreateInput(other, "snapshot-name-other", "name-other-sandbox", now)
	otherInput.Snapshot.Name = "held-name"
	if _, err := store.CreateSnapshot(t.Context(), otherInput); err != nil {
		t.Fatalf("different Sandbox: %v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.snapshots SET state='deleted' WHERE id=$1`, existing); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSnapshot(t.Context(), input)
	if err != nil {
		t.Fatalf("reuse deleted name: %v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.snapshots SET state='ready' WHERE id=$1`, input.Snapshot.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := store.CreateSnapshot(t.Context(), input)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay=%s err=%v", replay.ID, err)
	}
}

func TestSnapshotNameMigrationReportsExistingDuplicates(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)
	workspace, sandbox := seedLocalWorkspace(t, store, "migration-names", now)
	first := seedReadyLocalSnapshot(t, store, "migration-name-first", sandbox, workspace, now)
	second := seedReadyLocalSnapshot(t, store, "migration-name-second", sandbox, workspace, now)
	migration, err := os.ReadFile("../../migrations/postgres/0024_snapshot_name_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `DROP INDEX secondbox.snapshots_sandbox_ready_name_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE secondbox.snapshots SET name='migration-duplicate' WHERE id IN ($1,$2)`, first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), string(migration)); err == nil || !strings.Contains(err.Error(), "Delete duplicates by Snapshot identifier before upgrading") || !strings.Contains(err.Error(), sandbox+"/migration-duplicate") {
		t.Fatalf("migration error=%v", err)
	}
}
