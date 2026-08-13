package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestConcurrentHomePlacementDoesNotOversubscribeOneRunner(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spec := placementTestSpec("pool-placement-capacity")
	seedPlacementRunner(t, store, spec.Pool, "runner-placement-capacity", now)
	seedPlacementProfileRevision(t, store, "revision-placement-capacity", spec, now)

	first, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(t.Context())
	selected, err := selectInitialHomeRunner(t.Context(), first, spec)
	if err != nil || selected != "runner-placement-capacity" {
		t.Fatalf("first placement selected=%q error=%v", selected, err)
	}
	insertPlacementReservation(
		t, first, "capacity", "revision-placement-capacity", selected, spec.Resources.WorkspaceBytes, now,
	)

	second, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(t.Context())
	secondContext, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	selection := make(chan runnerPlacementResult, 1)
	go func() {
		runnerID, err := selectInitialHomeRunner(secondContext, second, spec)
		selection <- runnerPlacementResult{runnerID: runnerID, err: err}
	}()
	select {
	case result := <-selection:
		t.Fatalf("concurrent placement did not wait: selected=%q error=%v", result.runnerID, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-selection:
		if !errors.Is(result.err, ports.ErrHomeRunnerUnavailable) || result.runnerID != "" {
			t.Fatalf("concurrent placement selected=%q error=%v", result.runnerID, result.err)
		}
	case <-secondContext.Done():
		t.Fatal(secondContext.Err())
	}
	if err := second.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	afterCommit, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer afterCommit.Rollback(t.Context())
	if selected, err := selectInitialHomeRunner(t.Context(), afterCommit, spec); !errors.Is(err, ports.ErrHomeRunnerUnavailable) || selected != "" {
		t.Fatalf("post-commit placement selected=%q error=%v", selected, err)
	}
	var reservations int
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.workspaces
		WHERE home_runner_id='runner-placement-capacity'`,
	).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("durable home reservations = %d, want 1", reservations)
	}
}

func TestDurableHomeReservationIsDiskOnlyAndReportedComputeStillApplies(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 4, 12, 30, 0, 0, time.UTC)
	spec := placementTestSpec("pool-placement-disk-only")
	runnerID := "runner-placement-disk-only"
	seedPlacementRunner(t, store, spec.Pool, runnerID, now)
	seedPlacementProfileRevision(t, store, "revision-placement-disk-only", spec, now)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET capacity_json=jsonb_set(capacity_json,'{DiskBytes}','2147483648'::jsonb)
		WHERE id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}

	first, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectInitialHomeRunner(t.Context(), first, spec)
	if err != nil || selected != runnerID {
		t.Fatalf("first placement selected=%q error=%v", selected, err)
	}
	insertPlacementReservation(
		t, first, "disk-only", "revision-placement-disk-only", runnerID,
		spec.Resources.WorkspaceBytes, now,
	)
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	read, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := durableHomeReservation(t.Context(), read, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved != (runnerCapacity{DiskBytes: spec.Resources.WorkspaceBytes}) {
		t.Fatalf("durable home reservation = %#v", reserved)
	}
	selected, err = selectInitialHomeRunner(t.Context(), read, spec)
	if err != nil || selected != runnerID {
		t.Fatalf("stopped-home compute admission selected=%q error=%v", selected, err)
	}
	if err := read.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET reserved_capacity_json='{"Instances":1}'
		WHERE id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}
	reported, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reported.Rollback(t.Context())
	if selected, err := selectInitialHomeRunner(t.Context(), reported, spec); !errors.Is(err, ports.ErrHomeRunnerUnavailable) || selected != "" {
		t.Fatalf("reported compute reservation selected=%q error=%v", selected, err)
	}
}

func TestConcurrentHomePlacementOnDistinctRunnersDoesNotWaitForFleet(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	spec := placementTestSpec("pool-placement-distinct")
	seedPlacementRunner(t, store, spec.Pool, "runner-placement-distinct-a", now)
	seedPlacementRunner(t, store, spec.Pool, "runner-placement-distinct-b", now)

	first, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(t.Context())
	selected, err := selectInitialHomeRunner(t.Context(), first, spec)
	if err != nil || selected != "runner-placement-distinct-a" {
		t.Fatalf("first placement selected=%q error=%v", selected, err)
	}

	second, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(t.Context())
	secondContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	selected, err = selectInitialHomeRunner(secondContext, second, spec)
	if err != nil || selected != "runner-placement-distinct-b" {
		t.Fatalf("non-blocking second placement selected=%q error=%v", selected, err)
	}
	if err := second.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestHomePlacementWaitsWhenEveryCompatibleRunnerIsLocked(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 4, 13, 15, 0, 0, time.UTC)
	spec := placementTestSpec("pool-placement-all-locked")
	runnerID := "runner-placement-all-locked"
	seedPlacementRunner(t, store, spec.Pool, runnerID, now)

	blocker, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `
		SELECT id FROM secondbox.runners WHERE id=$1 FOR UPDATE`, runnerID); err != nil {
		t.Fatal(err)
	}

	waiter, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback(t.Context())
	waitContext, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	selection := make(chan runnerPlacementResult, 1)
	go func() {
		selected, err := selectInitialHomeRunner(waitContext, waiter, spec)
		selection <- runnerPlacementResult{runnerID: selected, err: err}
	}()
	select {
	case result := <-selection:
		t.Fatalf("all-locked placement did not wait: selected=%q error=%v", result.runnerID, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-selection:
		if result.err != nil || result.runnerID != runnerID {
			t.Fatalf("all-locked placement selected=%q error=%v", result.runnerID, result.err)
		}
	case <-waitContext.Done():
		t.Fatal(waitContext.Err())
	}
	if err := waiter.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestExactHomePlacementWaitsForRunnerLock(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 4, 13, 30, 0, 0, time.UTC)
	spec := placementTestSpec("pool-placement-exact")
	runnerID := "runner-placement-exact"
	seedPlacementRunner(t, store, spec.Pool, runnerID, now)

	blocker, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `
		SELECT id FROM secondbox.runners WHERE id=$1 FOR UPDATE`, runnerID); err != nil {
		t.Fatal(err)
	}

	waiter, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback(t.Context())
	waitContext, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	selection := make(chan runnerPlacementResult, 1)
	go func() {
		selected, err := selectRunnerForPlacement(
			waitContext,
			waiter,
			spec,
			runnerPlacementOptions{
				exactRunnerID: runnerID,
				unavailable:   ports.ErrHomeRunnerUnavailable,
				errorPrefix:   "SecondBox exact home Runner test",
			},
		)
		selection <- runnerPlacementResult{runnerID: selected, err: err}
	}()
	select {
	case result := <-selection:
		t.Fatalf("exact placement did not wait: selected=%q error=%v", result.runnerID, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-selection:
		if result.err != nil || result.runnerID != runnerID {
			t.Fatalf("exact placement selected=%q error=%v", result.runnerID, result.err)
		}
	case <-waitContext.Done():
		t.Fatal(waitContext.Err())
	}
	if err := waiter.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type runnerPlacementResult struct {
	runnerID string
	err      error
}

func placementTestSpec(poolName string) contracts.ProfileRevisionSpec {
	return contracts.ProfileRevisionSpec{
		Pool: poolName, Architecture: "amd64",
		Resources: contracts.ResourcePolicy{
			VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 1 << 30,
			ConcurrentOperations: 1,
		},
	}
}

func seedPlacementRunner(
	t *testing.T,
	store *PostgresControlPlaneStore,
	poolName string,
	runnerID string,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			$1,$2,$1,'ready','["amd64"]','["compute","local-workspace"]',
			'{"VCPUCount":1,"MemoryBytes":1073741824,"DiskBytes":1073741824,"Instances":1,"Operations":1}',
			'["1"]',1,1,'test','connection-' || $1,0,'active','{}','[]',0,0,$3,1,$3,$3
		)`, runnerID, poolName, now); err != nil {
		t.Fatal(err)
	}
}

func seedPlacementProfileRevision(
	t *testing.T,
	store *PostgresControlPlaneStore,
	revisionID string,
	spec contracts.ProfileRevisionSpec,
	now time.Time,
) {
	t.Helper()
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ($1,$1,1,$2,$3)`, revisionID, specJSON, now); err != nil {
		t.Fatal(err)
	}
}

func insertPlacementReservation(
	t *testing.T,
	tx pgx.Tx,
	suffix string,
	revisionID string,
	runnerID string,
	workspaceBytes int64,
	now time.Time,
) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES ($1,'tenant','subject',$2,$3,'creating',$4,1,'','','','',NULL,NULL,'','{}',$5,$5);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES ($2,'tenant','subject',$6,$6,'creating','stopped',1,$1,'','{}','{}',1,$5,$5)`,
		pgx.QueryExecModeSimpleProtocol,
		"workspace-placement-"+suffix,
		"sandbox-placement-"+suffix,
		runnerID,
		workspaceBytes,
		now,
		revisionID,
	); err != nil {
		t.Fatal(err)
	}
}

// TestRunnerPlacementRequiresSnapshotResumeCapacityForResumeProfiles pins the
// second, independent placement filter. A Sandbox never leaves its home Runner,
// so the Runner chosen at creation has to satisfy the startup mode the Profile
// revision pins for every Instance it will ever host.
func TestRunnerPlacementRequiresSnapshotResumeCapacityForResumeProfiles(t *testing.T) {
	coldOnly := runnerPlacementCandidate{
		id: "runner-cold-only", poolName: "pool", state: "ready", drainPhase: "active",
		activeConnectionID: "connection", architectures: []string{"amd64"},
		capabilities: []string{"compute", "local-workspace"},
		allocatable: runnerCapacity{
			VCPUCount: 4, MemoryBytes: 8 << 30, DiskBytes: 8 << 30, Instances: 4, Operations: 8,
		},
	}
	resumeCapable := coldOnly
	resumeCapable.id = "runner-resume"
	resumeCapable.capabilities = []string{"compute", "local-workspace", "snapshot-resume"}

	coldSpec := placementTestSpec("pool")
	coldSpec.Startup = contracts.StartupPolicy{Mode: contracts.StartupModeColdBoot}
	resumeSpec := placementTestSpec("pool")
	resumeSpec.Startup = contracts.StartupPolicy{Mode: contracts.StartupModeSnapshotResume}

	for _, testCase := range []struct {
		name      string
		candidate runnerPlacementCandidate
		spec      contracts.ProfileRevisionSpec
		want      bool
	}{
		{"cold Profile on cold Runner", coldOnly, coldSpec, true},
		{"cold Profile on resume Runner", resumeCapable, coldSpec, true},
		{"resume Profile on cold Runner", coldOnly, resumeSpec, false},
		{"resume Profile on resume Runner", resumeCapable, resumeSpec, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := runnerPlacementCompatible(
				testCase.candidate, testCase.spec, runnerPlacementOptions{}, runnerCapacity{},
			)
			if got != testCase.want {
				t.Fatalf("compatible = %t, want %t", got, testCase.want)
			}
		})
	}
}
