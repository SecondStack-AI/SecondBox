package integration_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestCheckpointReservationsAreTransactionalAndInterruptedUploadsAreCollected(t *testing.T) {
	projectQuota := generousQuota()
	projectQuota.MaxRetainedBytes = 8192
	controlPlane, databaseStore := newControlPlaneFixture(t, projectQuota)
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "checkpoint-reservation",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-checkpoint-reservation",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "checkpoint-reservation-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	publications := []ports.CheckpointPublicationInput{
		checkpointReservationPublication(
			"chk_reservation_a", sandbox.Workspace.ID, sandbox.Generation, 6000, now.Add(-time.Hour),
		),
		checkpointReservationPublication(
			"chk_reservation_b", sandbox.Workspace.ID, sandbox.Generation, 6000, now.Add(-time.Hour),
		),
	}
	type stageResult struct {
		checkpoint contracts.WorkspaceCheckpoint
		err        error
	}
	results := make(chan stageResult, len(publications))
	var waitGroup sync.WaitGroup
	for _, publication := range publications {
		publication := publication
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			checkpoint, err := databaseStore.StageCheckpoint(t.Context(), publication)
			results <- stageResult{checkpoint: checkpoint, err: err}
		}()
	}
	waitGroup.Wait()
	close(results)
	var staged, quotaFailed int
	for result := range results {
		switch {
		case result.err == nil && result.checkpoint.State == contracts.ObjectStateStaging:
			staged++
		case errors.Is(result.err, ports.ErrQuotaExceeded):
			quotaFailed++
		default:
			t.Fatalf("checkpoint reservation result = %#v, %v", result.checkpoint, result.err)
		}
	}
	if staged != 1 || quotaFailed != 1 {
		t.Fatalf("checkpoint reservations: staged=%d quota_failed=%d", staged, quotaFailed)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var stagingCount, quotaFailureCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT
			count(*) FILTER (WHERE state='staging'),
			count(*) FILTER (WHERE state='quota_failed')
		FROM secondbox.workspace_checkpoints
		WHERE id IN ('chk_reservation_a','chk_reservation_b')`,
	).Scan(&stagingCount, &quotaFailureCount); err != nil {
		t.Fatal(err)
	}
	if stagingCount != 1 || quotaFailureCount != 1 {
		t.Fatalf(
			"checkpoint durable reservation states: staging=%d quota_failed=%d",
			stagingCount, quotaFailureCount,
		)
	}

	initialCandidates, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), now, time.Hour, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(initialCandidates) != 0 {
		t.Fatalf("checkpoint candidates before grace = %#v", initialCandidates)
	}
	candidates, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), now.Add(2*time.Hour), time.Hour, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("checkpoint candidates after grace = %#v", candidates)
	}
	for _, candidate := range candidates {
		if candidate.Kind != "checkpoint" {
			t.Fatalf("checkpoint garbage candidate = %#v", candidate)
		}
		if err := databaseStore.CompleteGarbageObject(
			t.Context(), candidate, now.Add(2*time.Hour),
		); err != nil {
			t.Fatal(err)
		}
	}
	var retainedBytes int64
	if err := pool.QueryRow(t.Context(), `
		SELECT retained_bytes FROM secondbox.workspaces WHERE id=$1`,
		sandbox.Workspace.ID,
	).Scan(&retainedBytes); err != nil {
		t.Fatal(err)
	}
	if retainedBytes != 0 {
		t.Fatalf("Workspace retained bytes after interrupted checkpoint cleanup = %d", retainedBytes)
	}
	replacement := checkpointReservationPublication(
		"chk_reservation_replacement", sandbox.Workspace.ID, sandbox.Generation,
		7000, now.Add(24*time.Hour),
	)
	if checkpoint, err := databaseStore.StageCheckpoint(
		t.Context(), replacement,
	); err != nil || checkpoint.State != contracts.ObjectStateStaging {
		t.Fatalf("checkpoint reservation after cleanup = %#v, %v", checkpoint, err)
	}
}

func checkpointReservationPublication(
	id string,
	workspaceID string,
	generation int64,
	sizeBytes int64,
	retainUntil time.Time,
) ports.CheckpointPublicationInput {
	return ports.CheckpointPublicationInput{
		Checkpoint: contracts.WorkspaceCheckpoint{
			ID: id, WorkspaceID: workspaceID, SourceGeneration: generation,
			SHA256:    "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			SizeBytes: sizeBytes, Compatibility: map[string]string{"formatVersion": "1"},
			RetainUntil: retainUntil, CreatedAt: retainUntil.Add(-time.Hour),
		},
		StorageKey: "checkpoints/" + id, ExpectedWorkspaceGeneration: generation,
	}
}
