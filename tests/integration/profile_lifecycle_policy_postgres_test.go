package integration_test

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPinnedProfilePolicyDrivesDisposableAndDurableRestartWithoutNameSemantics(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "profile-policy-lifecycle",
	)
	const operatorChosenProfileName = "operator-choice-zebra-47"
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, operatorChosenProfileName,
	)
	principal := authenticateCredential(t, controlPlane, credential)

	disposableSpec := testProfileSpec(1000)
	disposableSpec.Checkpoint.OnStop = false
	disposableRevision, err := controlPlane.ReviseProfile(
		t.Context(), admin, profile.Name,
		contracts.ReviseProfileRequest{Spec: disposableSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	disposable, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "profile-policy-disposable-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	durableSpec := testProfileSpec(1000)
	durableSpec.Checkpoint.OnStop = true
	durableRevision, err := controlPlane.ReviseProfile(
		t.Context(), admin, profile.Name,
		contracts.ReviseProfileRequest{Spec: durableSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	durable, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "profile-policy-durable-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	currentHead, err := controlPlane.ReviseProfile(
		t.Context(), admin, profile.Name,
		contracts.ReviseProfileRequest{Spec: disposableSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposable.ProfileRevisionID != disposableRevision.CurrentRevision.ID ||
		durable.ProfileRevisionID != durableRevision.CurrentRevision.ID ||
		currentHead.CurrentRevision.ID == durable.ProfileRevisionID {
		t.Fatalf(
			"pinned revisions: disposable=%q durable=%q current=%q",
			disposable.ProfileRevisionID, durable.ProfileRevisionID, currentHead.CurrentRevision.ID,
		)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	disposableSeed := seedRelayReadyAssignment(t, disposable, now)
	durableSeed := seedRelayReadyAssignment(t, durable, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	stopSandboxForProfilePolicyTest(t, controlPlane, principal, disposable, "disposable")
	stopSandboxForProfilePolicyTest(t, controlPlane, principal, durable, "durable")
	checkpointID := "chk_profile_policy_" + durable.ID
	driveProfilePolicyStop(
		t, databaseStore, pool, disposable, disposableSeed.Fence.InstanceId,
		"", lifecycle.ActionStopInstance, now,
	)
	driveProfilePolicyStop(
		t, databaseStore, pool, durable, durableSeed.Fence.InstanceId,
		checkpointID, lifecycle.ActionCheckpoint, now.Add(10*time.Millisecond),
	)

	var disposableCheckpointID, durableCheckpointID string
	if err := pool.QueryRow(t.Context(), `
		SELECT current_checkpoint_id FROM secondbox.workspaces WHERE id=$1`,
		disposable.Workspace.ID,
	).Scan(&disposableCheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT current_checkpoint_id FROM secondbox.workspaces WHERE id=$1`,
		durable.Workspace.ID,
	).Scan(&durableCheckpointID); err != nil {
		t.Fatal(err)
	}
	if disposableCheckpointID != "" || durableCheckpointID != checkpointID {
		t.Fatalf(
			"stopped Workspace roots: disposable=%q durable=%q",
			disposableCheckpointID, durableCheckpointID,
		)
	}

	capture := &profileLifecycleSchedulerCapture{pool: pool}
	idSequence := 0
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, capture,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              1,
			SerializationRetryLimit: 1,
			AssetCatalog:            profileLifecycleAssetCatalog{},
			SessionCanceller:        profileLifecycleSessionCanceller{},
			NewID: func(prefix string) string {
				idSequence++
				return fmt.Sprintf("%s-profile-policy-%d", prefix, idSequence)
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

	disposableRequest := restartSandboxThroughPinnedProfile(
		t, controlPlane, databaseStore, pool, effectBroker, capture,
		principal, disposable.ID, durable.ID, now.Add(time.Second),
	)
	durableRequest := restartSandboxThroughPinnedProfile(
		t, controlPlane, databaseStore, pool, effectBroker, capture,
		principal, durable.ID, disposable.ID, now.Add(2*time.Second),
	)
	if disposableRequest.ProfileRevisionID != disposable.ProfileRevisionID ||
		disposableRequest.SourceCheckpointID != "" ||
		disposableRequest.AssignmentCommand.GetSourceCheckpointId() != "" ||
		slices.Contains(disposableRequest.Requirements.RequiredCapabilities, "checkpoint") {
		t.Fatalf("disposable restart request = %#v", disposableRequest)
	}
	if durableRequest.ProfileRevisionID != durable.ProfileRevisionID ||
		durableRequest.SourceCheckpointID != checkpointID ||
		durableRequest.AssignmentCommand.GetSourceCheckpointId() != checkpointID ||
		!slices.Contains(durableRequest.Requirements.RequiredCapabilities, "checkpoint") {
		t.Fatalf("durable restart request = %#v", durableRequest)
	}
}

func TestOperatorExplicitlyDefinesEphemeralAndDurableProfilesWithoutDefaults(t *testing.T) {
	databaseName := "secondbox_operator_profiles_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	databaseURL := backupRestoreDatabaseURL(t, integrationDatabaseURL, databaseName)
	runBackupRestoreCommand(
		t,
		exec.Command("createdb", "--maintenance-db="+integrationDatabaseURL, databaseName),
		"create isolated operator Profile qualification database",
	)
	t.Cleanup(func() {
		command := exec.Command(
			"dropdb", "--if-exists", "--maintenance-db="+integrationDatabaseURL, databaseName,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("drop isolated operator Profile qualification database: %v\n%s", err, output)
		}
	})
	applyBackupRestoreMigration(t, databaseURL)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newControlPlaneService(t, databaseStore, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	project, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "operator-profile-qualification",
	)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var profilesBefore int64
	if err := pool.QueryRow(
		t.Context(), `SELECT count(*) FROM secondbox.profiles`,
	).Scan(&profilesBefore); err != nil {
		t.Fatal(err)
	}
	if profilesBefore != 0 {
		t.Fatalf("fresh deployment contains %d hidden Profiles", profilesBefore)
	}
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"},
		Capabilities:  []string{"firecracker", "checkpoint"},
		CapacityPolicy: map[string]int64{
			"maxInstances": 100,
		},
		ReadyRunnerCount: 1, Revision: 1,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	ephemeralSpec := testProfileSpec(1000)
	ephemeralSpec.Lifecycle.IdleSeconds = 60
	ephemeralSpec.Lifecycle.MaximumDurationSeconds = 900
	ephemeralSpec.Checkpoint.OnStop = false
	durableSpec := testProfileSpec(4000)
	durableSpec.Lifecycle.IdleSeconds = 3600
	durableSpec.Lifecycle.MaximumDurationSeconds = 43_200
	durableSpec.Checkpoint.OnStop = true
	const (
		ephemeralName = "operator-ephemeral-agent-turn"
		durableName   = "operator-durable-coding-session"
	)
	ephemeralProfile, err := controlPlane.CreateProfile(
		t.Context(), admin,
		contracts.CreateProfileRequest{Name: ephemeralName, Spec: ephemeralSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	durableProfile, err := controlPlane.CreateProfile(
		t.Context(), admin,
		contracts.CreateProfileRequest{Name: durableName, Spec: durableSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	grants := []string{ephemeralName, durableName}
	if _, err := controlPlane.UpdateServiceAccount(
		t.Context(), admin, project.ID, account.ID,
		contracts.UpdateServiceAccountRequest{ProfileGrants: &grants},
	); err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	ephemeral, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "operator-ephemeral-create",
		contracts.CreateSandboxRequest{
			Profile: ephemeralName, Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	durable, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "operator-durable-create",
		contracts.CreateSandboxRequest{
			Profile: durableName, Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral.ProfileRevisionID != ephemeralProfile.CurrentRevision.ID ||
		durable.ProfileRevisionID != durableProfile.CurrentRevision.ID ||
		ephemeral.ProfileRevisionID == durable.ProfileRevisionID ||
		ephemeralProfile.CurrentRevision.Spec.Checkpoint.OnStop ||
		!durableProfile.CurrentRevision.Spec.Checkpoint.OnStop ||
		ephemeralProfile.CurrentRevision.Spec.Lifecycle.MaximumDurationSeconds != 900 ||
		durableProfile.CurrentRevision.Spec.Lifecycle.MaximumDurationSeconds != 43_200 {
		t.Fatalf(
			"operator Profile authority ephemeral=%#v durable=%#v Sandboxes=%#v/%#v",
			ephemeralProfile, durableProfile, ephemeral, durable,
		)
	}
	var profilesAfter int64
	if err := pool.QueryRow(
		t.Context(), `SELECT count(*) FROM secondbox.profiles`,
	).Scan(&profilesAfter); err != nil {
		t.Fatal(err)
	}
	if profilesAfter != 2 {
		t.Fatalf("operator created 2 Profiles but deployment contains %d", profilesAfter)
	}
}

func stopSandboxForProfilePolicyTest(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	principal contracts.Principal,
	sandbox contracts.Sandbox,
	suffix string,
) {
	t.Helper()
	current, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "profile-policy-stop-"+suffix, current.Revision,
	); err != nil {
		t.Fatal(err)
	}
}

func driveProfilePolicyStop(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
	pool *pgxpool.Pool,
	sandbox contracts.Sandbox,
	instanceID string,
	checkpointID string,
	expectedSecondAction lifecycle.Action,
	now time.Time,
) {
	t.Helper()
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
		Store: databaseStore,
		Effects: &integrationLifecycleEffects{
			store:        databaseStore,
			pool:         pool,
			checkpointID: checkpointID,
		},
		WorkerID:      "profile-policy-stop-" + sandbox.ID,
		ClaimDuration: time.Minute,
		PollInterval:  time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("profile-policy drain = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(time.Millisecond),
	); err != nil || !found || decision.Action != expectedSecondAction {
		t.Fatalf("profile-policy stop decision = %#v, %t, %v", decision, found, err)
	}
	next := now.Add(2 * time.Millisecond)
	if expectedSecondAction == lifecycle.ActionCheckpoint {
		checkpoint := contracts.WorkspaceCheckpoint{
			ID: checkpointID, WorkspaceID: sandbox.Workspace.ID,
			SourceGeneration: sandbox.Generation,
			SHA256:           "abababababababababababababababababababababababababababababababab",
			SizeBytes:        4096,
			Compatibility:    map[string]string{"architecture": "amd64"},
			RetainUntil:      now.Add(time.Hour),
			CreatedAt:        now,
		}
		publication := ports.CheckpointPublicationInput{
			Checkpoint: checkpoint, StorageKey: "checkpoints/" + checkpoint.ID,
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
		if decision, found, err := reconciler.RunOnce(t.Context(), next); err != nil ||
			!found || decision.Action != lifecycle.ActionStopInstance {
			t.Fatalf("profile-policy durable stop = %#v, %t, %v", decision, found, err)
		}
		next = next.Add(time.Millisecond)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='stopped',termination_reason='requested_stop',
		    stopped_at=$2,updated_at=$2 WHERE id=$1`,
		instanceID, next,
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), next); err != nil ||
		!found || decision.Action != lifecycle.ActionFinishStop {
		t.Fatalf("profile-policy finish stop = %#v, %t, %v", decision, found, err)
	}
}

func restartSandboxThroughPinnedProfile(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	databaseStore *store.PostgresControlPlaneStore,
	pool *pgxpool.Pool,
	effectBroker *lifecycle.PostgresEffectBroker,
	capture *profileLifecycleSchedulerCapture,
	principal contracts.Principal,
	sandboxID string,
	otherSandboxID string,
	now time.Time,
) scheduler.ScheduleRequest {
	t.Helper()
	current, err := controlPlane.GetSandbox(t.Context(), principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, sandboxID, "profile-policy-restart-"+sandboxID, current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
			SET next_reconcile_at=CASE
				WHEN id=$1 THEN $2::timestamptz
				ELSE $3::timestamptz
			END
			WHERE id IN ($1,$4)`,
		sandboxID, now, now.Add(time.Hour), otherSandboxID,
	); err != nil {
		t.Fatal(err)
	}
	before := len(capture.requests)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID:      "profile-policy-restart-" + sandboxID,
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionMaterialize {
		t.Fatalf("profile-policy restart = %#v, %t, %v", decision, found, err)
	}
	if len(capture.requests) != before+1 {
		t.Fatalf("profile-policy scheduler requests = %d, want %d", len(capture.requests), before+1)
	}
	return capture.requests[before]
}

type profileLifecycleSchedulerCapture struct {
	pool     *pgxpool.Pool
	requests []scheduler.ScheduleRequest
}

func (capture *profileLifecycleSchedulerCapture) Schedule(
	ctx context.Context,
	request scheduler.ScheduleRequest,
) (scheduler.DurableAssignment, bool, error) {
	capture.requests = append(capture.requests, request)
	if _, err := capture.pool.Exec(ctx, `
		UPDATE secondbox.sandboxes SET current_instance_id=$2 WHERE id=$1`,
		request.SandboxID, request.InstanceID,
	); err != nil {
		return scheduler.DurableAssignment{}, false, err
	}
	return scheduler.DurableAssignment{
		ID: request.AssignmentID, SandboxID: request.SandboxID,
		InstanceID: request.InstanceID, ProfileRevisionID: request.ProfileRevisionID,
		Generation: int64(request.AssignmentCommand.GetFence().GetSandboxGeneration()),
	}, true, nil
}

type profileLifecycleAssetCatalog struct{}

func (profileLifecycleAssetCatalog) Resolve(digest string) (lifecycle.SignedAsset, error) {
	return lifecycle.SignedAsset{
		ArtifactID: "profile-policy-asset", ManifestDigest: digest,
		SignatureKeyID: "profile-policy-key", Architecture: "amd64",
		GuestProtocolGeneration: 1,
	}, nil
}

type profileLifecycleSessionCanceller struct{}

func (profileLifecycleSessionCanceller) CancelSandboxSessions(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (int64, error) {
	return 0, nil
}

var _ lifecycle.AssignmentScheduler = (*profileLifecycleSchedulerCapture)(nil)
var _ lifecycle.SignedAssetCatalog = profileLifecycleAssetCatalog{}
var _ lifecycle.ActiveSessionCanceller = profileLifecycleSessionCanceller{}
