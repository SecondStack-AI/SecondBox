package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestDrainRejectsNewWorkAndCancelsExistingWorkAtProfileGraceBeforeCheckpoint(
	t *testing.T,
) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "drain-admission-barrier",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-drain-admission-barrier",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	principal.Scopes = append(principal.Scopes, contracts.ScopeSandboxExec)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "drain-admission-barrier-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(),
		runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL:   integrationDatabaseURL,
			ClaimDuration: time.Second, Retention: time.Hour,
			MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	existing, _, err := relay.AdmitDataPlane(
		t.Context(),
		runnercontrol.DataPlaneAdmission{
			ID:        "dps-drain-barrier-" + sandbox.ID,
			StreamID:  "stream-drain-barrier-" + sandbox.ID,
			ProjectID: principal.ProjectID, SandboxID: sandbox.ID,
			ServiceAccountID: principal.ServiceAccountID,
			Generation:       sandbox.Generation, Kind: "exec", Operation: "exec",
			RequestID:      "request-drain-barrier-existing",
			IdempotencyKey: "drain-barrier-existing",
			RequestHash:    "drain-barrier-existing-hash",
			DeadlineAt:     now.Add(time.Minute), MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "sleep 60"},
				DeadlineUnixMs:   uint64(now.Add(time.Minute).UnixMilli()),
				OutputLimitBytes: 1024,
			},
			Now: now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), seed.RunnerID, seed.ConnectionOne, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if err := relay.MarkOutboundFrameDelivered(
			t.Context(), delivery.ID, seed.ConnectionOne, now,
		); err != nil {
			t.Fatal(err)
		}
	}

	current, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.DrainSandbox(
		t.Context(), principal, sandbox.ID, "drain-admission-barrier",
		current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=CASE
		  WHEN id=$1 THEN $2::timestamptz
		  ELSE $3::timestamptz
		END`,
		sandbox.ID, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, WorkerID: "drain-admission-barrier",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("drain barrier decision = %#v, %t, %v", decision, found, err)
	}

	_, _, err = relay.AdmitDataPlane(
		t.Context(),
		runnercontrol.DataPlaneAdmission{
			ID:        "dps-drain-rejected-" + sandbox.ID,
			StreamID:  "stream-drain-rejected-" + sandbox.ID,
			ProjectID: principal.ProjectID, SandboxID: sandbox.ID,
			ServiceAccountID: principal.ServiceAccountID,
			Generation:       sandbox.Generation, Kind: "exec", Operation: "exec",
			RequestID:      "request-drain-rejected",
			IdempotencyKey: "drain-rejected",
			RequestHash:    "drain-rejected-hash",
			DeadlineAt:     now.Add(time.Minute), MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "true"},
				DeadlineUnixMs:   uint64(now.Add(time.Minute).UnixMilli()),
				OutputLimitBytes: 1024,
			},
			Now: now.Add(time.Millisecond),
		},
	)
	if !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("post-drain data-plane admission error = %v", err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(29*time.Second),
	); err != nil || !found || decision.Action != lifecycle.ActionWait {
		t.Fatalf("pre-grace drain decision = %#v, %t, %v", decision, found, err)
	}
	stillActive, err := relay.GetDataPlaneSession(
		t.Context(), principal.ProjectID, existing.ID,
	)
	if err != nil || stillActive.State != "running" {
		t.Fatalf("pre-grace existing work = %#v, %v", stillActive, err)
	}

	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, checkpointUnusedScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 1, SerializationRetryLimit: 1,
			AssetCatalog: profileLifecycleAssetCatalog{}, SessionCanceller: relay,
			NewID: func(prefix string) string {
				return fmt.Sprintf("%s-drain-barrier-%s", prefix, sandbox.ID)
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
	reconciler.Effects = effectBroker
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(30*time.Second),
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("expired-grace drain decision = %#v, %t, %v", decision, found, err)
	}
	cancelling, err := relay.GetDataPlaneSession(
		t.Context(), principal.ProjectID, existing.ID,
	)
	if err != nil || cancelling.State != "cancelling" ||
		cancelling.TerminalKind !=
			runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("expired-grace existing work = %#v, %v", cancelling, err)
	}
	cancelDelivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(30*time.Second),
	)
	if err != nil || !found || cancelDelivery.Message.GetExec().GetCancel() == nil {
		t.Fatalf("expired-grace cancellation = %#v, %t, %v", cancelDelivery, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), cancelDelivery.ID, seed.ConnectionOne, now.Add(30*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if inserted, err := relay.PersistInboundFrame(
		t.Context(),
		runnercontrol.InboundRelayFrame{
			RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne,
			Message: &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_Exec{
					Exec: &runnerv1.ExecFrame{
						Fence: seed.Fence, OperationId: existing.ID,
						StreamId: existing.StreamID, Sequence: 1,
						Payload: &runnerv1.ExecFrame_Terminal{
							Terminal: &runnerv1.ExecTerminal{
								Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
							},
						},
					},
				},
			},
		},
		now.Add(30*time.Second+time.Millisecond),
	); err != nil || !inserted {
		t.Fatalf("expired-grace terminal proof = %t, %v", inserted, err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(30*time.Second+2*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("post-cancellation checkpoint decision = %#v, %t, %v", decision, found, err)
	}
	var sandboxState, checkpointCommandState string
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.state,command.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.lifecycle_effects AS effect ON effect.sandbox_id=sandbox.id
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE sandbox.id=$1 AND effect.kind='checkpoint'`,
		sandbox.ID,
	).Scan(&sandboxState, &checkpointCommandState); err != nil {
		t.Fatal(err)
	}
	if sandboxState != contracts.SandboxStateCheckpointing ||
		checkpointCommandState != "pending" {
		t.Fatalf(
			"post-cancellation checkpoint state = Sandbox %q command %q",
			sandboxState, checkpointCommandState,
		)
	}
}

func TestStartDuringStopSurvivesControlPlaneRestartAndRunnerCommandReconnect(
	t *testing.T,
) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "restart-stop-start",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-restart-stop-start",
	)
	disposableSpec := profile.CurrentRevision.Spec
	disposableSpec.Checkpoint.OnStop = false
	profile, err := controlPlane.ReviseProfile(
		t.Context(), admin, profile.Name,
		contracts.ReviseProfileRequest{Spec: disposableSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "restart-stop-start-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	const (
		runnerID     = "runner-restart-stop-start"
		connectionA  = "connection-restart-stop-start-a"
		connectionB  = "connection-restart-stop-start-b"
		runnerPoolID = "default-pool"
	)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.runners SET state='offline',updated_at=now() WHERE id=$1`,
			runnerID,
		); err != nil {
			t.Errorf("clean restart matrix Runner: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.sandboxes
			SET state='stopped',desired_state='stopped',
			    next_reconcile_at='2999-01-01 00:00:00+00',
			    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
			WHERE id=$1`,
			sandbox.ID,
		); err != nil {
			t.Errorf("clean restart matrix Sandbox: %v", err)
		}
	})
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(
		t.Context(),
		runnercontrol.EnrollmentRequest{
			TokenID: "enrollment-restart-stop-start", RunnerID: runnerID,
			PoolName: runnerPoolID, RunnerName: runnerID, ExpiresAt: now.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.RedeemEnrollment(
		t.Context(), enrollment.Token, task4CertificateRequest(t),
	)
	if err != nil {
		t.Fatal(err)
	}

	stateStore, err := runnercontrol.NewPostgresStateStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.OpenConnection(
		t.Context(), issued.Identity, connectionA, 1, now,
	); err != nil {
		t.Fatal(err)
	}
	firstRegistration := restartMatrixRegistration(
		runnerID, connectionA, runnerPoolID, profile,
	)
	if duplicate, err := stateStore.RecordRegistration(
		t.Context(), firstRegistration, now,
	); err != nil || duplicate {
		t.Fatalf("first Runner registration duplicate, error = %t, %v", duplicate, err)
	}

	schedulerStore, err := scheduler.NewPostgresStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	idSequence := 0
	newEffectBroker := func() *lifecycle.PostgresEffectBroker {
		t.Helper()
		effectBroker, err := lifecycle.NewPostgresEffectBroker(
			t.Context(), integrationDatabaseURL, schedulerStore,
			lifecycle.EffectBrokerConfig{
				AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
				HeartbeatTimeout: time.Minute, RetryLimit: 1, SerializationRetryLimit: 1,
				AssetCatalog:     profileLifecycleAssetCatalog{},
				SessionCanceller: profileLifecycleSessionCanceller{},
				NewID: func(prefix string) string {
					idSequence++
					return fmt.Sprintf("%s-restart-stop-start-%d", prefix, idSequence)
				},
				NewFencingToken: func() ([]byte, error) {
					idSequence++
					return []byte(fmt.Sprintf("%032d", idSequence)), nil
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return effectBroker
	}
	effectBroker := newEffectBroker()
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID:      "restart-stop-start-before-restart",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	startOperation, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "restart-stop-start-initial",
		sandbox.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=CASE
		  WHEN id=$1 THEN $2::timestamptz
		  ELSE $3::timestamptz
		END`,
		sandbox.ID, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionMaterialize {
		t.Fatalf("initial materialize = %#v, %t, %v", decision, found, err)
	}
	firstDelivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionA, now.Add(time.Millisecond),
	)
	if err != nil || !found || firstDelivery.Message.GetAssignment() == nil {
		t.Fatalf("initial Assignment delivery = %#v, %t, %v", firstDelivery, found, err)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), firstDelivery.ID, connectionA, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	firstAssignment := firstDelivery.Message.GetAssignment()
	recordCrossRunnerAssignmentReady(
		t, stateStore, runnerID, connectionA, firstAssignment,
		2, "restart-stop-start-initial-instance", now.Add(2*time.Millisecond),
	)
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(3*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionMarkReady {
		t.Fatalf("initial ready = %#v, %t, %v", decision, found, err)
	}

	ready, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopOperation, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "restart-stop-start-stop", ready.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(4*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("stop drain = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(5*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionStopInstance {
		t.Fatalf("stop fence queue = %#v, %t, %v", decision, found, err)
	}
	fenceBeforeRestart, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionA, now.Add(6*time.Millisecond),
	)
	if err != nil || !found || fenceBeforeRestart.Message.GetFence() == nil {
		t.Fatalf("pre-restart Fence delivery = %#v, %t, %v", fenceBeforeRestart, found, err)
	}

	stopping, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	restartOperation, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "restart-stop-start-overlap",
		stopping.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restartOperation.ID == startOperation.ID || restartOperation.ID == stopOperation.ID {
		t.Fatalf(
			"overlapping start Operation reused old identity: initial=%q stop=%q restart=%q",
			startOperation.ID, stopOperation.ID, restartOperation.ID,
		)
	}

	effectBroker.Close()
	schedulerStore.Close()
	stateStore.Close()
	databaseStore.Close()

	reopenedStore, err := store.NewPostgresControlPlaneStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopenedStore.Close)
	restartedControlPlane := newControlPlaneService(t, reopenedStore, generousQuota())
	restartedPrincipal := authenticateCredential(t, restartedControlPlane, credential)
	stateStore, err = runnercontrol.NewPostgresStateStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	if err := stateStore.OpenConnection(
		t.Context(), issued.Identity, connectionB, 1, now.Add(7*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	reconnectedRegistration := restartMatrixRegistration(
		runnerID, connectionB, runnerPoolID, profile,
	)
	reconnectedRegistration.MessageId = "restart-stop-start-registration-b"
	if duplicate, err := stateStore.RecordRegistration(
		t.Context(), reconnectedRegistration, now.Add(7*time.Millisecond),
	); err != nil || duplicate {
		t.Fatalf("reconnected Runner registration duplicate, error = %t, %v", duplicate, err)
	}
	replayedFence, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionB, now.Add(8*time.Millisecond),
	)
	if err != nil || !found || replayedFence.ID != fenceBeforeRestart.ID ||
		replayedFence.Message.GetFence() == nil ||
		!proto.Equal(
			replayedFence.Message.GetFence().Fence,
			fenceBeforeRestart.Message.GetFence().Fence,
		) {
		t.Fatalf(
			"reconnected Fence delivery = %#v, found=%t, error=%v; original=%#v",
			replayedFence, found, err, fenceBeforeRestart,
		)
	}
	if replayedFence.Message.GetFence().Sequence != 1 {
		t.Fatalf(
			"reconnected Fence control sequence = %d, want 1",
			replayedFence.Message.GetFence().Sequence,
		)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), replayedFence.ID, connectionB, now.Add(8*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := stateStore.RecordEvent(
		t.Context(),
		runnercontrol.Event{
			Kind: runnercontrol.EventFence, RunnerID: runnerID, ConnectionID: connectionB,
			Message: &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_FenceResult{
					FenceResult: &runnerv1.FenceResult{
						MessageId: "restart-stop-start-fence-result", Sequence: 2,
						Fence:                     replayedFence.Message.GetFence().Fence,
						Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
						TerminationEvidenceDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						Correlation: proto.Clone(
							replayedFence.Message.GetFence().Correlation,
						).(*runnerv1.Correlation),
					},
				},
			},
		},
		now.Add(9*time.Millisecond),
	); err != nil || duplicate {
		t.Fatalf("reconnected Fence result duplicate, error = %t, %v", duplicate, err)
	}

	schedulerStore, err = scheduler.NewPostgresStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(schedulerStore.Close)
	effectBroker = newEffectBroker()
	t.Cleanup(effectBroker.Close)
	reconciler = lifecycle.Reconciler{
		Store: reopenedStore, Effects: effectBroker,
		WorkerID:      "restart-stop-start-after-restart",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(10*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionFinishStop {
		t.Fatalf("post-restart finish stop = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(11*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionMaterialize {
		t.Fatalf("post-restart replacement materialize = %#v, %t, %v", decision, found, err)
	}
	replacementDelivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionB, now.Add(12*time.Millisecond),
	)
	if err != nil || !found || replacementDelivery.Message.GetAssignment() == nil {
		t.Fatalf("replacement Assignment delivery = %#v, %t, %v", replacementDelivery, found, err)
	}
	replacementAssignment := replacementDelivery.Message.GetAssignment()
	if replacementAssignment.Fence.SandboxGeneration !=
		firstAssignment.Fence.SandboxGeneration+1 ||
		replacementAssignment.Fence.AssignmentId == firstAssignment.Fence.AssignmentId ||
		replacementAssignment.Sequence != 2 {
		t.Fatalf(
			"replacement Assignment authority = %#v, original=%#v",
			replacementAssignment, firstAssignment,
		)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), replacementDelivery.ID, connectionB, now.Add(12*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	recordCrossRunnerAssignmentReady(
		t, stateStore, runnerID, connectionB, replacementAssignment,
		3, "restart-stop-start-replacement-instance", now.Add(13*time.Millisecond),
	)
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(14*time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionMarkReady {
		t.Fatalf("post-restart replacement ready = %#v, %t, %v", decision, found, err)
	}
	recovered, err := restartedControlPlane.GetSandbox(
		t.Context(), restartedPrincipal, sandbox.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != contracts.SandboxStateReady ||
		recovered.DesiredState != contracts.SandboxDesiredStateRunning ||
		recovered.Generation != sandbox.Generation+1 ||
		recovered.ProfileRevisionID != sandbox.ProfileRevisionID {
		t.Fatalf("post-restart Sandbox = %#v", recovered)
	}
	for _, operationID := range []string{stopOperation.ID, restartOperation.ID} {
		operation, err := restartedControlPlane.GetOperation(
			t.Context(), restartedPrincipal, operationID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State != contracts.OperationStateSucceeded {
			t.Fatalf("post-restart Operation %s = %#v", operationID, operation)
		}
	}
}

func restartMatrixRegistration(
	runnerID string,
	connectionID string,
	poolName string,
	profile contracts.Profile,
) *runnerv1.RunnerRegistration {
	registration := task4Registration(runnerID, connectionID, poolName)
	registration.ArtifactCache = []*runnerv1.ArtifactCacheEvidence{
		{
			ArtifactId: "runtime", ManifestDigest: profile.CurrentRevision.Spec.RuntimeBundleDigest,
			VerifiedAtUnixMs: 1,
		},
		{
			ArtifactId: "toolchain", ManifestDigest: profile.CurrentRevision.Spec.ToolchainBundleDigest,
			VerifiedAtUnixMs: 1,
		},
	}
	return registration
}
