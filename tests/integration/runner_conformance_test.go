package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol/conformance"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresRunnerControlBoundaryConformance(t *testing.T) {
	conformance.RunRunnerConformanceSuite(t, newPostgresConformanceBoundary)
}

type postgresConformanceBoundary struct {
	t              *testing.T
	now            time.Time
	pool           *pgxpool.Pool
	identity       runnercontrol.RunnerIdentity
	stateStore     *runnercontrol.PostgresStateStore
	schedulerStore *scheduler.PostgresStore
	reconcileStore *reconcile.PostgresStore
	sessions       map[string]*runnercontrol.Session
}

func newPostgresConformanceBoundary(
	t *testing.T,
	now time.Time,
) conformance.Boundary {
	t.Helper()
	task4InsertRunnerPool(t, "pool-conformance", now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	issued, err := authority.Issue("runner-conformance", task4CertificateRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := runnercontrol.NewPostgresStateStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	schedulerStore, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL,
			Now:         func() time.Time { return now },
		},
	)
	if err != nil {
		stateStore.Close()
		t.Fatal(err)
	}
	reconcileStore, err := reconcile.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		schedulerStore.Close()
		stateStore.Close()
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		reconcileStore.Close()
		schedulerStore.Close()
		stateStore.Close()
		t.Fatal(err)
	}
	boundary := &postgresConformanceBoundary{
		t: t, now: now, pool: pool, identity: issued.Identity,
		stateStore: stateStore, schedulerStore: schedulerStore,
		reconcileStore: reconcileStore,
		sessions:       make(map[string]*runnercontrol.Session),
	}
	t.Cleanup(boundary.close)
	return boundary
}

func (boundary *postgresConformanceBoundary) Connect(
	ctx context.Context,
	runnerID string,
	connectionID string,
	hello *runnerv1.RunnerToControlPlane,
) (*runnerv1.ControlPlaneToRunner, error) {
	session := runnercontrol.NewSession(runnercontrol.SessionConfig{
		AuthenticatedRunnerID: runnerID,
		SupportedVersions:     runnercontrol.VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		},
		HeartbeatInterval: 10 * time.Second,
		ConnectionID:      connectionID,
	})
	event, err := session.Accept(hello)
	if err != nil {
		return nil, err
	}
	if event.GetRejection() != nil {
		return event.Response, nil
	}
	if boundary.identity.RunnerID != runnerID {
		return nil, errors.New("PostgreSQL conformance Runner identity mismatch")
	}
	if err := boundary.stateStore.OpenConnection(
		ctx, boundary.identity, connectionID, event.GetWelcome().SelectedVersion, boundary.now,
	); err != nil {
		return nil, err
	}
	clear(boundary.sessions)
	boundary.sessions[connectionID] = session
	return event.Response, nil
}

func (boundary *postgresConformanceBoundary) Receive(
	ctx context.Context,
	connectionID string,
	message *runnerv1.RunnerToControlPlane,
	now time.Time,
) (conformance.Outcome, error) {
	session, exists := boundary.sessions[connectionID]
	if !exists {
		return "", conformance.ErrInactiveConnection
	}
	event, err := session.Accept(message)
	if errors.Is(err, runnercontrol.ErrSequenceReordered) {
		return "", conformance.ErrReorderedMessage
	}
	if err != nil {
		return "", err
	}
	switch event.Kind {
	case runnercontrol.EventDuplicate:
		return conformance.OutcomeDuplicate, nil
	case runnercontrol.EventRegistration:
		duplicate, err := boundary.stateStore.RecordRegistration(ctx, event.Registration, now)
		if duplicate {
			return conformance.OutcomeDuplicate, err
		}
		return conformance.OutcomeRegistration, err
	case runnercontrol.EventHeartbeat:
		duplicate, err := boundary.stateStore.RecordHeartbeat(ctx, event.Heartbeat, now)
		if duplicate {
			return conformance.OutcomeDuplicate, err
		}
		return conformance.OutcomeHeartbeat, err
	case runnercontrol.EventAssignment:
		duplicate, err := boundary.stateStore.RecordEvent(ctx, event, now)
		if duplicate {
			return conformance.OutcomeDuplicate, err
		}
		return conformance.OutcomeAssignment, err
	default:
		return "", fmt.Errorf("PostgreSQL conformance event %q is unsupported", event.Kind)
	}
}

func (boundary *postgresConformanceBoundary) SeedAssignment(
	ctx context.Context,
	fence *runnerv1.AssignmentFence,
	now time.Time,
) error {
	workspaceID := task4InsertSchedulableSandbox(
		boundary.t, fence.SandboxId, "profile-revision-conformance", "runner-conformance", now,
	)
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, created, err := boundary.schedulerStore.Schedule(ctx, scheduler.ScheduleRequest{
		AssignmentID: fence.AssignmentId, AssignmentCommandID: "assignment-command-" + fence.AssignmentId,
		InstanceID: fence.InstanceId, SandboxID: fence.SandboxId,
		ProfileRevisionID: "profile-revision-conformance",
		WorkspaceID:       workspaceID, StartMutationID: "workspace-start-" + fence.AssignmentId,
		Requirements: scheduler.Requirements{
			PoolName: "pool-conformance", BackendKind: "firecracker",
			Architecture: "amd64", RequiredCapabilities: []string{"local-workspace"},
			GuestProtocolGeneration: 1,
			Capacity: scheduler.Capacity{
				CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
				Instances: 1, Operations: 1,
			},
			PreferredArtifactDigests: []string{runtimeDigest},
		},
		AssignmentCommand: &runnerv1.AssignmentCommand{
			Fence: fence, ProfileRevisionId: "profile-revision-conformance", WorkspaceId: workspaceID,
			Requirements: &runnerv1.ProfileRequirements{
				VcpuCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
				Architecture: "amd64", RequiredCapabilities: []string{"local-workspace"},
				MaximumOperationMs: 60_000, MaximumOutputBytes: 1 << 20,
			},
			Assets: []*runnerv1.SignedAssetReference{
				{
					ArtifactId: "runtime", ManifestDigest: runtimeDigest,
					SignatureKeyId: "release-key-1", Architecture: "amd64",
					GuestProtocolGeneration: 1,
				},
			},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()),
			Correlation: &runnerv1.Correlation{
				RequestId: "request-conformance", OperationId: "operation-conformance",
				SandboxId: fence.SandboxId, SandboxGeneration: fence.SandboxGeneration,
			},
		},
		FencingToken:      fence.FencingToken,
		ResolvedArtifacts: map[string]string{"runtime": runtimeDigest},
		ClaimExpiresAt:    now.Add(time.Minute), OperationDeadline: now.Add(time.Minute),
		RetryLimit: 2, SerializationRetryLimit: 2,
		HeartbeatTimeout: 30 * time.Second, Now: now,
	})
	if err != nil {
		return err
	}
	if !created {
		return errors.New("PostgreSQL conformance Assignment was not created")
	}
	return nil
}

func (boundary *postgresConformanceBoundary) ExpireRunner(
	ctx context.Context,
	runnerID string,
	at time.Time,
) error {
	count, err := boundary.reconcileStore.MarkExpiredRunners(ctx, at, at)
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("PostgreSQL conformance did not expire a Runner")
	}
	return nil
}

func (boundary *postgresConformanceBoundary) Snapshot(
	ctx context.Context,
	runnerID string,
) (conformance.Snapshot, error) {
	var snapshot conformance.Snapshot
	var capacityJSON []byte
	if err := boundary.pool.QueryRow(ctx, `
		SELECT state,drain_phase,capacity_json,active_connection_id
		FROM secondbox.runners WHERE id=$1`, runnerID,
	).Scan(
		&snapshot.RunnerState, &snapshot.DrainPhase,
		&capacityJSON, &snapshot.ActiveConnectionID,
	); err != nil {
		return conformance.Snapshot{}, err
	}
	var capacity scheduler.Capacity
	if err := json.Unmarshal(capacityJSON, &capacity); err != nil {
		return conformance.Snapshot{}, err
	}
	snapshot.CPUMillis = capacity.CPUMillis
	snapshot.MemoryBytes = capacity.MemoryBytes
	snapshot.DiskBytes = capacity.DiskBytes
	snapshot.Instances = capacity.Instances
	snapshot.Operations = capacity.Operations
	var releaseProofJSON []byte
	err := boundary.pool.QueryRow(ctx, `
		SELECT state,release_proof_json FROM secondbox.assignments
		WHERE runner_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, runnerID,
	).Scan(&snapshot.AssignmentState, &releaseProofJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return conformance.Snapshot{}, err
	}
	var proof map[string]string
	if err := json.Unmarshal(releaseProofJSON, &proof); err != nil {
		return conformance.Snapshot{}, err
	}
	snapshot.MayReassign = snapshot.AssignmentState == "fenced" &&
		proof["terminationEvidenceDigest"] != ""
	return snapshot, nil
}

func (boundary *postgresConformanceBoundary) close() {
	for _, statement := range []string{
		`DELETE FROM secondbox.runner_messages WHERE connection_id IN (
			SELECT id FROM secondbox.runner_connections WHERE runner_id='runner-conformance'
		)`,
		`DELETE FROM secondbox.runner_commands WHERE runner_id='runner-conformance'`,
		`DELETE FROM secondbox.runner_connections WHERE runner_id='runner-conformance'`,
		`DELETE FROM secondbox.assignments WHERE runner_id='runner-conformance'`,
		`DELETE FROM secondbox.instances WHERE sandbox_id='sandbox-conformance'`,
		`DELETE FROM secondbox.sandboxes WHERE id='sandbox-conformance'`,
		`DELETE FROM secondbox.workspaces WHERE sandbox_id='sandbox-conformance'`,
		`DELETE FROM secondbox.runners WHERE id='runner-conformance'`,
		`DELETE FROM secondbox.runner_pools WHERE name='pool-conformance'`,
	} {
		if _, err := boundary.pool.Exec(context.Background(), statement); err != nil {
			boundary.t.Errorf("PostgreSQL conformance cleanup failed: %v", err)
		}
	}
	boundary.pool.Close()
	boundary.reconcileStore.Close()
	boundary.schedulerStore.Close()
	boundary.stateStore.Close()
}
