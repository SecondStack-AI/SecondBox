package integration_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var task4Sequence atomic.Int64

func TestRunnerEnrollmentRotationRevocationAndMTLSIdentity(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	poolName := task4ID("credential-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(t.Context(), runnercontrol.EnrollmentRequest{
		TokenID: task4ID("enrollment"), RunnerID: task4ID("runner"),
		PoolName: poolName, RunnerName: "credential runner", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	csr := task4CertificateRequest(t)
	issued, err := authority.RedeemEnrollment(t.Context(), enrollment.Token, csr)
	if err != nil {
		t.Fatal(err)
	}
	certificate := task4ParseCertificate(t, issued.CertificatePEM)
	identity, err := authority.VerifyClientCertificate(t.Context(), certificate)
	if err != nil {
		t.Fatal(err)
	}
	if identity.RunnerID != enrollment.RunnerID {
		t.Fatalf("certificate Runner identity = %q, want %q", identity.RunnerID, enrollment.RunnerID)
	}
	if _, err := authority.RedeemEnrollment(t.Context(), enrollment.Token, task4CertificateRequest(t)); !errors.Is(err, runnercontrol.ErrRunnerEnrollmentInvalid) {
		t.Fatalf("reused enrollment error = %v, want ErrRunnerEnrollmentInvalid", err)
	}

	rotated, err := authority.RotateCredential(t.Context(), identity, task4CertificateRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.VerifyClientCertificate(t.Context(), certificate); err != nil {
		t.Fatalf("retiring credential was not valid during rotation overlap: %v", err)
	}
	rotatedCertificate := task4ParseCertificate(t, rotated.CertificatePEM)
	if _, err := authority.VerifyClientCertificate(t.Context(), rotatedCertificate); err != nil {
		t.Fatalf("replacement credential was not active: %v", err)
	}
	if err := authority.RevokeCredential(t.Context(), identity.CredentialSerial); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.VerifyClientCertificate(t.Context(), certificate); !errors.Is(err, runnercontrol.ErrRunnerCredentialRevoked) {
		t.Fatalf("revoked credential verification error = %v, want ErrRunnerCredentialRevoked", err)
	}
	if _, err := authority.VerifyClientCertificate(t.Context(), rotatedCertificate); err != nil {
		t.Fatalf("replacement credential was revoked with predecessor: %v", err)
	}

	serverCertificate := task4ServerCertificate(t, caCertificate, caPrivateKey, now)
	tlsConfig, err := authority.ServerTLSConfig(serverCertificate)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.MinVersion != 0x0304 || tlsConfig.VerifyConnection == nil {
		t.Fatalf("runner mTLS server config lacks TLS 1.3 or revocation callback: %#v", tlsConfig)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var persistedHash []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT token_hash FROM secondbox.runner_enrollment_tokens WHERE id=$1`,
		enrollment.TokenID,
	).Scan(&persistedHash); err != nil {
		t.Fatal(err)
	}
	if len(persistedHash) != 32 || string(persistedHash) == enrollment.Token {
		t.Fatalf("runner enrollment did not persist exactly one non-plaintext keyed hash: %x", persistedHash)
	}
}

func TestRunnerEnrollmentRejectsPoolsThatAreNotReady(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 30, 0, 0, time.UTC)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	for _, state := range []string{"draining", "offline"} {
		poolName := task4ID(state + "-pool")
		task4InsertRunnerPool(t, poolName, now)
		if _, err := pool.Exec(
			t.Context(),
			`UPDATE secondbox.runner_pools SET state=$2 WHERE name=$1`,
			poolName,
			state,
		); err != nil {
			t.Fatal(err)
		}
		_, err := authority.CreateEnrollment(t.Context(), runnercontrol.EnrollmentRequest{
			TokenID:    task4ID(state + "-enrollment"),
			RunnerID:   task4ID(state + "-runner"),
			PoolName:   poolName,
			RunnerName: state + " pool runner",
			ExpiresAt:  now.Add(time.Hour),
		})
		if err == nil || err.Error() != "SecondBox RunnerPool is not accepting enrollment" {
			t.Fatalf("%s pool enrollment error = %v", state, err)
		}
	}
}

func TestRunnerProtocolPersistenceAndMultiControlPlaneSchedulingAreReplicaSafe(t *testing.T) {
	now := time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	poolName := task4ID("scheduler-pool")
	runnerID := task4ID("runner")
	connectionID := task4ID("connection")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(t.Context(), runnercontrol.EnrollmentRequest{
		TokenID: task4ID("enrollment"), RunnerID: runnerID, PoolName: poolName,
		RunnerName: "scheduler runner", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.RedeemEnrollment(t.Context(), enrollment.Token, task4CertificateRequest(t))
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

	sandboxID := task4ID("sandbox")
	profileRevisionID := task4ID("profile-revision")
	workspaceID := task4InsertSchedulableSandbox(t, sandboxID, profileRevisionID, now)
	firstScheduler, err := scheduler.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(firstScheduler.Close)
	secondScheduler, err := scheduler.NewPostgresStore(t.Context(), integrationDatabaseURL)
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
			assignment, created, err := schedulerStore.Schedule(t.Context(), scheduler.ScheduleRequest{
				AssignmentID: assignmentID, AssignmentCommandID: task4IDForIndex("assignment-command", index),
				InstanceID: instanceID, SandboxID: sandboxID, ProfileRevisionID: profileRevisionID,
				WorkspaceID: workspaceID, MaterializationID: task4IDForIndex("materialization", index),
				Requirements: scheduler.Requirements{
					PoolName: poolName, BackendKind: "firecracker", Architecture: "amd64",
					RequiredCapabilities:    []string{"checkpoint", "network-policy"},
					GuestProtocolGeneration: 1,
					Capacity: scheduler.Capacity{
						CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
						Instances: 1, Operations: 1,
					},
					PreferredArtifactDigests: []string{runtimeDigest},
				},
				AssignmentCommand: &runnerv1.AssignmentCommand{
					Fence: &runnerv1.AssignmentFence{
						AssignmentId: assignmentID, SandboxId: sandboxID, InstanceId: instanceID,
						SandboxGeneration: 1, FencingToken: fencingToken,
					},
					ProfileRevisionId: profileRevisionID,
					Requirements: &runnerv1.ProfileRequirements{
						VcpuCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
						Architecture: "amd64", RequiredCapabilities: []string{"checkpoint", "network-policy"},
						MaximumOperationMs: 60_000, MaximumOutputBytes: 1 << 20,
					},
					Assets: []*runnerv1.SignedAssetReference{
						{
							ArtifactId: "runtime", ManifestDigest: runtimeDigest,
							SignatureKeyId: "release-key-1", Architecture: "amd64",
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
			len(result.assignment.FencingToken) != 32 {
			t.Fatalf("replica returned divergent durable Assignment: %#v", result.assignment)
		}
	}
	if createdCount != 1 {
		t.Fatalf("replica race created %d Assignments, want exactly 1", createdCount)
	}
	delivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionID, now.Add(time.Second),
	)
	if err != nil || !found {
		t.Fatalf("Assignment command delivery found, error = %t, %v", found, err)
	}
	if delivery.Message.GetAssignment() == nil ||
		delivery.Message.GetAssignment().Fence.AssignmentId != durableAssignment.ID ||
		delivery.Message.GetAssignment().Sequence != 1 ||
		delivery.Message.GetAssignment().MessageId != delivery.ID {
		t.Fatalf("claimed Assignment command lacks durable authority: %#v", delivery.Message.GetAssignment())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), delivery.ID, connectionID, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	staleReady := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: "ready-stale-3", Sequence: 3,
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
	ready := proto.Clone(staleReady).(*runnerv1.RunnerToControlPlane)
	ready.GetAssignmentResult().MessageId = "ready-3"
	ready.GetAssignmentResult().Fence.FencingToken = durableAssignment.FencingToken
	ready.GetAssignmentResult().BackendReference = "fc-instance-1"
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID, ConnectionID: connectionID,
		Message: ready,
	}, now); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var count int
	var backendReference, capabilityJSON, sandboxState, guestLiveness, materializationState string
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*),max(assignment.backend_reference),max(assignment.capability_snapshot_json::text),
		       max(sandbox.state),max(instance.guest_liveness),max(materialization.state)
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.workspace_materializations AS materialization
		  ON materialization.assignment_id=assignment.id
		WHERE assignment.sandbox_id=$1`, sandboxID,
	).Scan(
		&count, &backendReference, &capabilityJSON, &sandboxState,
		&guestLiveness, &materializationState,
	); err != nil {
		t.Fatal(err)
	}
	if count != 1 || backendReference != "fc-instance-1" ||
		!json.Valid([]byte(capabilityJSON)) ||
		sandboxState != "starting" || guestLiveness != "ready" || materializationState != "ready" {
		t.Fatalf(
			"durable Assignment evidence = count %d, backend %q, capability %q, Sandbox %q, guest %q, materialization %q",
			count, backendReference, capabilityJSON, sandboxState, guestLiveness, materializationState,
		)
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
	if lossDecision.Action != reconcile.ActionFence || lossDecision.MayReassign {
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
		fenceDelivery.Message.GetFence().Sequence != 2 {
		t.Fatalf("claimed Fence command lacks durable authority: %#v", fenceDelivery.Message.GetFence())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), fenceDelivery.ID, connectionID, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	fenceResult := &runnerv1.FenceResult{
		MessageId: "fence-result-4", Sequence: 4,
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
		t.Fatalf("replacement generation = %d, want 2", nextGeneration)
	}
	staleAfterFence := proto.Clone(ready).(*runnerv1.RunnerToControlPlane)
	staleAfterFence.GetAssignmentResult().MessageId = "ready-after-fence-5"
	staleAfterFence.GetAssignmentResult().Sequence = 5
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
		drainDelivery.Message.GetDrain().Sequence != 1 ||
		drainDelivery.Message.GetDrain().Mode != runnerv1.DrainMode_DRAIN_MODE_BOUNDED {
		t.Fatalf("claimed Drain command lacks durable authority: %#v", drainDelivery.Message.GetDrain())
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), drainDelivery.ID, reconnectedID, now.Add(7*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := stateStore.RecordHeartbeat(
		t.Context(),
		task4Heartbeat(runnerID, reconnectedID, "draining-heartbeat-2", 2, runnerv1.DrainPhase_DRAIN_PHASE_DRAINING),
		now.Add(8*time.Second),
	); err != nil || duplicate {
		t.Fatalf("draining Heartbeat duplicate, error = %t, %v", duplicate, err)
	}
}

func task4CredentialAuthority(
	t *testing.T,
	caCertificate *x509.Certificate,
	caPrivateKey ed25519.PrivateKey,
	now time.Time,
) *runnercontrol.CredentialAuthority {
	t.Helper()
	authority, err := runnercontrol.NewCredentialAuthority(t.Context(), runnercontrol.CredentialAuthorityConfig{
		DatabaseURL:          integrationDatabaseURL,
		EnrollmentHashSecret: []byte("task-4-runner-enrollment-hash-secret-32-bytes"),
		CACertificate:        caCertificate, CAPrivateKey: caPrivateKey,
		CertificateLifetime:           24 * time.Hour,
		CredentialVerificationTimeout: 5 * time.Second,
		Now:                           func() time.Time { return now },
		NewToken:                      func() string { return "runner-enrollment-secret-material-0123456789" },
		NewSerial:                     func() *big.Int { return big.NewInt(10_000 + task4Sequence.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authority.Close)
	return authority
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
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
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
		DNSNames:  []string{"control.secondbox.test"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
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
		) VALUES ($1,'ready','["amd64"]','["firecracker","checkpoint","network-policy"]','{}',1,1,$2,$2)`,
		poolName, now,
	); err != nil {
		t.Fatal(err)
	}
}

func task4InsertSchedulableSandbox(
	t *testing.T,
	sandboxID string,
	profileRevisionID string,
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
		INSERT INTO secondbox.workspaces (
			id,project_id,sandbox_id,generation,retained_bytes,current_checkpoint_id,
			created_at,updated_at
		) VALUES ($1,'task4-project',$2,1,0,'checkpoint-current',$3,$3)`,
		workspaceID, sandboxID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,project_id,profile_name,profile_revision_id,state,desired_state,generation,
			workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			last_activity_at,revision,created_at,updated_at,deleted_at
		) VALUES (
			$2,'task4-project','task4-profile',$4,'creating','running',1,$1,'',
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
		SoftwareVersion: "1.0.0", ProtocolVersion: 1,
		Capabilities: &runnerv1.RunnerCapabilities{
			Architecture: "amd64", FirecrackerVersion: "1.16.1",
			KvmReady: true, JailerReady: true, CgroupReady: true,
			NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
			GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
		},
		Allocatable: &runnerv1.Capacity{
			VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
			Instances: 8, Operations: 32,
		},
		Reserved: &runnerv1.Capacity{},
		ArtifactCache: []*runnerv1.ArtifactCacheEvidence{
			{ArtifactId: "runtime", ManifestDigest: "sha256:runtime", VerifiedAtUnixMs: 1},
		},
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
			VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
			Instances: 8, Operations: 32,
		},
		Reserved: &runnerv1.Capacity{}, DrainPhase: phase,
	}
}

func task4ID(prefix string) string {
	return prefix + "-" + big.NewInt(task4Sequence.Add(1)).String()
}

func task4IDForIndex(prefix string, index int) string {
	return prefix + "-" + big.NewInt(task4Sequence.Add(1)).String() + "-" + big.NewInt(int64(index)).String()
}
