package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var task4Sequence atomic.Int64

func TestRunnerPreSharedCredentialAndMTLSIdentity(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	poolName := task4ID("credential-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	runnerID := task4ID("runner")
	csr := task4CertificateRequest(t)
	issued, err := authority.Issue(runnerID, csr)
	if err != nil {
		t.Fatal(err)
	}
	certificate := task4ParseCertificate(t, issued.CertificatePEM)
	identity, err := authority.VerifyClientCertificate(
		t.Context(), certificate, task4RunnerCredential,
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.RunnerID != runnerID {
		t.Fatalf("certificate Runner identity = %q, want %q", identity.RunnerID, runnerID)
	}
	for _, credential := range []string{"", "mismatched-runner-credential-material-000000"} {
		if _, err := authority.verifier.VerifyClientCertificate(
			t.Context(), certificate, credential,
		); !errors.Is(err, runnercontrol.ErrRunnerCredentialInvalid) {
			t.Fatalf("credential %q error = %v, want ErrRunnerCredentialInvalid", credential, err)
		}
	}

	serverCertificate := task4ServerCertificate(t, caCertificate, caPrivateKey, now)
	tlsConfig, err := authority.ServerTLSConfig(serverCertificate)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS13 ||
		tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("runner mTLS server config lacks TLS 1.3 or client verification: %#v", tlsConfig)
	}
}

func TestRunnerRegistrationRejectsPoolsThatAreNotReady(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 30, 0, 0, time.UTC)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)

	for _, state := range []string{"draining", "offline"} {
		poolName := task4ID(state + "-pool")
		runnerID := task4ID(state + "-runner")
		connectionID := task4ID(state + "-connection")
		task4InsertRunnerPool(t, poolName, now)
		if _, err := pool.Exec(
			t.Context(),
			`UPDATE secondbox.runner_pools SET state=$2 WHERE name=$1`,
			poolName,
			state,
		); err != nil {
			t.Fatal(err)
		}
		issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
			t.Fatal(err)
		}
		_, err = stateStore.RecordRegistration(
			t.Context(), task4Registration(runnerID, connectionID, poolName), now,
		)
		if err == nil || err.Error() != "SecondBox RunnerPool is not accepting runners" {
			t.Fatalf("%s pool registration error = %v", state, err)
		}
	}
}

func TestRunnerPoolSealsToFirstHealthyBackendAndRejectsMismatch(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	poolName := task4ID("sealed-backend-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)

	register := func(runnerID, connectionID string, registration *runnerv1.RunnerRegistration) error {
		issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
		if err != nil {
			return err
		}
		if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
			return err
		}
		_, err = stateStore.RecordRegistration(t.Context(), registration, now)
		return err
	}

	firstRunner, firstConnection := task4ID("firecracker-runner"), task4ID("firecracker-connection")
	if err := register(firstRunner, firstConnection, task4Registration(firstRunner, firstConnection, poolName)); err != nil {
		t.Fatal(err)
	}

	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)
	var sealedKind string
	if err := databasePool.QueryRow(t.Context(), `SELECT backend_kind FROM secondbox.runner_pools WHERE name=$1`, poolName).Scan(&sealedKind); err != nil {
		t.Fatal(err)
	}
	if sealedKind != "firecracker" {
		t.Fatalf("sealed backend kind = %q", sealedKind)
	}

	secondRunner, secondConnection := task4ID("microsandbox-runner"), task4ID("microsandbox-connection")
	mismatch := task4Registration(secondRunner, secondConnection, poolName)
	mismatch.BackendKind = runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_MICROSANDBOX
	mismatch.Materializations[0].BackendKind = mismatch.BackendKind
	mismatch.Materializations[0].SourceOciManifestDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	mismatch.Materializations[0].FlatRootDigest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	mismatch.Materializations[0].HelperBuildId = "secondbox-microsandbox-helper-v1"
	if err := register(secondRunner, secondConnection, mismatch); err == nil || !strings.Contains(err.Error(), "sealed to a different compute backend") {
		t.Fatalf("backend mismatch registration error = %v", err)
	}
}

// TestRunnerRegistrationAdvertisesSnapshotResumeOnlyWhenTheRunnerReportsIt pins
// the one place the scheduler's capability vocabulary is minted. Resume capacity
// is optional: a Runner that does not report it registers normally and stays
// fully schedulable for cold_boot, and only a Runner that reports it becomes
// visible to a snapshot_resume Profile.
func TestRunnerRegistrationAdvertisesSnapshotResumeOnlyWhenTheRunnerReportsIt(t *testing.T) {
	now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
	poolName := task4ID("resume-capability-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)

	for _, testCase := range []struct {
		name                string
		snapshotResumeReady bool
	}{
		{"cold boot only", false},
		{"snapshot resume ready", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runnerID := task4ID("runner")
			connectionID := task4ID("connection")
			issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := stateStore.OpenConnection(
				t.Context(), issued.Identity, connectionID, 1, now,
			); err != nil {
				t.Fatal(err)
			}
			registration := task4Registration(runnerID, connectionID, poolName)
			registration.Capabilities.SnapshotResumeReady = testCase.snapshotResumeReady
			if duplicate, err := stateStore.RecordRegistration(
				t.Context(), registration, now,
			); err != nil || duplicate {
				t.Fatalf("RecordRegistration duplicate, error = %t, %v", duplicate, err)
			}
			var capabilitiesJSON []byte
			if err := databasePool.QueryRow(t.Context(), `
				SELECT capabilities_json FROM secondbox.runners WHERE id=$1`, runnerID,
			).Scan(&capabilitiesJSON); err != nil {
				t.Fatal(err)
			}
			var capabilities []string
			if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
				t.Fatal(err)
			}
			advertised := slices.Contains(capabilities, contracts.RunnerCapabilitySnapshotResume)
			if advertised != testCase.snapshotResumeReady {
				t.Fatalf(
					"snapshot-resume advertised = %t, want %t (capabilities %v)",
					advertised, testCase.snapshotResumeReady, capabilities,
				)
			}
			// Cold-boot placement never depends on resume capacity, so the
			// ordinary prerequisites must be present either way.
			for _, prerequisite := range []string{
				"compute", "network-policy", "storage", "cleanup", "local-workspace",
			} {
				if !slices.Contains(capabilities, prerequisite) {
					t.Errorf("registration lost prerequisite capability %q", prerequisite)
				}
			}
		})
	}
}

func TestRunnerProtocolPersistenceAndMultiControlPlaneSchedulingAreReplicaSafe(t *testing.T) {
	now := time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	poolName := task4ID("scheduler-pool")
	runnerID := task4ID("runner")
	connectionID := task4ID("connection")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
		t.Fatal(err)
	}
	registration := task4Registration(runnerID, connectionID, poolName)
	if duplicate, err := stateStore.RecordRegistration(t.Context(), registration, now); err != nil || duplicate {
		t.Fatalf("RecordRegistration duplicate, error = %t, %v", duplicate, err)
	}
	heartbeat := task4Heartbeat(runnerID, connectionID, "heartbeat-2", 2, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
	if duplicate, err := stateStore.RecordHeartbeat(t.Context(), heartbeat, now); err != nil || duplicate {
		t.Fatalf("RecordHeartbeat duplicate, error = %t, %v", duplicate, err)
	}
	if duplicate, err := stateStore.RecordHeartbeat(t.Context(), heartbeat, now); err != nil || !duplicate {
		t.Fatalf("duplicate Heartbeat duplicate, error = %t, %v", duplicate, err)
	}
	reordered := task4Heartbeat(runnerID, connectionID, "heartbeat-reordered", 1, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
	if _, err := stateStore.RecordHeartbeat(t.Context(), reordered, now); !errors.Is(err, runnercontrol.ErrSequenceReordered) {
		t.Fatalf("reordered Heartbeat error = %v, want ErrSequenceReordered", err)
	}
	task4CompleteWorkspaceReconciliation(
		t,
		stateStore,
		runnerID,
		connectionID,
		3,
		nil,
		now,
	)

	sandboxID := task4ID("sandbox")
	profileRevisionID := task4ID("profile-revision")
	workspaceID := task4InsertSchedulableSandbox(t, sandboxID, profileRevisionID, runnerID, now)
	firstScheduler, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL,
			Now:         func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(firstScheduler.Close)
	secondScheduler, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL,
			Now:         func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secondScheduler.Close)
	stores := []*scheduler.PostgresStore{firstScheduler, secondScheduler}
	type result struct {
		assignment scheduler.DurableAssignment
		created    bool
		err        error
	}
	start := make(chan struct{})
	results := make(chan result, len(stores))
	var waitGroup sync.WaitGroup
	for index, schedulerStore := range stores {
		waitGroup.Add(1)
		go func(index int, schedulerStore *scheduler.PostgresStore) {
			defer waitGroup.Done()
			<-start
			assignmentID := task4IDForIndex("assignment", index)
			instanceID := task4IDForIndex("instance", index)
			fencingToken := []byte("01234567890123456789012345678901")
			runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			assignment, created, err := schedulerStore.Schedule(t.Context(), scheduler.ScheduleRequest{
				AssignmentID: assignmentID, AssignmentCommandID: task4IDForIndex("assignment-command", index),
				InstanceID: instanceID, SandboxID: sandboxID, ProfileRevisionID: profileRevisionID,
				WorkspaceID: workspaceID, StartMutationID: task4IDForIndex("workspace-start", index),
				Requirements: scheduler.Requirements{
					PoolName: poolName, Architecture: "amd64",
					RequiredCapabilities:    []string{"local-workspace", "network-policy"},
					GuestProtocolGeneration: 1,
					Capacity: scheduler.Capacity{
						VCPUCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
						Instances: 1, Operations: 1,
					},
					PreferredArtifactDigests: []string{runtimeDigest, toolchainDigest},
				},
				AssignmentCommand: &runnerv1.AssignmentCommand{
					WorkspaceId: workspaceID,
					Fence: &runnerv1.AssignmentFence{
						AssignmentId: assignmentID, SandboxId: sandboxID, InstanceId: instanceID,
						SandboxGeneration: 1, FencingToken: fencingToken,
					},
					ProfileRevisionId: profileRevisionID,
					Requirements: &runnerv1.ProfileRequirements{
						VcpuCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
						Architecture: "amd64", RequiredCapabilities: []string{"local-workspace", "network-policy"},
						MaximumOperationMs: 60_000, MaximumOutputBytes: 1 << 20,
					},
					Assets: []*runnerv1.AssetReference{
						{
							ArtifactId: "runtime", ManifestDigest: runtimeDigest,
							Architecture:            "amd64",
							GuestProtocolGeneration: 1,
						},
					},
					DeadlineUnixMs: uint64(now.Add(2 * time.Minute).UnixMilli()),
					Correlation: &runnerv1.Correlation{
						RequestId:   task4IDForIndex("request", index),
						OperationId: task4IDForIndex("operation", index),
						SandboxId:   sandboxID, SandboxGeneration: 1,
					},
				},
				FencingToken:      fencingToken,
				ResolvedArtifacts: map[string]string{"runtime": runtimeDigest},
				ClaimExpiresAt:    now.Add(time.Minute), OperationDeadline: now.Add(2 * time.Minute),
				RetryLimit: 2, SerializationRetryLimit: 3,
				HeartbeatTimeout: 30 * time.Second, Now: now,
				EffectStartedAt: now, PlanReadyAt: now,
			})
			results <- result{assignment: assignment, created: created, err: err}
		}(index, schedulerStore)
	}
	close(start)
	waitGroup.Wait()
	close(results)
	createdCount := 0
	var durableAssignment scheduler.DurableAssignment
	for result := range results {
		if result.err != nil {
			t.Fatalf("replica Schedule failed: %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if durableAssignment.ID == "" {
			durableAssignment = result.assignment
		}
		if result.assignment.ID != durableAssignment.ID ||
			result.assignment.RunnerID != runnerID ||
			result.assignment.ProfileRevisionID != profileRevisionID ||
			result.assignment.BackendKind != "firecracker" ||
			result.assignment.Generation != 1 ||
			!result.assignment.NextReconcileAt.Equal(now.Add(2*time.Minute)) ||
			len(result.assignment.FencingToken) != 32 {
			t.Fatalf("replica returned divergent durable Assignment: %#v", result.assignment)
		}
	}
	if createdCount != 1 {
		t.Fatalf("replica race created %d Assignments, want exactly 1", createdCount)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var commandState, targetConnectionID string
	var deliveryCount, lastControlSequence int64
	if err := pool.QueryRow(t.Context(), `
		SELECT command.state,command.target_connection_id,command.delivery_count,
		       connection.last_control_sequence
		FROM secondbox.runner_commands AS command
		JOIN secondbox.runner_connections AS connection ON connection.id=$2
		WHERE command.assignment_id=$1`,
		durableAssignment.ID,
		connectionID,
	).Scan(
		&commandState,
		&targetConnectionID,
		&deliveryCount,
		&lastControlSequence,
	); err != nil {
		t.Fatal(err)
	}
	// Placement queues the command and nothing more. Assigning the stream
	// sequence here required locking the runner's single runner_connections row
	// inside every placement transaction, which made concurrent placements for
	// one runner serialise on one row and lose the race under serializable
	// isolation. The connection owner assigns the sequence when it claims.
	if commandState != "pending" ||
		targetConnectionID != "" ||
		deliveryCount != 0 ||
		lastControlSequence != 1 {
		t.Fatalf(
			"queued Assignment dispatch = state %q target %q count %d sequence %d",
			commandState,
			targetConnectionID,
			deliveryCount,
			lastControlSequence,
		)
	}
	var placementStages []string
	var firstPlacementStage, lastPlacementStage time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT array_agg(stage ORDER BY stage),min(observed_at),max(observed_at)
		FROM secondbox.operation_stage_timings
		WHERE sandbox_id=$1 AND stage LIKE 'placement_%'`,
		sandboxID,
	).Scan(&placementStages, &firstPlacementStage, &lastPlacementStage); err != nil {
		t.Fatal(err)
	}
	wantPlacementStages := []string{
		"placement_assignment_checked",
		"placement_attempt_started",
		"placement_candidate_selected",
		"placement_candidates_locked",
		"placement_effect_started",
		"placement_plan_ready",
		"placement_ready",
		"placement_reconcile_started",
		"placement_sandbox_locked",
		"placement_schedule_started",
	}
	if !slices.Equal(placementStages, wantPlacementStages) ||
		!firstPlacementStage.Equal(now) || !lastPlacementStage.Equal(now) {
		t.Fatalf(
			"placement timing stages=%v first=%s last=%s",
			placementStages, firstPlacementStage, lastPlacementStage,
		)
	}
	delivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionID, now.Add(time.Second),
	)
	if err != nil || !found {
		t.Fatalf("Assignment command delivery found, error = %t, %v", found, err)
	}
	if delivery.Message.GetAssignment() == nil ||
		delivery.Message.GetAssignment().Fence.AssignmentId != durableAssignment.ID ||
		delivery.Message.GetAssignment().Sequence != 2 ||
		delivery.Message.GetAssignment().MessageId != delivery.ID {
		t.Fatalf("claimed Assignment command lacks durable authority: %#v", delivery.Message.GetAssignment())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), delivery, connectionID, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	staleReady := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: "ready-stale-4", Sequence: 4,
				Fence: &runnerv1.AssignmentFence{
					AssignmentId: durableAssignment.ID, SandboxId: sandboxID,
					InstanceId:        durableAssignment.InstanceID,
					SandboxGeneration: uint64(durableAssignment.Generation),
					FencingToken:      []byte("stale-stale-stale-stale-stale-stale"),
				},
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "fc-stale",
				Correlation: proto.Clone(delivery.Message.GetAssignment().Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID, ConnectionID: connectionID,
		Message: staleReady,
	}, now); !errors.Is(err, runnercontrol.ErrStaleAssignmentEvidence) {
		t.Fatalf("stale ready result error = %v, want ErrStaleAssignmentEvidence", err)
	}
	timingObservedAt := now.Add(750*time.Millisecond + 123*time.Microsecond)
	progress := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
			AssignmentProgress: &runnerv1.AssignmentProgress{
				MessageId: "progress-4", Sequence: 4,
				Fence:            proto.Clone(delivery.Message.GetAssignment().Fence).(*runnerv1.AssignmentFence),
				Stage:            runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
				ObservedAtUnixMs: uint64(timingObservedAt.UnixMilli()),
				ObservedAtUnixNs: uint64(timingObservedAt.UnixNano()),
				Correlation:      proto.Clone(delivery.Message.GetAssignment().Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if duplicate, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID, ConnectionID: connectionID,
		Message: progress,
	}, now.Add(time.Second)); err != nil || duplicate {
		t.Fatalf("AssignmentProgress duplicate, error = %t, %v", duplicate, err)
	}
	ready := proto.Clone(staleReady).(*runnerv1.RunnerToControlPlane)
	ready.GetAssignmentResult().MessageId = "ready-5"
	ready.GetAssignmentResult().Sequence = 5
	ready.GetAssignmentResult().Fence.FencingToken = durableAssignment.FencingToken
	ready.GetAssignmentResult().BackendReference = "fc-instance-1"
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID, ConnectionID: connectionID,
		Message: ready,
	}, now); err != nil {
		t.Fatal(err)
	}

	var count int
	var backendReference, capabilityJSON, sandboxState, guestLiveness string
	var sandboxStartSampleCount, sandboxStartP95Milliseconds int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*),max(assignment.backend_reference),max(assignment.capability_snapshot_json::text),
		       max(sandbox.state),max(instance.guest_liveness),
		       max(runner.sandbox_start_sample_count),max(runner.sandbox_start_p95_milliseconds)
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.runners AS runner ON runner.id=assignment.runner_id
		WHERE assignment.sandbox_id=$1`, sandboxID,
	).Scan(
		&count, &backendReference, &capabilityJSON, &sandboxState,
		&guestLiveness, &sandboxStartSampleCount, &sandboxStartP95Milliseconds,
	); err != nil {
		t.Fatal(err)
	}
	if count != 1 || backendReference != "fc-instance-1" ||
		!json.Valid([]byte(capabilityJSON)) ||
		sandboxState != "ready" || guestLiveness != "ready" ||
		sandboxStartSampleCount != 4 || sandboxStartP95Milliseconds != 75 {
		t.Fatalf(
			"durable Assignment evidence = count %d, backend %q, capability %q, Sandbox %q, guest %q, starts %d p95 %d",
			count, backendReference, capabilityJSON, sandboxState, guestLiveness,
			sandboxStartSampleCount, sandboxStartP95Milliseconds,
		)
	}
	var timingOperationID, timingSandboxID, timingStage string
	var observedAt, receivedAt time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT operation_id,sandbox_id,stage,observed_at,received_at
		FROM secondbox.assignment_stage_timings
		WHERE assignment_id=$1`,
		durableAssignment.ID,
	).Scan(
		&timingOperationID, &timingSandboxID, &timingStage, &observedAt, &receivedAt,
	); err != nil {
		t.Fatal(err)
	}
	if timingOperationID != delivery.Message.GetAssignment().Correlation.OperationId ||
		timingSandboxID != sandboxID ||
		timingStage != "runner_admission" ||
		!observedAt.Equal(timingObservedAt) ||
		!receivedAt.Equal(now.Add(time.Second)) {
		t.Fatalf(
			"AssignmentProgress timing = operation %q Sandbox %q stage %q observed %s received %s",
			timingOperationID, timingSandboxID, timingStage, observedAt, receivedAt,
		)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET state='offline' WHERE id=$1`,
		runnerID,
	); err != nil {
		t.Fatal(err)
	}

	reconcileStore, err := reconcile.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reconcileStore.Close)
	lostCount, err := reconcileStore.MarkExpiredRunners(
		t.Context(), now.Add(time.Second), now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if lostCount != 1 {
		t.Fatalf("expired Runner count = %d, want 1", lostCount)
	}
	claim, found, err := reconcileStore.ClaimNext(
		t.Context(), "control-plane-replica-2", now.Add(time.Minute), now.Add(2*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("reconciliation claim found, error = %t, %v", found, err)
	}
	lossDecision := reconcile.DecideRunnerLoss(claim.State, now.Add(2*time.Second))
	if lossDecision.Action != reconcile.ActionFence {
		t.Fatalf("unproved Runner loss decision = %#v", lossDecision)
	}
	fenceCommand := &runnerv1.FenceCommand{
		MessageId: task4ID("fence-command"),
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: durableAssignment.ID, SandboxId: sandboxID,
			InstanceId:        durableAssignment.InstanceID,
			SandboxGeneration: uint64(durableAssignment.Generation),
			FencingToken:      durableAssignment.FencingToken,
		},
		Reason:         runnerv1.FenceReason_FENCE_REASON_GENERATION_ADVANCED,
		DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()),
		Correlation:    proto.Clone(claim.Correlation).(*runnerv1.Correlation),
	}
	if err := reconcileStore.ApplyDecision(
		t.Context(), claim, lossDecision, fenceCommand,
		now.Add(3*time.Second), now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	fenceDelivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionID, now.Add(2*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("Fence command delivery found, error = %t, %v", found, err)
	}
	if fenceDelivery.Message.GetFence() == nil ||
		fenceDelivery.Message.GetFence().Fence.AssignmentId != durableAssignment.ID ||
		fenceDelivery.Message.GetFence().Sequence != 3 {
		t.Fatalf("claimed Fence command lacks durable authority: %#v", fenceDelivery.Message.GetFence())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), fenceDelivery, connectionID, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	fenceResult := &runnerv1.FenceResult{
		MessageId: "fence-result-6", Sequence: 6,
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: durableAssignment.ID, SandboxId: sandboxID,
			InstanceId:        durableAssignment.InstanceID,
			SandboxGeneration: uint64(durableAssignment.Generation),
			FencingToken:      durableAssignment.FencingToken,
		},
		Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
		TerminationEvidenceDigest: "sha256:termination-proof",
		Correlation:               proto.Clone(fenceDelivery.Message.GetFence().Correlation).(*runnerv1.Correlation),
	}
	if duplicate, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventFence, RunnerID: runnerID, ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_FenceResult{FenceResult: fenceResult},
		},
	}, now.Add(3*time.Second)); err != nil || duplicate {
		t.Fatalf("FenceResult duplicate, error = %t, %v", duplicate, err)
	}
	nextGeneration, err := reconcileStore.AdvanceFencedGeneration(
		t.Context(), durableAssignment.ID, 0, now.Add(4*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if nextGeneration != 2 {
		t.Fatalf("queued local generation = %d, want 2", nextGeneration)
	}
	advanceDelivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionID, now.Add(4*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("local generation command delivery found, error = %t, %v", found, err)
	}
	advanceCommand := advanceDelivery.Message.GetLocalWorkspace()
	if advanceCommand == nil ||
		advanceCommand.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION ||
		advanceCommand.WorkspaceId != workspaceID ||
		advanceCommand.ExpectedGeneration != 1 ||
		advanceCommand.NextGeneration != 2 {
		t.Fatalf("claimed local generation command lacks durable authority: %#v", advanceCommand)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), advanceDelivery, connectionID, now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventLocalWorkspace, RunnerID: runnerID, ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{
				LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{
					MessageId: "local-generation-result-7", Sequence: 7, CommandVersion: 1,
					Kind:        runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
					Terminal:    runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
					OperationId: advanceCommand.OperationId, EffectId: advanceCommand.EffectId,
					SandboxId: sandboxID, WorkspaceId: workspaceID,
					PreviousGeneration: 1, Generation: 2, LogicalCapacityBytes: 1 << 30,
					ReceiptRecordedAtUnixMs: uint64(now.Add(5 * time.Second).UnixMilli()),
					Correlation:             proto.Clone(advanceCommand.Correlation).(*runnerv1.Correlation),
				},
			},
		},
	}, now.Add(5*time.Second)); err != nil || duplicate {
		t.Fatalf("local generation result duplicate, error = %t, %v", duplicate, err)
	}
	redundantFencePayload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Fence{
			Fence: proto.Clone(fenceDelivery.Message.GetFence()).(*runnerv1.FenceCommand),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	redundantFenceCommandID := task4ID("redundant-fence")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'fence',$4,'delivered',$5,1,$6,$6,$6)`,
		redundantFenceCommandID,
		runnerID,
		durableAssignment.ID,
		redundantFencePayload,
		connectionID,
		now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	redundantFenceResult := proto.Clone(fenceResult).(*runnerv1.FenceResult)
	redundantFenceResult.MessageId = "redundant-fence-result-8"
	redundantFenceResult.Sequence = 8
	redundantFenceResult.Result =
		runnerv1.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED
	if duplicate, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventFence, RunnerID: runnerID, ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_FenceResult{
				FenceResult: redundantFenceResult,
			},
		},
	}, now.Add(5*time.Second)); err != nil || duplicate {
		t.Fatalf("redundant FenceResult duplicate, error = %t, %v", duplicate, err)
	}
	var redundantFenceState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.runner_commands WHERE id=$1`,
		redundantFenceCommandID,
	).Scan(&redundantFenceState); err != nil {
		t.Fatal(err)
	}
	if redundantFenceState != "acknowledged" {
		t.Fatalf(
			"redundant Fence command state = %q, want acknowledged",
			redundantFenceState,
		)
	}
	staleAfterFence := proto.Clone(ready).(*runnerv1.RunnerToControlPlane)
	staleAfterFence.GetAssignmentResult().MessageId = "ready-after-fence-9"
	staleAfterFence.GetAssignmentResult().Sequence = 9
	staleAfterFence.GetAssignmentResult().BackendReference = "fc-stale-after-fence"
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID, ConnectionID: connectionID,
		Message: staleAfterFence,
	}, now.Add(5*time.Second)); !errors.Is(err, runnercontrol.ErrStaleAssignmentEvidence) {
		t.Fatalf("post-fence stale result error = %v, want ErrStaleAssignmentEvidence", err)
	}

	reconnectedID := task4ID("connection")
	if err := stateStore.OpenConnection(
		t.Context(), issued.Identity, reconnectedID, 1, now.Add(6*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	reconnectedRegistration := task4Registration(runnerID, reconnectedID, poolName)
	reconnectedRegistration.MessageId = "reconnected-registration-1"
	if duplicate, err := stateStore.RecordRegistration(
		t.Context(), reconnectedRegistration, now.Add(6*time.Second),
	); err != nil || duplicate {
		t.Fatalf("reconnected Registration duplicate, error = %t, %v", duplicate, err)
	}
	task4CompleteWorkspaceReconciliation(
		t,
		stateStore,
		runnerID,
		reconnectedID,
		2,
		[]*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          workspaceID,
			Generation:           2,
			LogicalCapacityBytes: 1 << 30,
			Formatted:            true,
		}},
		now.Add(6*time.Second),
	)
	if _, err := stateStore.RecordHeartbeat(
		t.Context(),
		task4Heartbeat(runnerID, connectionID, "stale-old-connection", 4, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE),
		now.Add(6*time.Second),
	); err == nil {
		t.Fatal("superseded connection Heartbeat was accepted")
	}
	drainCommand := &runnerv1.DrainCommand{
		MessageId:      task4ID("drain-command"),
		Mode:           runnerv1.DrainMode_DRAIN_MODE_BOUNDED,
		DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()),
	}
	if err := reconcileStore.RequestRunnerDrain(
		t.Context(), runnerID, drainCommand, now.Add(7*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	drainDelivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, reconnectedID, now.Add(7*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("Drain command delivery found, error = %t, %v", found, err)
	}
	if drainDelivery.Message.GetDrain() == nil ||
		drainDelivery.Message.GetDrain().Sequence != 2 ||
		drainDelivery.Message.GetDrain().Mode != runnerv1.DrainMode_DRAIN_MODE_BOUNDED {
		t.Fatalf("claimed Drain command lacks durable authority: %#v", drainDelivery.Message.GetDrain())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), drainDelivery, reconnectedID, now.Add(7*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := stateStore.RecordHeartbeat(
		t.Context(),
		task4Heartbeat(runnerID, reconnectedID, "draining-heartbeat-3", 3, runnerv1.DrainPhase_DRAIN_PHASE_DRAINING),
		now.Add(8*time.Second),
	); err != nil || duplicate {
		t.Fatalf("draining Heartbeat duplicate, error = %t, %v", duplicate, err)
	}
}

const task4RunnerCredential = "task-4-pre-shared-runner-credential-material"

type task4IssuedCertificate struct {
	Identity       runnercontrol.RunnerIdentity
	CertificatePEM []byte
}

type task4TestCredentialAuthority struct {
	t           *testing.T
	verifier    *runnercontrol.CredentialAuthority
	certificate *x509.Certificate
	privateKey  ed25519.PrivateKey
	now         time.Time
}

func newTask4CredentialAuthority(
	t *testing.T,
	caCertificate *x509.Certificate,
	caPrivateKey ed25519.PrivateKey,
	now time.Time,
) *task4TestCredentialAuthority {
	t.Helper()
	verifier, err := runnercontrol.NewCredentialAuthority(runnercontrol.CredentialAuthorityConfig{
		Credential: task4RunnerCredential, CACertificate: caCertificate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &task4TestCredentialAuthority{
		t: t, verifier: verifier, certificate: caCertificate,
		privateKey: caPrivateKey, now: now,
	}
}

func (authority *task4TestCredentialAuthority) Issue(
	runnerID string,
	certificateRequestPEM []byte,
) (task4IssuedCertificate, error) {
	block, remainder := pem.Decode(certificateRequestPEM)
	if block == nil || len(remainder) != 0 {
		return task4IssuedCertificate{}, errors.New("invalid test CSR")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return task4IssuedCertificate{}, err
	}
	serial := big.NewInt(10_000 + task4Sequence.Add(1))
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: runnerID},
		// x509 verification uses wall-clock time, not the frozen logical clock
		// these tests reason with, so the validity window must be expressed in
		// wall-clock. Deriving it from the frozen date made every runner-trust
		// test expire exactly one day after that date.
		NotBefore: time.Now().UTC().Add(-time.Hour),
		NotAfter:  time.Now().UTC().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{{Scheme: "spiffe", Host: "secondbox", Path: "/runner/" + url.PathEscape(runnerID)}},
	}
	der, err := x509.CreateCertificate(
		rand.Reader, template, authority.certificate, request.PublicKey, authority.privateKey,
	)
	if err != nil {
		return task4IssuedCertificate{}, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return task4IssuedCertificate{}, err
	}
	identity, err := authority.VerifyClientCertificate(
		context.Background(), certificate, task4RunnerCredential,
	)
	if err != nil {
		return task4IssuedCertificate{}, err
	}
	return task4IssuedCertificate{
		Identity:       identity,
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func (authority *task4TestCredentialAuthority) VerifyClientCertificate(
	ctx context.Context,
	certificate *x509.Certificate,
	credential string,
) (runnercontrol.RunnerIdentity, error) {
	return authority.verifier.VerifyClientCertificate(ctx, certificate, credential)
}

func (authority *task4TestCredentialAuthority) ServerTLSConfig(
	certificate tls.Certificate,
) (*tls.Config, error) {
	return authority.verifier.ServerTLSConfig(certificate)
}

func task4CertificateAuthority(
	t *testing.T,
	now time.Time,
) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(9001), Subject: pkix.Name{CommonName: "Task 4 runner CA"},
		// Wall-clock for the same reason as the leaf certificates below.
		NotBefore: time.Now().UTC().Add(-time.Hour),
		NotAfter:  time.Now().UTC().Add(10 * 365 * 24 * time.Hour),
		IsCA:      true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, privateKey
}

func task4CertificateRequest(t *testing.T) []byte {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestDER, err := x509.CreateCertificateRequest(
		rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "runner"}}, privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER})
}

func task4ParseCertificate(t *testing.T, certificatePEM []byte) *x509.Certificate {
	t.Helper()
	block, remainder := pem.Decode(certificatePEM)
	if block == nil || len(remainder) != 0 {
		t.Fatal("invalid issued certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func task4ServerCertificate(
	t *testing.T,
	caCertificate *x509.Certificate,
	caPrivateKey ed25519.PrivateKey,
	now time.Time,
) tls.Certificate {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(9100), Subject: pkix.Name{CommonName: "control.secondbox.test"},
		DNSNames: []string{"control.secondbox.test"},
		// Wall-clock, for the same reason as the CA and leaf certificates.
		NotBefore:   time.Now().UTC().Add(-time.Hour),
		NotAfter:    time.Now().UTC().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader, template, caCertificate, publicKey, caPrivateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{certificateDER}, PrivateKey: privateKey}
}

func task4InsertRunnerPool(t *testing.T, poolName string, now time.Time) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ($1,'ready','["amd64"]','["compute","local-workspace","network-policy"]','{}',1,1,$2,$2)`,
		poolName, now,
	); err != nil {
		t.Fatal(err)
	}
}

func task4InsertSchedulableSandbox(
	t *testing.T,
	sandboxID string,
	profileRevisionID string,
	homeRunnerID string,
	now time.Time,
) string {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	workspaceID := task4ID("workspace")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_vcpu_count,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ('task4-project',100,100,100000,1099511627776,100,100,100,100,100,$4)
		ON CONFLICT (tenant_ref) DO NOTHING;
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_vcpu_count,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ('task4-project','task4-subject',100,100,100000,1099511627776,100,100,100,$4)
		ON CONFLICT (tenant_ref,subject_ref) DO NOTHING;
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES (
			$1,'task4-project','task4-subject',$2,$3,'ready',1073741824,
			1,'','','','',NULL,NULL,'','{}',$4,$4
		)`,
		pgx.QueryExecModeSimpleProtocol, workspaceID, sandboxID, homeRunnerID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,generation,
			workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			last_activity_at,revision,created_at,updated_at,deleted_at
		) VALUES (
			$2,'task4-project','task4-subject','task4-profile',$4,'creating','running',1,$1,'',
			'{}','{}',NULL,1,$3,$3,NULL
		)`, workspaceID, sandboxID, now, profileRevisionID,
	); err != nil {
		t.Fatal(err)
	}
	return workspaceID
}

func task4Registration(
	runnerID string,
	connectionID string,
	poolName string,
) *runnerv1.RunnerRegistration {
	return &runnerv1.RunnerRegistration{
		MessageId: "registration-1", Sequence: 1, RunnerId: runnerID,
		ConnectionId: connectionID, RunnerPoolId: poolName,
		SoftwareVersion: "1.0.0", ProtocolVersion: 3,
		BackendKind: runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_FIRECRACKER,
		Capabilities: &runnerv1.RunnerCapabilities{
			Architecture: "amd64", ComputeBackendVersion: "1.16.1",
			HypervisorReady: true, IsolationReady: true, ResourceLimitsReady: true,
			NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
			DataPlaneReady:           true,
			GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
		},
		Allocatable: &runnerv1.Capacity{
			VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
			Instances: 8, Operations: 32,
		},
		Reserved: &runnerv1.Capacity{},
		Materializations: []*runnerv1.BackendMaterializationEvidence{{
			SchemaVersion:           1,
			BackendKind:             runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_FIRECRACKER,
			GuestArchitecture:       "amd64",
			RuntimeManifestDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			MaterializationDigest:   "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			AgentProtocolGeneration: 1, BackendBuildId: "firecracker-1.16.1", VerifiedAtUnixMs: 1,
		}},
		StartupTiming:                  &runnerv1.StartupTiming{SampleCount: 2, P95Milliseconds: 50},
		DataPlaneAdvertisedAddress:     "10.0.0.5:7443",
		DataPlaneCertificateSpkiSha256: strings.Repeat("a", 64),
	}
}

func task4Heartbeat(
	runnerID string,
	connectionID string,
	messageID string,
	sequence uint64,
	phase runnerv1.DrainPhase,
) *runnerv1.RunnerHeartbeat {
	return &runnerv1.RunnerHeartbeat{
		MessageId: messageID, Sequence: sequence, RunnerId: runnerID,
		ConnectionId: connectionID, ObservedAtUnixMs: 1,
		Allocatable: &runnerv1.Capacity{
			VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
			Instances: 8, Operations: 32,
		},
		Reserved: &runnerv1.Capacity{}, DrainPhase: phase,
		StartupTiming: &runnerv1.StartupTiming{SampleCount: 4, P95Milliseconds: 75},
	}
}

func task4CompleteWorkspaceReconciliation(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	sequence uint64,
	inventory []*runnerv1.LocalWorkspaceInventoryItem,
	now time.Time,
) {
	t.Helper()
	delivery, found, err := stateStore.ClaimCommand(
		t.Context(),
		runnerID,
		connectionID,
		now,
	)
	if err != nil || !found {
		t.Fatalf("Workspace reconciliation delivery found=%t error=%v", found, err)
	}
	command := delivery.Message.GetLocalWorkspace()
	if command == nil ||
		command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE ||
		command.OperationId != delivery.ID ||
		command.EffectId != delivery.ID {
		t.Fatalf("returning-runner reconciliation command = %#v", delivery.Message)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(),
		delivery,
		connectionID,
		now,
	); err != nil {
		t.Fatal(err)
	}
	duplicate, err := stateStore.RecordEvent(
		t.Context(),
		runnercontrol.Event{
			Kind:         runnercontrol.EventLocalWorkspace,
			RunnerID:     runnerID,
			ConnectionID: connectionID,
			Message: &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{
					LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{
						MessageId:      fmt.Sprintf("workspace-reconcile-result-%d", sequence),
						Sequence:       sequence,
						CommandVersion: 1,
						Kind:           command.Kind,
						Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
						OperationId:    command.OperationId,
						EffectId:       command.EffectId,
						Inventory:      inventory,
						Correlation:    proto.Clone(command.Correlation).(*runnerv1.Correlation),
					},
				},
			},
		},
		now,
	)
	if err != nil || duplicate {
		t.Fatalf("Workspace reconciliation result duplicate=%t error=%v", duplicate, err)
	}
}

func task4ID(prefix string) string {
	return prefix + "-" + big.NewInt(task4Sequence.Add(1)).String()
}

func task4IDForIndex(prefix string, index int) string {
	return prefix + "-" + big.NewInt(task4Sequence.Add(1)).String() + "-" + big.NewInt(int64(index)).String()
}

// TestRunnerPoolSealsToGVisorAndRejectsOtherBackends proves the third backend
// kind registers through the same evidence validation and seals a pool exactly
// like the first two, with no sealing-mechanism change.
func TestRunnerPoolSealsToGVisorAndRejectsOtherBackends(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	poolName := task4ID("gvisor-sealed-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)

	register := func(runnerID, connectionID string, registration *runnerv1.RunnerRegistration) error {
		issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
		if err != nil {
			return err
		}
		if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
			return err
		}
		_, err = stateStore.RecordRegistration(t.Context(), registration, now)
		return err
	}

	gvisorRunner, gvisorConnection := task4ID("gvisor-runner"), task4ID("gvisor-connection")
	gvisorRegistration := task4Registration(gvisorRunner, gvisorConnection, poolName)
	gvisorRegistration.BackendKind = runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_GVISOR
	gvisorRegistration.Materializations[0].BackendKind = gvisorRegistration.BackendKind
	gvisorRegistration.Materializations[0].SourceOciManifestDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	gvisorRegistration.Materializations[0].FlatRootDigest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	gvisorRegistration.Materializations[0].HelperBuildId = "runsc-release-20260817.0"
	if err := register(gvisorRunner, gvisorConnection, gvisorRegistration); err != nil {
		t.Fatal(err)
	}

	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)
	var sealedKind string
	if err := databasePool.QueryRow(t.Context(), `SELECT backend_kind FROM secondbox.runner_pools WHERE name=$1`, poolName).Scan(&sealedKind); err != nil {
		t.Fatal(err)
	}
	if sealedKind != "gvisor" {
		t.Fatalf("sealed backend kind = %q", sealedKind)
	}

	firecrackerRunner, firecrackerConnection := task4ID("firecracker-into-gvisor"), task4ID("firecracker-into-gvisor-connection")
	mismatch := task4Registration(firecrackerRunner, firecrackerConnection, poolName)
	if err := register(firecrackerRunner, firecrackerConnection, mismatch); err == nil || !strings.Contains(err.Error(), "sealed to a different compute backend") {
		t.Fatalf("firecracker into gvisor pool registration error = %v", err)
	}
}

// TestGVisorRegistrationRequiresOCIMaterializationEvidence proves the evidence
// branch: a gVisor registration without the digest-pinned OCI identity is
// rejected before the pool seals.
func TestGVisorRegistrationRequiresOCIMaterializationEvidence(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 30, 0, 0, time.UTC)
	poolName := task4ID("gvisor-evidence-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)

	runnerID, connectionID := task4ID("gvisor-incomplete-runner"), task4ID("gvisor-incomplete-connection")
	registration := task4Registration(runnerID, connectionID, poolName)
	registration.BackendKind = runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_GVISOR
	registration.Materializations[0].BackendKind = registration.BackendKind
	// The OCI identity fields stay empty: this must fail prerequisites.
	issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.RecordRegistration(t.Context(), registration, now); err == nil {
		t.Fatal("gVisor registration without OCI materialization evidence was accepted")
	}
	var sealedKind string
	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)
	if err := databasePool.QueryRow(t.Context(), `SELECT backend_kind FROM secondbox.runner_pools WHERE name=$1`, poolName).Scan(&sealedKind); err != nil {
		t.Fatal(err)
	}
	if sealedKind != "" {
		t.Fatalf("pool sealed to %q by a rejected registration", sealedKind)
	}
}
