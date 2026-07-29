package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

func TestDurableLifecycleGenerationActivityAndWorkspaceAuthority(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "lifecycle")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-lifecycle")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "lifecycle-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	var waitGroup sync.WaitGroup
	operations := make(chan contracts.Operation, 2)
	failures := make(chan error, 2)
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			operation, startErr := controlPlane.StartSandbox(
				t.Context(), principal, sandbox.ID, "lifecycle-start", sandbox.Revision,
			)
			if startErr != nil {
				failures <- startErr
				return
			}
			operations <- operation
		}()
	}
	waitGroup.Wait()
	close(operations)
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	var operationID string
	for operation := range operations {
		if operationID == "" {
			operationID = operation.ID
		}
		if operation.ID != operationID {
			t.Fatalf("concurrent start operations diverged: %q and %q", operationID, operation.ID)
		}
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	reconciler := lifecycle.Reconciler{
		Store:         databaseStore,
		Effects:       &integrationLifecycleEffects{store: databaseStore, pool: pool},
		WorkerID:      "lifecycle-test-worker",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	)
	if err != nil || !found || decision.Action != lifecycle.ActionMaterialize {
		t.Fatalf("durable start reconciliation = %#v, %t, %v", decision, found, err)
	}
	afterStart, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterStart.State != contracts.SandboxStateStarting {
		t.Fatalf("reconciled start state = %q, want starting", afterStart.State)
	}
	checkpointOperation, err := controlPlane.CheckpointSandbox(
		t.Context(), principal, sandbox.ID, "checkpoint-request", afterStart.Revision,
		map[string]string{"label": "portable"},
	)
	if err != nil {
		t.Fatal(err)
	}
	replayedCheckpoint, err := controlPlane.CheckpointSandbox(
		t.Context(), principal, sandbox.ID, "checkpoint-request", afterStart.Revision,
		map[string]string{"label": "portable"},
	)
	if err != nil || replayedCheckpoint.ID != checkpointOperation.ID {
		t.Fatalf("checkpoint replay = %#v, %v", replayedCheckpoint, err)
	}
	if _, err := controlPlane.CheckpointSandbox(
		t.Context(), principal, sandbox.ID, "checkpoint-request", afterStart.Revision,
		map[string]string{"label": "different"},
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("checkpoint idempotency conflict error = %v", err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "stale-stop-request", afterStart.Revision,
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale lifecycle revision error = %v", err)
	}
	var intentKind, requestLabel string
	if err := pool.QueryRow(t.Context(), `
		SELECT lifecycle_intent_kind,lifecycle_request_metadata_json->>'label'
		FROM secondbox.sandboxes WHERE id=$1`,
		sandbox.ID,
	).Scan(&intentKind, &requestLabel); err != nil {
		t.Fatal(err)
	}
	if intentKind != "checkpoint" || requestLabel != "portable" {
		t.Fatalf("durable checkpoint intent = %q metadata label %q", intentKind, requestLabel)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	instanceID := "ins_lifecycle"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		instanceID, sandbox.ID, sandbox.Generation, now.Add(-2*time.Minute), now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',current_instance_id=$1,last_activity_at=$3,updated_at=$3
		WHERE id=$2`,
		instanceID, sandbox.ID, now.Add(-2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	beforePing, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.ReportGuestLiveness(
		t.Context(), principal, sandbox.ID, sandbox.Generation, contracts.GuestLivenessReady,
	); err != nil {
		t.Fatal(err)
	}
	afterPing, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterPing.LastActivityAt.Equal(*beforePing.LastActivityAt) {
		t.Fatalf("ping changed useful activity from %s to %s", beforePing.LastActivityAt, afterPing.LastActivityAt)
	}

	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "lifecycle-lease-acquire", 30,
	)
	if err != nil {
		t.Fatal(err)
	}
	touchedAt, err := controlPlane.TouchSandbox(
		t.Context(), principal, sandbox.ID, sandbox.Generation, lease.ID, "lifecycle-touch-current",
	)
	if err != nil || !touchedAt.LastActivityAt.Equal(now) {
		t.Fatalf("touch = %#v, %v", touchedAt, err)
	}
	session, err := controlPlane.OpenActivitySession(
		t.Context(), principal, sandbox.ID, sandbox.Generation, lease.ID,
		contracts.ActivitySessionKindExec,
	)
	if err != nil || session.State != contracts.ActivitySessionStateActive {
		t.Fatalf("active session = %#v, %v", session, err)
	}
	if _, err := controlPlane.TouchSandbox(
		t.Context(), principal, sandbox.ID, sandbox.Generation+1, lease.ID, "lifecycle-touch-stale",
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale generation touch error = %v, want generation fence", err)
	}
	if _, err := controlPlane.CloseActivitySession(
		t.Context(), principal, sandbox.ID, sandbox.Generation, session.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='stopped',termination_reason='requested_stop',
		    stopped_at=$2,updated_at=$2 WHERE id=$1;
	`, instanceID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='stopped',desired_state='stopped',
		    current_instance_id='',updated_at=$2 WHERE id=$1`,
		sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}

	materialization := contracts.WorkspaceMaterialization{
		ID: "mat_lifecycle_a", WorkspaceID: sandbox.Workspace.ID, SandboxID: sandbox.ID,
		AssignmentID: "asn_lifecycle_a", RunnerID: "run_lifecycle_a",
		Generation: sandbox.Generation, CreatedAt: now,
	}
	acquired, err := databaseStore.AcquireMaterialization(t.Context(), ports.MaterializationInput{
		Materialization: materialization, ExpectedWorkspaceGeneration: sandbox.Generation,
	})
	if err != nil || acquired.State != contracts.MaterializationStatePreparing {
		t.Fatalf("materialization = %#v, %v", acquired, err)
	}
	acquired, err = databaseStore.ConfirmMaterialization(
		t.Context(),
		ports.MaterializationInput{
			Materialization: acquired, ExpectedWorkspaceGeneration: sandbox.Generation,
		},
		now,
	)
	if err != nil || acquired.State != contracts.MaterializationStateReady {
		t.Fatalf("materialization confirmation = %#v, %v", acquired, err)
	}
	conflict := materialization
	conflict.ID, conflict.AssignmentID, conflict.RunnerID = "mat_lifecycle_b", "asn_lifecycle_b", "run_lifecycle_b"
	if _, err := databaseStore.AcquireMaterialization(t.Context(), ports.MaterializationInput{
		Materialization: conflict, ExpectedWorkspaceGeneration: sandbox.Generation,
	}); !errors.Is(err, ports.ErrMaterializationConflict) {
		t.Fatalf("concurrent materialization error = %v", err)
	}
	if _, err := databaseStore.ReleaseMaterialization(
		t.Context(),
		ports.MaterializationInput{
			Materialization: acquired, ExpectedWorkspaceGeneration: sandbox.Generation,
		},
		map[string]string{"digest": "sha256:release"}, now,
	); err != nil {
		t.Fatal(err)
	}

	checkpoint := contracts.WorkspaceCheckpoint{
		ID: "chk_lifecycle", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 4096, Compatibility: map[string]string{"architecture": "amd64"},
		RetainUntil: now.Add(time.Hour), CreatedAt: now,
	}
	publication := ports.CheckpointPublicationInput{
		Checkpoint: checkpoint, StorageKey: "checkpoints/chk_lifecycle",
		ExpectedWorkspaceGeneration: sandbox.Generation,
	}
	badPublication := publication
	badPublication.Checkpoint.ID = "chk_lifecycle_bad"
	badPublication.StorageKey = "checkpoints/chk_lifecycle_bad"
	if _, err := databaseStore.StageCheckpoint(t.Context(), badPublication); err != nil {
		t.Fatal(err)
	}
	mismatched := badPublication
	mismatched.Checkpoint.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := databaseStore.PublishCheckpoint(t.Context(), mismatched, now); !errors.Is(err, ports.ErrCheckpointIntegrity) {
		t.Fatalf("mismatched checkpoint publication error = %v", err)
	}
	var failedCheckpointState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.workspace_checkpoints WHERE id=$1`,
		badPublication.Checkpoint.ID,
	).Scan(&failedCheckpointState); err != nil {
		t.Fatal(err)
	}
	if failedCheckpointState != contracts.ObjectStateIntegrityFailed {
		t.Fatalf("mismatched checkpoint state = %q", failedCheckpointState)
	}
	if _, err := databaseStore.StageCheckpoint(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.VerifyCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	published, err := databaseStore.PublishCheckpoint(t.Context(), publication, now)
	if err != nil || published.State != contracts.ObjectStatePublished {
		t.Fatalf("checkpoint publication = %#v, %v", published, err)
	}
	reloaded, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Workspace.CurrentCheckpointID != checkpoint.ID ||
		reloaded.Workspace.CurrentCheckpointHash != checkpoint.SHA256 ||
		reloaded.Workspace.CurrentCheckpointSize != checkpoint.SizeBytes {
		t.Fatalf("published Workspace evidence = %#v", reloaded.Workspace)
	}

	restored := contracts.WorkspaceMaterialization{
		ID: "mat_lifecycle_restore", WorkspaceID: sandbox.Workspace.ID, SandboxID: sandbox.ID,
		AssignmentID: "asn_lifecycle_restore", RunnerID: "run_lifecycle_other",
		Generation: sandbox.Generation, SourceCheckpointID: checkpoint.ID, CreatedAt: now,
	}
	restored, err = databaseStore.AcquireMaterialization(t.Context(), ports.MaterializationInput{
		Materialization: restored, ExpectedWorkspaceGeneration: sandbox.Generation,
	})
	if err != nil {
		t.Fatalf("cross-runner stopped-checkpoint restore authority failed: %v", err)
	}
	if _, err := databaseStore.ConfirmMaterialization(
		t.Context(),
		ports.MaterializationInput{
			Materialization: restored, ExpectedWorkspaceGeneration: sandbox.Generation,
		},
		now,
	); err != nil {
		t.Fatalf("cross-runner materialization confirmation failed: %v", err)
	}
	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, lease.ID, "lifecycle-lease-release",
	); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.instances
		SET state='ready',guest_liveness='ready',termination_reason='',ready_at=$2,
		    guest_heartbeat_at=$2,maximum_duration_at=$3,stopped_at=NULL,updated_at=$2
		WHERE id=$1`,
		instanceID, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,backend_reference,
			generation,fencing_token,state,capability_snapshot_json,resolved_artifacts_json,
			release_proof_json,failure_class,retry_count,retry_limit,operation_deadline,
			claim_expires_at,reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,
			revision,created_at,updated_at
		) VALUES (
			'asn_lifecycle_restore',$1,$2,'run_lifecycle_other',$3,'firecracker','vm-lost',
			$4,$5,'active','{}','{}','{}','',0,3,$6,$6,'worker',$6,$7,1,$8,$8
		)`,
		sandbox.ID, instanceID, sandbox.ProfileRevisionID, sandbox.Generation,
		[]byte("fencing-token"), now.Add(time.Minute), now.Add(24*time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id=$2,updated_at=$3
		WHERE id=$1`,
		sandbox.ID, instanceID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.assignments
		SET state='fenced',failure_class='fencing',
		    release_proof_json='{"terminationEvidenceDigest":"sha256:termination-evidence"}',
		    revision=revision+1,updated_at=$2
		WHERE id=$1`, "asn_lifecycle_restore", now,
	); err != nil {
		t.Fatal(err)
	}
	runnerLossStore, err := reconcile.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runnerLossStore.Close()
	nextGeneration, err := runnerLossStore.AdvanceFencedGeneration(
		t.Context(), "asn_lifecycle_restore", 0, now,
	)
	if err != nil || nextGeneration != sandbox.Generation+1 {
		t.Fatalf("runner-loss generation fence = %d, %v", nextGeneration, err)
	}
	afterFence, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFence.Generation != nextGeneration || afterFence.Workspace.Generation != nextGeneration ||
		afterFence.Workspace.CurrentCheckpointID != checkpoint.ID || afterFence.Instance != nil {
		t.Fatalf("runner-loss authority = %#v", afterFence)
	}

	replacementCheckpoint := contracts.WorkspaceCheckpoint{
		ID: "chk_lifecycle_replacement", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: nextGeneration,
		SHA256:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		SizeBytes:        8192, Compatibility: map[string]string{"architecture": "amd64"},
		RetainUntil: now.Add(4 * time.Hour), CreatedAt: now.Add(time.Minute),
	}
	replacementPublication := ports.CheckpointPublicationInput{
		Checkpoint: replacementCheckpoint, StorageKey: "checkpoints/chk_lifecycle_replacement",
		ExpectedWorkspaceGeneration: nextGeneration,
	}
	if _, err := databaseStore.StageCheckpoint(t.Context(), replacementPublication); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.VerifyCheckpoint(
		t.Context(), replacementPublication, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.PublishCheckpoint(t.Context(), replacementPublication, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, artifactPolicy, err := databaseStore.GetSandboxLifecyclePolicy(
		t.Context(), sandbox.TenantRef, sandbox.SubjectRef, sandbox.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact := contracts.Artifact{
		ID: "art_lifecycle", ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID,
		TenantRef: sandbox.TenantRef, SubjectRef: sandbox.SubjectRef,
		SourceGeneration: nextGeneration, Name: "result.tar", MediaType: "application/x-tar",
		SizeBytes: 1024,
		SHA256:    "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Metadata:  map[string]string{"purpose": "integration"},
		RetainUntil: now.Add(
			time.Duration(artifactPolicy.ArtifactRetentionSeconds) * time.Second,
		),
		CreatedAt: now,
	}
	artifactPublication := ports.ArtifactPublicationInput{
		Artifact: artifact, StorageKey: "artifacts/art_lifecycle",
		ExpectedGeneration: nextGeneration,
	}
	badArtifactPublication := artifactPublication
	badArtifactPublication.Artifact.ID = "art_lifecycle_bad"
	badArtifactPublication.StorageKey = "artifacts/art_lifecycle_bad"
	if _, err := databaseStore.StageArtifact(t.Context(), badArtifactPublication); err != nil {
		t.Fatal(err)
	}
	mismatchedArtifact := badArtifactPublication
	mismatchedArtifact.Artifact.SizeBytes++
	if _, err := databaseStore.PublishArtifact(
		t.Context(), mismatchedArtifact, now,
	); !errors.Is(err, ports.ErrArtifactIntegrity) {
		t.Fatalf("mismatched Artifact publication error = %v", err)
	}
	var failedArtifactState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.artifacts WHERE id=$1`,
		badArtifactPublication.Artifact.ID,
	).Scan(&failedArtifactState); err != nil {
		t.Fatal(err)
	}
	if failedArtifactState != contracts.ObjectStateIntegrityFailed {
		t.Fatalf("mismatched Artifact state = %q", failedArtifactState)
	}
	if _, err := databaseStore.StageArtifact(t.Context(), artifactPublication); err != nil {
		t.Fatal(err)
	}
	publishedArtifact, err := databaseStore.PublishArtifact(t.Context(), artifactPublication, now)
	if err != nil || publishedArtifact.State != contracts.ObjectStatePublished {
		t.Fatalf("Artifact publication = %#v, %v", publishedArtifact, err)
	}
	garbageMarkAt := artifact.RetainUntil.Add(time.Hour)
	if candidates, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), garbageMarkAt, time.Minute, 10,
	); err != nil {
		t.Fatalf("garbage grace first pass = %#v, %v", candidates, err)
	} else {
		for _, candidate := range candidates {
			if candidate.ID == checkpoint.ID || candidate.ID == artifact.ID {
				t.Fatalf("own object bypassed garbage grace: %#v", candidate)
			}
		}
	}
	candidates, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), garbageMarkAt.Add(2*time.Minute), time.Minute, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	garbageIDs := map[string]bool{}
	for _, candidate := range candidates {
		garbageIDs[candidate.ID] = true
		if err := databaseStore.CompleteGarbageObject(
			t.Context(), candidate, garbageMarkAt.Add(2*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
	}
	if !garbageIDs[checkpoint.ID] || !garbageIDs[artifact.ID] ||
		garbageIDs[replacementCheckpoint.ID] {
		t.Fatalf("reachability-aware garbage candidates = %#v", candidates)
	}
}

func TestExpiredLeaseSessionCannotSuppressLifecycleReclamation(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "expired-session")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-expired-session")
	principal := authenticateCredential(t, controlPlane, credential)

	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "expired-session-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	instanceID := "instance-expired-session"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		instanceID, sandbox.ID, sandbox.Generation, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id=$1,
		    last_activity_at=$3,next_reconcile_at=$3,updated_at=$3
		WHERE id=$2`,
		instanceID, sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		"expired-session-lease", 30,
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := controlPlane.OpenActivitySession(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		lease.ID, contracts.ActivitySessionKindExec,
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := databaseStore.ClaimLifecycle(
		t.Context(), "expired-session-worker", now.Add(31*time.Second), time.Minute,
	)
	if err != nil || !found {
		t.Fatalf("lifecycle claim = %#v, %t, %v", claim, found, err)
	}
	if claim.SandboxID != sandbox.ID || claim.ActiveSessions != 0 {
		t.Fatalf("expired Lease lifecycle claim = %#v", claim)
	}
	var leaseState, sessionState string
	if err := pool.QueryRow(t.Context(), `
		SELECT lease.state,session.state
		FROM secondbox.leases AS lease
		JOIN secondbox.activity_sessions AS session ON session.lease_id=lease.id
		WHERE lease.id=$1 AND session.id=$2`,
		lease.ID, session.ID,
	).Scan(&leaseState, &sessionState); err != nil {
		t.Fatal(err)
	}
	if leaseState != contracts.LeaseStateExpired ||
		sessionState != contracts.ActivitySessionStateClosed {
		t.Fatalf("expired authority state = Lease %q, session %q", leaseState, sessionState)
	}
}

func TestCheckpointEffectDeadlineRetriesThenFailsTerminally(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "checkpoint-retry")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-checkpoint-retry")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "checkpoint-retry-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	current, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "checkpoint-retry-stop", current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, checkpointUnusedScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 1, SerializationRetryLimit: 1,
			AssetCatalog:     checkpointUnusedAssetCatalog{},
			SessionCanceller: checkpointUnusedSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-unused"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(effectBroker.Close)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker, WorkerID: "checkpoint-retry-worker",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("checkpoint retry drain = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now.Add(time.Millisecond)); err != nil ||
		!found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint retry initial queue = %#v, %t, %v", decision, found, err)
	}
	var effectID, commandID string
	if err := pool.QueryRow(t.Context(), `
		SELECT id,command_id FROM secondbox.lifecycle_effects
		WHERE sandbox_id=$1 AND kind='checkpoint'`,
		sandbox.ID,
	).Scan(&effectID, &commandID); err != nil {
		t.Fatal(err)
	}
	initialCommandID := commandID
	expireDelivery := func(at time.Time) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `
			UPDATE secondbox.lifecycle_effects SET effect_deadline=$2,updated_at=$2 WHERE id=$1`,
			effectID, at.Add(-time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `
			UPDATE secondbox.runner_commands
			SET state='delivered',target_connection_id='lost-connection',
			    delivered_at=$2,updated_at=$2
			WHERE id=$1`,
			commandID, at.Add(-time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}
	expireDelivery(now.Add(2 * time.Millisecond))
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(2*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint retry requeue = %#v, %t, %v", decision, found, err)
	}
	var effectState, commandState, failureClass, targetConnectionID string
	var retryCount int64
	var retryDeadline time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,effect.retry_count,effect.failure_class,effect.command_id,
		       effect.effect_deadline,
		       command.state,command.target_connection_id
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.id=$1`,
		effectID,
	).Scan(
		&effectState, &retryCount, &failureClass, &commandID, &retryDeadline,
		&commandState, &targetConnectionID,
	); err != nil {
		t.Fatal(err)
	}
	if effectState != "queued" || retryCount != 1 ||
		failureClass != "checkpoint_deadline_retry" ||
		commandID == initialCommandID ||
		!retryDeadline.After(now.Add(2*time.Millisecond)) ||
		commandState != "pending" || targetConnectionID != "" {
		t.Fatalf(
			"checkpoint retry evidence = effect %q retry %d failure %q command %q target %q",
			effectState, retryCount, failureClass, commandState, targetConnectionID,
		)
	}
	var initialCommandState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.runner_commands WHERE id=$1`,
		initialCommandID,
	).Scan(&initialCommandState); err != nil {
		t.Fatal(err)
	}
	if initialCommandState != "expired" {
		t.Fatalf("expired initial checkpoint command state = %q", initialCommandState)
	}
	expireDelivery(now.Add(3 * time.Millisecond))
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(3*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint retry exhaustion = %#v, %t, %v", decision, found, err)
	}
	var failureMessage string
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,effect.retry_count,effect.failure_class,effect.failure_message,
		       command.state
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.id=$1`,
		effectID,
	).Scan(
		&effectState, &retryCount, &failureClass, &failureMessage, &commandState,
	); err != nil {
		t.Fatal(err)
	}
	if effectState != "runner_failed" || retryCount != 1 ||
		failureClass != "checkpoint_retry_exhausted" ||
		failureMessage != "checkpoint command exhausted its delivery deadline retry bound" ||
		commandState != "failed" {
		t.Fatalf(
			"checkpoint exhaustion evidence = state %q retry %d class %q message %q command %q",
			effectState, retryCount, failureClass, failureMessage, commandState,
		)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(4*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionFail {
		t.Fatalf("checkpoint terminal lifecycle failure = %#v, %t, %v", decision, found, err)
	}
}

func TestStopEffectDeadlineRetriesThenFailsTerminally(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "stop-retry")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-stop-retry")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "stop-retry-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.profile_revisions
		SET spec_json=jsonb_set(spec_json,'{checkpoint,onStop}','false'::jsonb)
		WHERE id=$1`,
		sandbox.ProfileRevisionID,
	); err != nil {
		t.Fatal(err)
	}
	current, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "stop-retry-stop", current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, checkpointUnusedScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 1, SerializationRetryLimit: 1,
			AssetCatalog:     checkpointUnusedAssetCatalog{},
			SessionCanceller: checkpointUnusedSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-unused"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(effectBroker.Close)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker, WorkerID: "stop-retry-worker",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("stop retry drain = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now.Add(time.Millisecond)); err != nil ||
		!found || decision.Action != lifecycle.ActionStopInstance {
		t.Fatalf("stop retry initial queue = %#v, %t, %v", decision, found, err)
	}
	var queuedTerminationReason string
	if err := pool.QueryRow(t.Context(), `
		SELECT lifecycle_termination_reason FROM secondbox.sandboxes WHERE id=$1`,
		sandbox.ID,
	).Scan(&queuedTerminationReason); err != nil {
		t.Fatal(err)
	}
	if queuedTerminationReason != contracts.TerminationReasonRequestedStop {
		t.Fatalf(
			"queued production stop reason = %q, want %q",
			queuedTerminationReason, contracts.TerminationReasonRequestedStop,
		)
	}
	var effectID, commandID string
	if err := pool.QueryRow(t.Context(), `
		SELECT id,command_id FROM secondbox.lifecycle_effects
		WHERE sandbox_id=$1 AND kind='stop'`,
		sandbox.ID,
	).Scan(&effectID, &commandID); err != nil {
		t.Fatal(err)
	}
	initialCommandID := commandID
	expireStopDelivery := func(at time.Time) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `
			UPDATE secondbox.lifecycle_effects SET effect_deadline=$2,updated_at=$2 WHERE id=$1`,
			effectID, at.Add(-time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `
			UPDATE secondbox.runner_commands
			SET state='delivered',target_connection_id='lost-connection',
			    delivered_at=$2,updated_at=$2
			WHERE id=$1`,
			commandID, at.Add(-time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}
	expireStopDelivery(now.Add(2 * time.Millisecond))
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(2*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionStopInstance {
		t.Fatalf("stop retry requeue = %#v, %t, %v", decision, found, err)
	}
	var effectState, commandState, failureClass, targetConnectionID string
	var retryCount int64
	var retryDeadline time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,effect.retry_count,effect.failure_class,effect.command_id,
		       effect.effect_deadline,command.state,command.target_connection_id
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.id=$1`,
		effectID,
	).Scan(
		&effectState, &retryCount, &failureClass, &commandID, &retryDeadline,
		&commandState, &targetConnectionID,
	); err != nil {
		t.Fatal(err)
	}
	if effectState != "queued" || retryCount != 1 ||
		failureClass != "stop_deadline_retry" ||
		commandID == initialCommandID ||
		!retryDeadline.After(now.Add(2*time.Millisecond)) ||
		commandState != "pending" || targetConnectionID != "" {
		t.Fatalf(
			"stop retry evidence = effect %q retry %d failure %q command %q target %q",
			effectState, retryCount, failureClass, commandState, targetConnectionID,
		)
	}
	expireStopDelivery(now.Add(3 * time.Millisecond))
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(3*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionStopInstance {
		t.Fatalf("stop retry exhaustion = %#v, %t, %v", decision, found, err)
	}
	var failureMessage string
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,effect.retry_count,effect.failure_class,effect.failure_message,
		       command.state
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.id=$1`,
		effectID,
	).Scan(
		&effectState, &retryCount, &failureClass, &failureMessage, &commandState,
	); err != nil {
		t.Fatal(err)
	}
	if effectState != "runner_failed" || retryCount != 1 ||
		failureClass != "stop_retry_exhausted" ||
		failureMessage != "stop command exhausted its delivery deadline retry bound" ||
		commandState != "failed" {
		t.Fatalf(
			"stop exhaustion evidence = state %q retry %d class %q message %q command %q",
			effectState, retryCount, failureClass, failureMessage, commandState,
		)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(4*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionFail {
		t.Fatalf("stop terminal lifecycle failure = %#v, %t, %v", decision, found, err)
	}
}

func TestAssignmentWorkerBoundsMissingResultAndFenceResult(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "assignment-retry")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-assignment-retry")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "assignment-retry-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	startOperation, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "assignment-retry-start", sandbox.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	assignmentCommandID := "assignment-command-" + sandbox.ID
	assignmentPayload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Assignment{
			Assignment: &runnerv1.AssignmentCommand{
				Fence: seed.Fence, ProfileRevisionId: sandbox.ProfileRevisionID,
				DeadlineUnixMs: uint64(now.Add(-time.Millisecond).UnixMilli()),
				Correlation: &runnerv1.Correlation{
					RequestId: startOperation.RequestID, OperationId: startOperation.ID,
					SandboxId: sandbox.ID, InstanceId: seed.Fence.InstanceId,
					SandboxGeneration: seed.Fence.SandboxGeneration,
					AssignmentId:      seed.Fence.AssignmentId, RunnerId: seed.RunnerID,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.assignments
		SET state='starting',failure_class='',retry_count=0,retry_limit=1,
		    operation_deadline=$2,reconcile_owner='',reconcile_claim_expires_at=$2,
		    next_reconcile_at=$2,updated_at=$2
		WHERE id=$1`,
		seed.Fence.AssignmentId, now.Add(-time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='starting',desired_state='running',current_instance_id=$2,
		    next_reconcile_at=$3,updated_at=$3
		WHERE id=$1`,
		sandbox.ID, seed.Fence.InstanceId, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspace_materializations (
			id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
			source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at,released_at
		)
		SELECT 'mat-assignment-retry-' || sandbox.id,sandbox.workspace_id,sandbox.id,
		       $2,$3,sandbox.generation,'','preparing','{}',1,$4,$4,NULL
		FROM secondbox.sandboxes AS sandbox WHERE sandbox.id=$1`,
		sandbox.ID, seed.Fence.AssignmentId, seed.RunnerID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'assignment',$4,'delivered','lost-connection',1,$5,$5,$5)`,
		assignmentCommandID, seed.RunnerID, seed.Fence.AssignmentId, assignmentPayload, now,
	); err != nil {
		t.Fatal(err)
	}
	reconcileStore, err := reconcile.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reconcileStore.Close)
	commandSequence := 0
	worker := reconcile.AssignmentWorker{
		Store: reconcileStore, WorkerID: "assignment-retry-worker",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
		CommandDeadline: time.Minute, HeartbeatTimeout: time.Hour,
		NewCommandID: func(prefix string) string {
			commandSequence++
			return fmt.Sprintf("%s-%s-%d", prefix, sandbox.ID, commandSequence)
		},
	}
	if decision, found, err := worker.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != reconcile.ActionFence {
		t.Fatalf("assignment timeout fence = %#v, %t, %v", decision, found, err)
	}
	var assignmentState, failureClass, fenceCommandID, fenceCommandState string
	var retryCount int64
	var commandDeadline time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT assignment.state,assignment.failure_class,assignment.retry_count,
		       assignment.operation_deadline,command.id,command.state
		FROM secondbox.assignments AS assignment
		JOIN secondbox.runner_commands AS command
		  ON command.assignment_id=assignment.id AND command.kind='fence'
		WHERE assignment.id=$1 AND command.state='pending'`,
		seed.Fence.AssignmentId,
	).Scan(
		&assignmentState, &failureClass, &retryCount, &commandDeadline,
		&fenceCommandID, &fenceCommandState,
	); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "fencing" || failureClass != "startup_timeout" ||
		retryCount != 0 || !commandDeadline.After(now) ||
		fenceCommandState != "pending" {
		t.Fatalf(
			"assignment fence evidence = state %q failure %q retry %d deadline %s command %q",
			assignmentState, failureClass, retryCount, commandDeadline, fenceCommandState,
		)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.assignments
		SET operation_deadline=$2,reconcile_claim_expires_at=$2,next_reconcile_at=$2
		WHERE id=$1`,
		seed.Fence.AssignmentId, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runner_commands
		SET state='delivered',target_connection_id='lost-connection',delivered_at=$2,updated_at=$2
		WHERE id=$1`,
		fenceCommandID, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := worker.RunOnce(
		t.Context(), now.Add(2*time.Millisecond),
	); err != nil || !found || decision.Action != reconcile.ActionFence {
		t.Fatalf("fence result retry = %#v, %t, %v", decision, found, err)
	}
	var retryFenceCommandID string
	if err := pool.QueryRow(t.Context(), `
		SELECT assignment.retry_count,command.id
		FROM secondbox.assignments AS assignment
		JOIN secondbox.runner_commands AS command
		  ON command.assignment_id=assignment.id AND command.kind='fence'
		WHERE assignment.id=$1 AND command.state='pending'`,
		seed.Fence.AssignmentId,
	).Scan(&retryCount, &retryFenceCommandID); err != nil {
		t.Fatal(err)
	}
	if retryCount != 1 || retryFenceCommandID == fenceCommandID {
		t.Fatalf("fence retry evidence = retry %d command %q", retryCount, retryFenceCommandID)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.assignments
		SET operation_deadline=$2,reconcile_claim_expires_at=$2,next_reconcile_at=$2
		WHERE id=$1`,
		seed.Fence.AssignmentId, now.Add(2*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := worker.RunOnce(
		t.Context(), now.Add(3*time.Millisecond),
	); err != nil || !found || decision.Action != reconcile.ActionFailTerminal {
		t.Fatalf("fence result exhaustion = %#v, %t, %v", decision, found, err)
	}
	var sandboxState, operationState, terminalCommandState, startupTerminationReason string
	if err := pool.QueryRow(t.Context(), `
		SELECT assignment.state,sandbox.state,operation.state,command.state,
		       instance.termination_reason
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.operations AS operation ON operation.id=$2
		JOIN secondbox.runner_commands AS command ON command.id=$3
		WHERE assignment.id=$1`,
		seed.Fence.AssignmentId, startOperation.ID, retryFenceCommandID,
	).Scan(
		&assignmentState, &sandboxState, &operationState, &terminalCommandState,
		&startupTerminationReason,
	); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "failed_terminal" || sandboxState != contracts.SandboxStateFailed ||
		operationState != contracts.OperationStateFailed || terminalCommandState != "failed" ||
		startupTerminationReason != contracts.TerminationReasonStartupFailed {
		t.Fatalf(
			"assignment terminal evidence = assignment %q Sandbox %q Operation %q command %q reason %q",
			assignmentState, sandboxState, operationState, terminalCommandState,
			startupTerminationReason,
		)
	}
}

type checkpointUnusedScheduler struct{}

func (checkpointUnusedScheduler) Schedule(
	context.Context,
	scheduler.ScheduleRequest,
) (scheduler.DurableAssignment, bool, error) {
	return scheduler.DurableAssignment{}, false, errors.New("checkpoint retry test scheduler was unexpectedly called")
}

type checkpointUnusedAssetCatalog struct{}

func (checkpointUnusedAssetCatalog) Resolve(string) (lifecycle.SignedAsset, error) {
	return lifecycle.SignedAsset{}, errors.New("checkpoint retry test catalog was unexpectedly called")
}

type checkpointUnusedSessionCanceller struct{}

func (checkpointUnusedSessionCanceller) CancelSandboxSessions(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (int64, error) {
	return 0, errors.New("checkpoint retry test session cancellation was unexpectedly called")
}

func TestLifecycleWorkerPreservesRunnerEffectAndCompletesDatabaseOnlyDelete(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "worker-boundary")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-worker-boundary")
	principal := authenticateCredential(t, controlPlane, credential)

	runnerBound, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "worker-boundary-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	startOperation, err := controlPlane.StartSandbox(
		t.Context(), principal, runnerBound.ID, "worker-boundary-start", runnerBound.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		runnerBound.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	reconciler := lifecycle.Reconciler{
		Store: databaseStore,
		Effects: &integrationLifecycleEffects{
			store: databaseStore, pool: pool, createMaterialization: true,
		},
		WorkerID:      "worker-boundary",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	first, found, err := reconciler.RunOnce(t.Context(), now)
	if err != nil || !found || first.Action != lifecycle.ActionMaterialize {
		t.Fatalf("first runner-bound decision = %#v, %t, %v", first, found, err)
	}
	second, found, err := reconciler.RunOnce(t.Context(), now.Add(time.Millisecond))
	if err != nil || !found || second.Action != lifecycle.ActionWait {
		t.Fatalf("second runner-bound decision = %#v, %t, %v", second, found, err)
	}
	var durableAction string
	if err := pool.QueryRow(
		t.Context(), `SELECT lifecycle_action FROM secondbox.sandboxes WHERE id=$1`, runnerBound.ID,
	).Scan(&durableAction); err != nil {
		t.Fatal(err)
	}
	if durableAction != string(lifecycle.ActionMaterialize) {
		t.Fatalf("runner-facing action = %q, want durable materialize", durableAction)
	}
	pending, err := controlPlane.GetOperation(t.Context(), principal, startOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != contracts.OperationStatePending {
		t.Fatalf("runner-bound operation state = %q, want pending", pending.State)
	}
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id=$1`,
		runnerBound.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	databaseOnly, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "worker-delete-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if databaseOnly.State != contracts.SandboxStateStopped {
		t.Fatalf("create-to-stopped state = %q", databaseOnly.State)
	}
	supersededStart, err := controlPlane.StartSandbox(
		t.Context(), principal, databaseOnly.ID, "worker-boundary-delete-start", databaseOnly.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	databaseOnly, err = controlPlane.GetSandbox(t.Context(), principal, databaseOnly.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleteOperation, err := controlPlane.DeleteSandbox(
		t.Context(), principal, databaseOnly.ID, "worker-boundary-delete", databaseOnly.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	deleting, found, err := reconciler.RunOnce(t.Context(), now.Add(2*time.Millisecond))
	if err != nil || !found || deleting.Action != lifecycle.ActionDelete {
		t.Fatalf("database-only delete decision = %#v, %t, %v", deleting, found, err)
	}
	deleted, found, err := reconciler.RunOnce(t.Context(), now.Add(3*time.Millisecond))
	if err != nil || !found || deleted.Action != lifecycle.ActionFinishDelete {
		t.Fatalf("database-only terminal decision = %#v, %t, %v", deleted, found, err)
	}
	completed, err := controlPlane.GetOperation(t.Context(), principal, deleteOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != contracts.OperationStateSucceeded {
		t.Fatalf("database-only delete operation state = %q", completed.State)
	}
	superseded, err := controlPlane.GetOperation(t.Context(), principal, supersededStart.ID)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.State != contracts.OperationStateFailed ||
		superseded.Error == nil || superseded.Error.Code != "state_conflict" {
		t.Fatalf("delete-superseded start Operation = %#v", superseded)
	}
}

func TestFinishStopCompletesEveryCompatiblePendingLifecycleOperation(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "stop-operations")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-stop-operations")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "stop-operations-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	instanceID := "ins_stop_operations"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		instanceID, sandbox.ID, sandbox.Generation, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id=$1,last_activity_at=$3,
		    next_reconcile_at=$3,updated_at=$3
		WHERE id=$2`,
		instanceID, sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	drainOperation, err := controlPlane.DrainSandbox(
		t.Context(), principal, sandbox.ID, "stop-operations-drain", sandbox.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	var requestedDrainReason string
	if err := pool.QueryRow(t.Context(), `
		SELECT lifecycle_termination_reason FROM secondbox.sandboxes WHERE id=$1`,
		sandbox.ID,
	).Scan(&requestedDrainReason); err != nil {
		t.Fatal(err)
	}
	if requestedDrainReason != contracts.TerminationReasonRequestedDrain {
		t.Fatalf(
			"production drain reason = %q, want %q",
			requestedDrainReason, contracts.TerminationReasonRequestedDrain,
		)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopOperation, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "stop-operations-stop", sandbox.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpointOperation, err := controlPlane.CheckpointSandbox(
		t.Context(), principal, sandbox.ID, "stop-operations-checkpoint", sandbox.Revision,
		map[string]string{"label": "all-compatible"},
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := lifecycle.Reconciler{
		Store: databaseStore,
		Effects: &integrationLifecycleEffects{
			store: databaseStore, pool: pool, checkpointID: "chk_stop_operations",
		},
		WorkerID:      "stop-operations-worker",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("drain decision = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now.Add(time.Millisecond)); err != nil ||
		!found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint decision = %#v, %t, %v", decision, found, err)
	}
	checkpoint := contracts.WorkspaceCheckpoint{
		ID: "chk_stop_operations", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		SizeBytes:        4096, Compatibility: map[string]string{"architecture": "amd64"},
		RetainUntil: now.Add(time.Hour), CreatedAt: now,
	}
	publication := ports.CheckpointPublicationInput{
		Checkpoint: checkpoint, StorageKey: "checkpoints/chk_stop_operations",
		ExpectedWorkspaceGeneration: sandbox.Generation,
	}
	if _, err := databaseStore.StageCheckpoint(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.VerifyCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.PublishCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now.Add(2*time.Millisecond)); err != nil ||
		!found || decision.Action != lifecycle.ActionStopInstance {
		t.Fatalf("stop-instance decision = %#v, %t, %v", decision, found, err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='stopped',termination_reason='',
		    stopped_at=$2,updated_at=$2
		WHERE id=$1`,
		instanceID, now.Add(3*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now.Add(3*time.Millisecond)); err != nil ||
		!found || decision.Action != lifecycle.ActionFinishStop {
		t.Fatalf("finish-stop decision = %#v, %t, %v", decision, found, err)
	}
	var finishedInstanceReason string
	if err := pool.QueryRow(t.Context(), `
		SELECT termination_reason FROM secondbox.instances WHERE id=$1`,
		instanceID,
	).Scan(&finishedInstanceReason); err != nil {
		t.Fatal(err)
	}
	if finishedInstanceReason != contracts.TerminationReasonRequestedStop {
		t.Fatalf(
			"finish-stop old-generation reason = %q, want %q",
			finishedInstanceReason, contracts.TerminationReasonRequestedStop,
		)
	}
	for _, operationID := range []string{drainOperation.ID, stopOperation.ID, checkpointOperation.ID} {
		operation, err := controlPlane.GetOperation(t.Context(), principal, operationID)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State != contracts.OperationStateSucceeded {
			t.Fatalf("compatible Operation %s state = %q", operationID, operation.State)
		}
	}
}

func TestLifecycleReconcilerPersistsPolicyTerminationCauses(t *testing.T) {
	for _, test := range []struct {
		name     string
		reason   string
		idle     int64
		maximum  int64
		liveness string
	}{
		{
			name: "idle_timeout", reason: contracts.TerminationReasonIdleTimeout,
			idle: 1, liveness: contracts.GuestLivenessReady,
		},
		{
			name: "maximum_duration", reason: contracts.TerminationReasonMaximumDuration,
			maximum: 1, liveness: contracts.GuestLivenessReady,
		},
		{
			name: "guest_agent_lost", reason: contracts.TerminationReasonGuestAgentLost,
			liveness: contracts.GuestLivenessLost,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			slug := strings.ReplaceAll(test.name, "_", "-")
			controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
			admin := fixtureAdmin(t, controlPlane)
			_, account, credential := createProjectAccountAndCredential(
				t, controlPlane, admin, "termination-"+slug,
			)
			profile := createGrantedProfile(
				t, controlPlane, databaseStore, admin, account, "profile-"+slug,
			)
			principal := authenticateCredential(t, controlPlane, credential)
			sandbox, _, err := controlPlane.CreateSandbox(
				t.Context(), principal, "create-"+slug,
				contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
			)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			seed := seedRelayReadyAssignment(t, sandbox, now)
			pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if _, err := pool.Exec(t.Context(), `
				UPDATE secondbox.profile_revisions
				SET spec_json=jsonb_set(
				  jsonb_set(spec_json,'{lifecycle,idleSeconds}',to_jsonb($2::bigint)),
				  '{lifecycle,maximumDurationSeconds}',to_jsonb($3::bigint)
				)
				WHERE id=$1`,
				sandbox.ProfileRevisionID, test.idle, test.maximum,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `
				UPDATE secondbox.instances
				SET guest_liveness=$2,ready_at=$3,updated_at=$4
				WHERE id=$1`,
				seed.Fence.InstanceId, test.liveness, now.Add(-2*time.Second), now,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `
				UPDATE secondbox.sandboxes
				SET last_activity_at=$2,next_reconcile_at=$3,updated_at=$3
				WHERE id=$1`,
				sandbox.ID, now.Add(-2*time.Second), now,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `
				UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
				sandbox.ID, now.Add(time.Hour),
			); err != nil {
				t.Fatal(err)
			}
			reconciler := lifecycle.Reconciler{
				Store: databaseStore,
				Effects: &integrationLifecycleEffects{
					store: databaseStore, pool: pool,
				},
				WorkerID:      "termination-worker-" + slug,
				ClaimDuration: time.Minute, PollInterval: time.Millisecond,
			}
			decision, found, err := reconciler.RunOnce(t.Context(), now)
			if err != nil || !found ||
				decision.Action != lifecycle.ActionDrain ||
				decision.TerminationReason != test.reason {
				t.Fatalf(
					"policy termination decision = %+v, found %t, error %v",
					decision, found, err,
				)
			}
			var persisted string
			if err := pool.QueryRow(t.Context(), `
				SELECT lifecycle_termination_reason
				FROM secondbox.sandboxes WHERE id=$1`,
				sandbox.ID,
			).Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			if persisted != test.reason {
				t.Fatalf("persisted policy reason = %q, want %q", persisted, test.reason)
			}
		})
	}
}

func TestCheckpointReceiverPublishesAndStreamsCrossRunnerRestoreIdempotently(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "checkpoint-receiver")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-checkpoint-receiver")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "checkpoint-receiver-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	const (
		runnerID       = "runner-checkpoint-receiver"
		assignmentID   = "assignment-checkpoint-receiver"
		instanceID     = "instance-checkpoint-receiver"
		checkpointID   = "checkpoint-receiver"
		storageObject  = "checkpoints/checkpoint-receiver.ext4"
		maximumBytes   = int64(4096)
		retentionHours = 24
	)
	fencingToken := []byte("01234567890123456789012345678901")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,checkpoint_id,storage_object_id,fencing_token,retry_count,retry_limit,
			effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
			payload_json,evidence_json,created_at,updated_at
		) VALUES (
			'effect-checkpoint-receiver',$1,$2,'checkpoint','queued',$3,$4,$5,
			'command-checkpoint-receiver',$6,$7,$8,0,2,$9,'',$10,'','',
			jsonb_build_object(
				'workspaceId',$11::text,
				'retainUntil',$12::timestamptz,
				'maximumSizeBytes',$13::bigint
			),
			'{}',$14,$14
		)`,
		sandbox.ID, sandbox.Generation, assignmentID, instanceID, runnerID,
		checkpointID, storageObject, fencingToken, now.Add(time.Minute), now,
		sandbox.Workspace.ID, now.Add(retentionHours*time.Hour), maximumBytes, now,
	); err != nil {
		t.Fatal(err)
	}
	objects := &checkpointObjectStore{
		objects:              make(map[string][]byte),
		putFailuresRemaining: 1,
	}
	spoolDirectory := t.TempDir()
	receiver, err := lifecycle.NewCheckpointReceiver(t.Context(), lifecycle.CheckpointReceiverConfig{
		DatabaseURL: integrationDatabaseURL, SpoolDirectory: spoolDirectory,
		ObjectStore: objects, LifecycleStore: databaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := &runnerv1.AssignmentFence{
		AssignmentId: assignmentID, SandboxId: sandbox.ID, InstanceId: instanceID,
		SandboxGeneration: uint64(sandbox.Generation), FencingToken: fencingToken,
	}
	firstBytes := []byte("verified-")
	secondBytes := []byte("checkpoint")
	chunkEvent := func(offset uint64, data []byte) runnercontrol.Event {
		return runnercontrol.Event{
			Kind: runnercontrol.EventCheckpoint, RunnerID: runnerID,
			Message: &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_CheckpointChunk{
					CheckpointChunk: &runnerv1.CheckpointChunk{
						Fence: fence, CheckpointId: checkpointID,
						StorageObjectId: storageObject, Offset: offset, Data: data,
					},
				},
			},
		}
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), chunkEvent(0, firstBytes), now); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), chunkEvent(0, firstBytes), now); err != nil {
		t.Fatalf("identical chunk replay failed: %v", err)
	}
	if err := receiver.ReceiveCheckpoint(
		t.Context(), chunkEvent(0, []byte("different")), now,
	); err == nil {
		t.Fatal("divergent chunk replay succeeded")
	}
	if err := receiver.ReceiveCheckpoint(
		t.Context(), chunkEvent(uint64(len(firstBytes)+1), secondBytes), now,
	); err == nil {
		t.Fatal("chunk offset gap succeeded")
	}
	if err := receiver.ReceiveCheckpoint(
		t.Context(), chunkEvent(uint64(len(firstBytes)), secondBytes), now,
	); err != nil {
		t.Fatal(err)
	}
	content := append(bytes.Clone(firstBytes), secondBytes...)
	sum := sha256.Sum256(content)
	resultEvent := runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: runnerID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointResult{
				CheckpointResult: &runnerv1.CheckpointResult{
					Fence: fence, CheckpointId: checkpointID, StorageObjectId: storageObject,
					Terminal: runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED,
					Sha256:   hex.EncodeToString(sum[:]), SizeBytes: uint64(len(content)),
					Compatibility: map[string]string{
						"architecture": "amd64", "backend": "firecracker",
						"profileRevisionId": sandbox.ProfileRevisionID, "workspaceFormat": "ext4",
						"runtimeManifestDigest":   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"toolchainManifestDigest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
						"guestProtocolGeneration": "1", "mandatoryGuestFeatures": "",
					},
				},
			},
		},
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), resultEvent, now); err == nil {
		t.Fatal("checkpoint publication succeeded during object-store interruption")
	}
	var interruptedEffectState, interruptedCheckpointID string
	var interruptedCheckpointCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,
		       COALESCE(workspace.current_checkpoint_id,''),
		       (SELECT count(*) FROM secondbox.workspace_checkpoints WHERE id=$2)
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=effect.sandbox_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE effect.checkpoint_id=$1`,
		checkpointID, checkpointID,
	).Scan(
		&interruptedEffectState,
		&interruptedCheckpointID,
		&interruptedCheckpointCount,
	); err != nil {
		t.Fatal(err)
	}
	if interruptedEffectState != "queued" || interruptedCheckpointID != "" ||
		interruptedCheckpointCount != 0 || len(objects.objects) != 0 {
		t.Fatalf(
			"object-store interruption published authority: effect=%q current=%q checkpoints=%d objects=%d",
			interruptedEffectState,
			interruptedCheckpointID,
			interruptedCheckpointCount,
			len(objects.objects),
		)
	}
	receiver.Close()
	receiver, err = lifecycle.NewCheckpointReceiver(
		t.Context(),
		lifecycle.CheckpointReceiverConfig{
			DatabaseURL: integrationDatabaseURL, SpoolDirectory: spoolDirectory,
			ObjectStore: objects, LifecycleStore: databaseStore,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	if err := receiver.ReceiveCheckpoint(t.Context(), resultEvent, now); err != nil {
		t.Fatalf("checkpoint publication did not recover after object-store interruption: %v", err)
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), resultEvent, now); err != nil {
		t.Fatalf("terminal replay failed: %v", err)
	}
	if stored := objects.objects[storageObject]; !bytes.Equal(stored, content) {
		t.Fatalf("immutable object bytes = %q, want %q", stored, content)
	}
	var effectState, checkpointState, currentCheckpointID string
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,checkpoint.state,workspace.current_checkpoint_id
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.workspace_checkpoints AS checkpoint
		  ON checkpoint.id=effect.checkpoint_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=checkpoint.workspace_id
		WHERE effect.checkpoint_id=$1`,
		checkpointID,
	).Scan(&effectState, &checkpointState, &currentCheckpointID); err != nil {
		t.Fatal(err)
	}
	if effectState != "published" || checkpointState != contracts.ObjectStatePublished ||
		currentCheckpointID != checkpointID {
		t.Fatalf(
			"checkpoint publication state = effect %q, checkpoint %q, current %q",
			effectState, checkpointState, currentCheckpointID,
		)
	}
	restoreSender, err := lifecycle.NewCheckpointRestoreSender(
		t.Context(),
		lifecycle.CheckpointRestoreSenderConfig{
			DatabaseURL: integrationDatabaseURL, ObjectStore: objects,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoreSender.Close)
	restoreFence := &runnerv1.AssignmentFence{
		AssignmentId: "assignment-cross-runner", SandboxId: sandbox.ID,
		InstanceId: "instance-cross-runner", SandboxGeneration: uint64(sandbox.Generation),
		FencingToken: []byte("abcdefghijklmnopqrstuvwxyz012345"),
	}
	var restoreFrames []*runnerv1.ControlPlaneToRunner
	if err := restoreSender.StreamRestore(
		t.Context(),
		&runnerv1.AssignmentCommand{
			Fence: restoreFence, ProfileRevisionId: sandbox.ProfileRevisionID,
			Requirements: &runnerv1.ProfileRequirements{
				Architecture: "amd64", DiskBytes: uint64(len(content)),
			},
			Assets: []*runnerv1.SignedAssetReference{
				{
					ArtifactId:              "runtime",
					ManifestDigest:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					GuestProtocolGeneration: 1,
				},
				{
					ArtifactId:              "toolchain",
					ManifestDigest:          "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					GuestProtocolGeneration: 1,
				},
			},
			SourceCheckpointId: checkpointID,
			DeadlineUnixMs:     uint64(now.Add(time.Minute).UnixMilli()),
		},
		func(frame *runnerv1.ControlPlaneToRunner) error {
			restoreFrames = append(restoreFrames, frame)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(restoreFrames) < 3 || restoreFrames[0].GetRestoreBegin() == nil ||
		!restoreFrames[len(restoreFrames)-1].GetRestoreChunk().GetEndOfObject() {
		t.Fatalf("cross-runner restore frames = %#v", restoreFrames)
	}
	var restored []byte
	for _, frame := range restoreFrames[1:] {
		chunk := frame.GetRestoreChunk()
		if chunk == nil {
			t.Fatalf("cross-runner restore emitted non-chunk frame %#v", frame)
		}
		restored = append(restored, chunk.Data...)
	}
	if !bytes.Equal(restored, content) {
		t.Fatalf("cross-runner restored bytes = %q, want %q", restored, content)
	}
}

type checkpointObjectStore struct {
	objects              map[string][]byte
	putFailuresRemaining int
}

func (store *checkpointObjectStore) PutImmutable(
	_ context.Context,
	key string,
	reader io.Reader,
	sizeBytes int64,
	expectedSHA256 string,
) (objectstore.Evidence, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return objectstore.Evidence{}, err
	}
	sum := sha256.Sum256(content)
	actualSHA256 := hex.EncodeToString(sum[:])
	if int64(len(content)) != sizeBytes || actualSHA256 != expectedSHA256 {
		return objectstore.Evidence{}, errors.New("fake immutable object integrity mismatch")
	}
	if store.putFailuresRemaining > 0 {
		store.putFailuresRemaining--
		return objectstore.Evidence{}, errors.New("fake immutable object store interruption")
	}
	if existing, exists := store.objects[key]; exists && !bytes.Equal(existing, content) {
		return objectstore.Evidence{}, errors.New("fake immutable object replacement")
	}
	store.objects[key] = bytes.Clone(content)
	return objectstore.Evidence{SHA256: actualSHA256, SizeBytes: sizeBytes, ETag: "fake-etag"}, nil
}

func (store *checkpointObjectStore) HeadVerified(
	_ context.Context,
	key string,
	expected objectstore.Evidence,
) (objectstore.Evidence, error) {
	content, exists := store.objects[key]
	if !exists {
		return objectstore.Evidence{}, errors.New("fake immutable object not found")
	}
	sum := sha256.Sum256(content)
	actual := objectstore.Evidence{
		SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content)), ETag: "fake-etag",
	}
	if actual.SHA256 != expected.SHA256 || actual.SizeBytes != expected.SizeBytes {
		return objectstore.Evidence{}, errors.New("fake immutable object evidence mismatch")
	}
	return actual, nil
}

func (store *checkpointObjectStore) GetVerified(
	ctx context.Context,
	key string,
	expected objectstore.Evidence,
) (io.ReadCloser, objectstore.Evidence, error) {
	actual, err := store.HeadVerified(ctx, key, expected)
	if err != nil {
		return nil, objectstore.Evidence{}, err
	}
	return io.NopCloser(bytes.NewReader(store.objects[key])), actual, nil
}

func (store *checkpointObjectStore) Delete(_ context.Context, key string) error {
	delete(store.objects, key)
	return nil
}

type integrationLifecycleEffects struct {
	store                 *store.PostgresControlPlaneStore
	pool                  *pgxpool.Pool
	checkpointID          string
	createMaterialization bool
}

func (effects *integrationLifecycleEffects) ExecuteLifecycleEffect(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	decision lifecycle.Decision,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	switch decision.Action {
	case lifecycle.ActionMaterialize:
		if !effects.createMaterialization {
			break
		}
		_, err := effects.pool.Exec(ctx, `
			INSERT INTO secondbox.workspace_materializations (
				id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
				source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at,released_at
			)
			SELECT 'mat-fake-' || sandbox.id,sandbox.workspace_id,sandbox.id,
			       'assignment-fake-' || sandbox.id,'runner-fake',sandbox.generation,
			       '','preparing','{}',1,$2,$2,NULL
			FROM secondbox.sandboxes AS sandbox WHERE sandbox.id=$1
			ON CONFLICT (id) DO NOTHING`,
			claim.SandboxID, now.UTC(),
		)
		if err != nil {
			return err
		}
	case lifecycle.ActionCheckpoint:
		if effects.checkpointID == "" {
			return errors.New("integration lifecycle checkpoint ID is required")
		}
		_, err := effects.pool.Exec(ctx, `
			INSERT INTO secondbox.lifecycle_effects (
				id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
				command_id,checkpoint_id,storage_object_id,fencing_token,retry_count,retry_limit,
				effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
				payload_json,evidence_json,created_at,updated_at
			)
			SELECT 'effect-fake-' || sandbox.id,sandbox.id,sandbox.generation,'checkpoint','queued',
			       'assignment-fake',sandbox.current_instance_id,'runner-fake','command-fake',
			       $2,'checkpoints/' || $2,''::bytea,0,1,$3,'',$1::timestamptz,'','','{}','{}',$1,$1
			FROM secondbox.sandboxes AS sandbox WHERE sandbox.id=$4
			ON CONFLICT (id) DO NOTHING`,
			now.UTC(), effects.checkpointID, now.UTC().Add(time.Minute), claim.SandboxID,
		)
		if err != nil {
			return err
		}
	}
	return effects.store.ApplyLifecycleAction(
		ctx, claim, string(decision.Action), decision.TerminationReason,
		now, nextReconcileAt,
	)
}
