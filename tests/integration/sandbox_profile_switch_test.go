package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

const offlineAttributedGateway = "agent-gateway-offline.secondbox.internal"

// profileSwitchFixture is one Tenant and Subject whose dedicated home Runner
// supports attributed execution in the Tenant's pinned egress context.
type profileSwitchFixture struct {
	controlPlane  *service.ControlPlaneService
	databaseStore *store.PostgresControlPlaneStore
	pool          *pgxpool.Pool
	admin         contracts.Principal
	principal     contracts.Principal
	project       fixtureProject
	account       fixtureServiceAccount
	poolName      string
	runnerID      string
	egressContext string
	now           time.Time
}

func newProfileSwitchFixture(t *testing.T, suffix string) profileSwitchFixture {
	t.Helper()
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, suffix)
	fixture := profileSwitchFixture{
		controlPlane: controlPlane, databaseStore: databaseStore, admin: admin,
		principal: authenticateCredential(t, controlPlane, credential),
		project:   project, account: account,
		poolName:      "switch-pool-" + suffix,
		runnerID:      "runner-switch-" + suffix,
		egressContext: "switch-context-" + suffix,
		now:           time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fixture.pool = pool
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: fixture.poolName, State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"},
		ReadyRunnerCount: 1, Revision: 1, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}); err != nil {
		t.Fatal(err)
	}
	seedFixtureHomeRunner(t, fixture.poolName, fixture.runnerID)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET capabilities_json=capabilities_json || to_jsonb($2::text),
		    supported_egress_contexts_json=to_jsonb(ARRAY[$3::text])
		WHERE id=$1`,
		fixture.runnerID, contracts.RunnerCapabilityPerExecAttribution, fixture.egressContext,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlPlane.UpdateTenantEgressContext(
		t.Context(), admin, project.ID, "switch-context-"+suffix, 1,
		contracts.UpdateTenantEgressContextRequest{EgressContext: &fixture.egressContext},
	); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// onlineWorkspaceSpec is an allow-list Profile that reaches its ordinary
// gateway and also permits attributed execution.
func (fixture profileSwitchFixture) onlineWorkspaceSpec() contracts.ProfileRevisionSpec {
	spec := testProfileSpec(1)
	spec.Pool = fixture.poolName
	requiresContext := true
	spec.Network = contracts.NetworkPolicy{
		Mode: "allow_list",
		Destinations: []contracts.NetworkDestination{{
			Protocol: "tcp", Domain: "agent-gateway.secondbox.internal", Port: 3128,
		}},
		RequiresTenantEgressContext: &requiresContext,
	}
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{
		Gateway: "agent-gateway.secondbox.internal", MaximumConnections: 32,
	}
	return spec
}

// offlineWorkspaceSpec has no ordinary destination: only attributed execs reach
// the gateway, through its own logical name.
func (fixture profileSwitchFixture) offlineWorkspaceSpec() contracts.ProfileRevisionSpec {
	spec := fixture.onlineWorkspaceSpec()
	spec.Network.Mode = "deny_all"
	spec.Network.Destinations = []contracts.NetworkDestination{}
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{
		Gateway: offlineAttributedGateway, MaximumConnections: 32,
	}
	return spec
}

func (fixture profileSwitchFixture) createProfile(
	t *testing.T,
	name string,
	spec contracts.ProfileRevisionSpec,
) contracts.Profile {
	t.Helper()
	profile, _, err := fixture.controlPlane.CreateProfileIdempotent(
		t.Context(), fixture.admin, "create-"+name, contracts.CreateProfileRequest{Name: name, Spec: spec},
	)
	if err != nil {
		t.Fatalf("create Profile %s: %v", name, err)
	}
	return profile
}

// createReadySandbox creates a Sandbox of the Profile and completes its home
// Workspace, leaving it stopped.
func (fixture profileSwitchFixture) createReadySandbox(
	t *testing.T,
	profile string,
	key string,
) contracts.Sandbox {
	t.Helper()
	sandbox, _, err := fixture.controlPlane.CreateSandbox(
		t.Context(), fixture.principal, key,
		contracts.CreateSandboxRequest{Profile: profile, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatalf("create Sandbox of %s: %v", profile, err)
	}
	completeFixtureSandboxCreation(t, sandbox.ID)
	sandbox, err = fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != contracts.SandboxStateStopped || sandbox.Workspace.State != "ready" {
		t.Fatalf("created Sandbox = state %q Workspace %q", sandbox.State, sandbox.Workspace.State)
	}
	return sandbox
}

// startAssignment requests a start and runs the lifecycle reconciler through
// the real scheduler, returning the Assignment committed for the home Runner.
func (fixture profileSwitchFixture) startAssignment(
	t *testing.T,
	sandbox contracts.Sandbox,
	key string,
	at time.Time,
) *runnerv1.AssignmentCommand {
	t.Helper()
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandbox.ID, key, sandbox.Revision, contracts.ExecutionImage{},
	); err != nil {
		t.Fatalf("start Sandbox: %v", err)
	}
	assignmentScheduler, err := scheduler.NewPostgresStore(t.Context(), scheduler.PostgresStoreConfig{
		DatabaseURL: integrationDatabaseURL, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assignmentScheduler.Close)
	sequence := 0
	broker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, assignmentScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 8, SerializationRetryLimit: 3,
			SessionCanceller: multirunnerSessionCanceller{},
			NewID: func(prefix string) string {
				sequence++
				return fmt.Sprintf("%s-%s-%d", prefix, key, sequence)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: func() time.Time { return at },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	reconciler := lifecycle.Reconciler{
		Store: fixture.databaseStore, Effects: broker, WorkerID: "switch-worker-" + key,
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	multirunnerRunLifecycle(t, fixture.pool, reconciler, sandbox.ID, at, lifecycle.ActionStartInstance)
	command := multirunnerAssertHomeCommand(
		t, fixture.pool, sandbox.ID, fixture.runnerID, "runner-not-home", "assignment",
	)
	assignment := command.GetAssignment()
	if assignment == nil {
		t.Fatalf("start command = %#v", command)
	}
	return assignment
}

// TestOfflineWorkspaceProfileShapeStartsWithoutOrdinaryGateway proves that a
// deny_all Profile whose only gateway path is attributed execution is accepted
// by Profile validation, admits a Sandbox, and starts with no ordinary
// destination in its Assignment.
func TestOfflineWorkspaceProfileShapeStartsWithoutOrdinaryGateway(t *testing.T) {
	fixture := newProfileSwitchFixture(t, "offline-shape")
	offlineSpec := fixture.offlineWorkspaceSpec()
	// Create from an online revision, then revise to the offline shape, so both
	// validation paths accept it.
	profile := fixture.createProfile(t, "profile-offline-shape", fixture.onlineWorkspaceSpec())
	profile, _, err := fixture.controlPlane.ReviseProfileAtRevisionIdempotent(
		t.Context(), fixture.admin, profile.Name, "revise-offline-shape",
		contracts.ReviseProfileRequest{Spec: offlineSpec}, profile.Revision,
	)
	if err != nil {
		t.Fatalf("revise Profile to the offline shape: %v", err)
	}
	created := fixture.createProfile(t, "profile-offline-shape-created", offlineSpec)
	if created.CurrentRevision.Spec.Network.Mode != "deny_all" ||
		created.CurrentRevision.Spec.AttributedExecution.Gateway != offlineAttributedGateway {
		t.Fatalf("created offline Profile = %#v", created.CurrentRevision.Spec)
	}

	sandbox := fixture.createReadySandbox(t, profile.Name, "offline-shape-create")
	if sandbox.ProfileRevisionID != profile.CurrentRevision.ID ||
		sandbox.EgressContext == nil || *sandbox.EgressContext != fixture.egressContext {
		t.Fatalf("offline Sandbox revision=%q context=%v", sandbox.ProfileRevisionID, sandbox.EgressContext)
	}
	assignment := fixture.startAssignment(t, sandbox, "offline-shape-start", fixture.now.Add(10*time.Second))
	if assignment.ProfileRevisionId != profile.CurrentRevision.ID ||
		assignment.NetworkPolicy.GetMode() != runnerv1.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL ||
		len(assignment.NetworkPolicy.GetDestinations()) != 0 ||
		assignment.EgressContext != fixture.egressContext ||
		!assignment.Requirements.RequiresTenantEgressContext ||
		!slices.Contains(assignment.Requirements.RequiredCapabilities, contracts.RunnerCapabilityPerExecAttribution) ||
		assignment.AttributedExecutionPermission.GetGateway() != offlineAttributedGateway ||
		assignment.AttributedExecutionPermission.GetMaximumConnections() != 32 {
		t.Fatalf("offline Assignment = %#v", assignment)
	}
}

func (fixture profileSwitchFixture) switchProfile(
	ctx context.Context,
	sandboxID string,
	key string,
	revision int64,
	profile string,
) (contracts.Sandbox, bool, error) {
	return fixture.controlPlane.SwitchSandboxProfile(
		ctx, fixture.principal, sandboxID, key, revision,
		contracts.SwitchSandboxProfileRequest{Profile: profile},
	)
}

// TestSandboxProfileSwitchRepinsStoppedSandboxForItsNextStart moves a stopped
// Sandbox between an online and an offline Profile in both directions and
// proves that the next start is built from the target revision alone.
func TestSandboxProfileSwitchRepinsStoppedSandboxForItsNextStart(t *testing.T) {
	fixture := newProfileSwitchFixture(t, "switch-repin")
	online := fixture.createProfile(t, "profile-switch-repin-online", fixture.onlineWorkspaceSpec())
	offlineSpec := fixture.offlineWorkspaceSpec()
	offlineSpec.Lifecycle.IdleSeconds = 120
	offline := fixture.createProfile(t, "profile-switch-repin-offline", offlineSpec)

	sandbox := fixture.createReadySandbox(t, online.Name, "switch-repin-create")
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-repin-stale", sandbox.Revision+1, offline.Name,
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale If-Match switch error = %v", err)
	}
	switched, replayed, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-repin-offline", sandbox.Revision, offline.Name,
	)
	if err != nil || replayed {
		t.Fatalf("switch to offline = replayed %t error %v", replayed, err)
	}
	if switched.ID != sandbox.ID || switched.Profile != offline.Name ||
		switched.ProfileRevisionID != offline.CurrentRevision.ID ||
		switched.Revision != sandbox.Revision+1 ||
		switched.Generation != sandbox.Generation ||
		switched.Workspace.ID != sandbox.Workspace.ID ||
		switched.State != contracts.SandboxStateStopped ||
		switched.Resources != sandbox.Resources ||
		!equalOptionalString(switched.EgressContext, sandbox.EgressContext) ||
		switched.Lifecycle.IdleSeconds != 120 {
		t.Fatalf("switched Sandbox = %#v, was %#v", switched, sandbox)
	}

	replay, replayed, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-repin-offline", sandbox.Revision, offline.Name,
	)
	if err != nil || !replayed || replay.Revision != switched.Revision {
		t.Fatalf("switch replay = revision %d replayed %t error %v", replay.Revision, replayed, err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-repin-offline", sandbox.Revision, online.Name,
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("reused key with another target error = %v", err)
	}
	converged, replayed, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-repin-converged", switched.Revision, offline.Name,
	)
	if err != nil || replayed || converged.Revision != switched.Revision ||
		converged.ProfileRevisionID != offline.CurrentRevision.ID {
		t.Fatalf("converged switch = revision %d replayed %t error %v", converged.Revision, replayed, err)
	}
	events, err := fixture.databaseStore.ListAuditEvents(t.Context(), fixture.project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	switchEvents := 0
	for _, event := range events {
		if event.Action != "sandbox.profile.switched" || event.ResourceID != sandbox.ID {
			continue
		}
		switchEvents++
		if event.Details["fromProfile"] != online.Name ||
			event.Details["fromProfileRevisionId"] != online.CurrentRevision.ID ||
			event.Details["toProfile"] != offline.Name ||
			event.Details["toProfileRevisionId"] != offline.CurrentRevision.ID {
			t.Fatalf("switch audit = %#v", event)
		}
	}
	if switchEvents != 1 {
		t.Fatalf("switch audit events = %d, want 1", switchEvents)
	}

	offlineAssignment := fixture.startAssignment(t, converged, "switch-repin-offline-start", fixture.now.Add(10*time.Second))
	if offlineAssignment.ProfileRevisionId != offline.CurrentRevision.ID ||
		offlineAssignment.NetworkPolicy.GetMode() != runnerv1.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL ||
		len(offlineAssignment.NetworkPolicy.GetDestinations()) != 0 ||
		offlineAssignment.AttributedExecutionPermission.GetGateway() != offlineAttributedGateway ||
		offlineAssignment.EgressContext != fixture.egressContext {
		t.Fatalf("Assignment after switch to offline = %#v", offlineAssignment)
	}

	reverse := fixture.createReadySandbox(t, offline.Name, "switch-repin-reverse-create")
	reverse, _, err = fixture.switchProfile(
		t.Context(), reverse.ID, "switch-repin-online", reverse.Revision, online.Name,
	)
	if err != nil || reverse.ProfileRevisionID != online.CurrentRevision.ID || reverse.Lifecycle.IdleSeconds != 300 {
		t.Fatalf("switch to online = %#v error %v", reverse, err)
	}
	onlineAssignment := fixture.startAssignment(t, reverse, "switch-repin-online-start", fixture.now.Add(20*time.Second))
	if onlineAssignment.ProfileRevisionId != online.CurrentRevision.ID ||
		onlineAssignment.NetworkPolicy.GetMode() != runnerv1.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST ||
		len(onlineAssignment.NetworkPolicy.GetDestinations()) != 1 ||
		onlineAssignment.NetworkPolicy.GetDestinations()[0].GetDomain() != "agent-gateway.secondbox.internal" ||
		onlineAssignment.AttributedExecutionPermission.GetGateway() != "agent-gateway.secondbox.internal" {
		t.Fatalf("Assignment after switch to online = %#v", onlineAssignment)
	}
}

// TestSandboxProfileSwitchKeepsSubjectPolicyOfNamedProfiles proves one Subject
// policy naming both Profiles selects the same lifecycle at creation and after
// a switch, and the same connection limit for the switched Assignment, while a
// Profile outside the set keeps its own defaults.
func TestSandboxProfileSwitchKeepsSubjectPolicyOfNamedProfiles(t *testing.T) {
	fixture := newProfileSwitchFixture(t, "switch-policy")
	ceilings := func(spec contracts.ProfileRevisionSpec) contracts.ProfileRevisionSpec {
		spec.Lifecycle.MaximumDurationSeconds = contracts.Unlimited
		spec.LifecycleCeiling = &contracts.SandboxLifecycleLimits{IdleSeconds: 900, MaximumDurationSeconds: contracts.Unlimited}
		spec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{MaximumConnections: 256}
		return spec
	}
	online := fixture.createProfile(t, "profile-switch-policy-online", ceilings(fixture.onlineWorkspaceSpec()))
	offline := fixture.createProfile(t, "profile-switch-policy-offline", ceilings(fixture.offlineWorkspaceSpec()))
	unnamedSpec := ceilings(fixture.offlineWorkspaceSpec())
	unnamedSpec.Lifecycle.IdleSeconds = 120
	unnamed := fixture.createProfile(t, "profile-switch-policy-unnamed", unnamedSpec)
	policy := `{"profiles":["profile-switch-policy-online","profile-switch-policy-offline"],"lifecycle":{"idleSeconds":450,"maximumDurationSeconds":null},"attributedExecution":{"maximumConnections":64}}`
	tag, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET sandbox_policy_json=$3
		WHERE tenant_ref=$1 AND ref=$2`,
		fixture.principal.TenantRef, fixture.principal.SubjectRef, policy,
	)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("seed Subject policy = %d rows, %v", tag.RowsAffected(), err)
	}

	sandbox := fixture.createReadySandbox(t, online.Name, "switch-policy-create")
	if sandbox.Lifecycle.IdleSeconds != 450 {
		t.Fatalf("created lifecycle = %#v, want selected idle 450", sandbox.Lifecycle)
	}
	switched, _, err := fixture.switchProfile(t.Context(), sandbox.ID, "switch-policy-offline", sandbox.Revision, offline.Name)
	if err != nil || switched.Lifecycle.IdleSeconds != 450 {
		t.Fatalf("switched lifecycle = %#v, error %v", switched.Lifecycle, err)
	}
	assignment := fixture.startAssignment(t, switched, "switch-policy-offline-start", fixture.now.Add(10*time.Second))
	if assignment.ProfileRevisionId != offline.CurrentRevision.ID ||
		assignment.AttributedExecutionPermission.GetMaximumConnections() != 64 {
		t.Fatalf("Assignment after switch = %#v", assignment)
	}

	member := fixture.createReadySandbox(t, offline.Name, "switch-policy-member-create")
	if member.Lifecycle.IdleSeconds != 450 {
		t.Fatalf("second member lifecycle = %#v, want selected idle 450", member.Lifecycle)
	}
	other := fixture.createReadySandbox(t, online.Name, "switch-policy-unnamed-create")
	other, _, err = fixture.switchProfile(t.Context(), other.ID, "switch-policy-unnamed", other.Revision, unnamed.Name)
	if err != nil || other.Lifecycle.IdleSeconds != 120 {
		t.Fatalf("unnamed target lifecycle = %#v, error %v", other.Lifecycle, err)
	}
}

// TestSandboxProfileSwitchRefusesUnsafeTargets leaves the Sandbox untouched for
// every refused precondition.
func TestSandboxProfileSwitchRefusesUnsafeTargets(t *testing.T) {
	fixture := newProfileSwitchFixture(t, "switch-refuse")
	online := fixture.createProfile(t, "profile-switch-refuse-online", fixture.onlineWorkspaceSpec())
	offline := fixture.createProfile(t, "profile-switch-refuse-offline", fixture.offlineWorkspaceSpec())
	sandbox := fixture.createReadySandbox(t, online.Name, "switch-refuse-create")

	otherPool := fixture.offlineWorkspaceSpec()
	otherPool.Pool = "switch-refuse-other-pool"
	resumed := fixture.offlineWorkspaceSpec()
	resumed.Startup.Mode = contracts.StartupModeSnapshotResume
	isolated := testProfileSpec(1)
	isolated.Pool = fixture.poolName
	smaller := fixture.offlineWorkspaceSpec()
	smaller.Resources.MemoryBytes = 512 << 20
	for _, refusal := range []struct {
		name     string
		spec     contracts.ProfileRevisionSpec
		property string
	}{
		{"profile-switch-refuse-pool", otherPool, "pool"},
		{"profile-switch-refuse-startup", resumed, "startup.mode"},
		{"profile-switch-refuse-isolated", isolated, "network.requiresTenantEgressContext"},
		{"profile-switch-refuse-smaller", smaller, "resources"},
	} {
		target := fixture.createProfile(t, refusal.name, refusal.spec)
		_, _, err := fixture.switchProfile(
			t.Context(), sandbox.ID, "switch-refuse-"+refusal.property, sandbox.Revision, target.Name,
		)
		var incompatible *ports.ProfileIncompatibleError
		if !errors.As(err, &incompatible) || incompatible.Property != refusal.property {
			t.Fatalf("switch to %s error = %v, want incompatible %s", refusal.name, err, refusal.property)
		}
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET capabilities_json=capabilities_json - $2::text WHERE id=$1`,
		fixture.runnerID, contracts.RunnerCapabilityPerExecAttribution,
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := fixture.switchProfile(t.Context(), sandbox.ID, "switch-refuse-home", sandbox.Revision, offline.Name)
	var incompatible *ports.ProfileIncompatibleError
	if !errors.As(err, &incompatible) || incompatible.Property != "homeRunner" {
		t.Fatalf("switch onto a home Runner without attribution error = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET capabilities_json=capabilities_json || to_jsonb($2::text) WHERE id=$1`,
		fixture.runnerID, contracts.RunnerCapabilityPerExecAttribution,
	); err != nil {
		t.Fatal(err)
	}

	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-refuse-missing", sandbox.Revision, "profile-switch-refuse-missing",
	); !errors.Is(err, ports.ErrProfileNotFound) {
		t.Fatalf("switch to a missing Profile error = %v", err)
	}
	disabled := fixture.createProfile(t, "profile-switch-refuse-disabled", fixture.offlineWorkspaceSpec())
	if _, _, err := fixture.controlPlane.DisableProfileAtRevisionIdempotent(
		t.Context(), fixture.admin, disabled.Name, "disable-switch-refuse", disabled.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-refuse-disabled", sandbox.Revision, disabled.Name,
	); !errors.Is(err, ports.ErrProfileDisabled) {
		t.Fatalf("switch to a disabled Profile error = %v", err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-refuse-invalid", sandbox.Revision, "Not A Profile",
	); !errors.Is(err, ports.ErrInvalidRequest) {
		t.Fatalf("switch to an invalid Profile name error = %v", err)
	}

	for _, grants := range [][]string{{}, {online.Name}, {offline.Name}} {
		ctx := service.ContextWithApplicationProfileGrants(t.Context(), grants)
		if _, _, err := fixture.switchProfile(
			ctx, sandbox.ID, fmt.Sprintf("switch-refuse-grants-%d", len(grants)), sandbox.Revision, offline.Name,
		); !errors.Is(err, ports.ErrAuthorizationDenied) {
			t.Fatalf("switch with grants %v error = %v", grants, err)
		}
	}

	snapshotOperation, _, err := fixture.controlPlane.CreateSandboxSnapshot(
		t.Context(), fixture.principal, sandbox.ID, "switch-refuse-snapshot", sandbox.Revision,
		contracts.CreateSnapshotRequest{Name: "keep", Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-refuse-snapshot-mutation", current.Revision, offline.Name,
	); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("switch during a Snapshot mutation error = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.snapshots
		SET state='ready',runner_receipt_json='{"durable":true}' WHERE id=$1`,
		snapshotOperation.Snapshot.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state=''
		WHERE sandbox_id=$1`,
		sandbox.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-refuse-snapshot", current.Revision, offline.Name,
	); !errors.Is(err, ports.ErrProfileSwitchSnapshotsPresent) {
		t.Fatalf("switch with a retained Snapshot error = %v", err)
	}

	starting := fixture.createReadySandbox(t, online.Name, "switch-refuse-starting-create")
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, starting.ID, "switch-refuse-start", starting.Revision, contracts.ExecutionImage{},
	); err != nil {
		t.Fatal(err)
	}
	starting, err = fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, starting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.switchProfile(
		t.Context(), starting.ID, "switch-refuse-running", starting.Revision, offline.Name,
	); !errors.Is(err, ports.ErrProfileSwitchSandboxNotStopped) {
		t.Fatalf("switch of a Sandbox wanted running error = %v", err)
	}

	unchanged, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ProfileRevisionID != online.CurrentRevision.ID || unchanged.Revision != current.Revision {
		t.Fatalf("refused switches changed the Sandbox: %#v", unchanged)
	}
}

// TestSandboxProfileSwitchHTTPContract drives the route as an application
// authority through the Go SDK, validating every response against the OpenAPI
// document: both Profiles must be granted, and refusals carry typed codes.
func TestSandboxProfileSwitchHTTPContract(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	admin := fixtureAdmin(t, controlPlane)
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"},
		ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	sequence := integrationIdentitySequence.Add(1)
	seedFixtureHomeRunner(t, "default-pool", fmt.Sprintf("runner-switch-http-%d", sequence))
	online := fmt.Sprintf("switch-http-online-%d", sequence)
	offline := fmt.Sprintf("switch-http-offline-%d", sequence)
	incompatible := fmt.Sprintf("switch-http-incompatible-%d", sequence)
	onlineSpec := testProfileSpec(1)
	onlineSpec.Network.Mode = "allow_list"
	onlineSpec.Network.Destinations = []contracts.NetworkDestination{{Protocol: "https", Domain: "public.example", Port: 443}}
	incompatibleSpec := testProfileSpec(1)
	incompatibleSpec.Startup.Mode = contracts.StartupModeSnapshotResume
	for name, spec := range map[string]contracts.ProfileRevisionSpec{
		online: onlineSpec, offline: testProfileSpec(1), incompatible: incompatibleSpec,
	} {
		if _, _, err := controlPlane.CreateProfileIdempotent(
			t.Context(), admin, "create-"+name, contracts.CreateProfileRequest{Name: name, Spec: spec},
		); err != nil {
			t.Fatal(err)
		}
	}
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	tenantRef := secondboxclient.OwnershipRef(fmt.Sprintf("switch-http-tenant-%d", sequence))
	tenant := persistedHTTPTenantRequest(tenantRef)
	tenant.AllowedProfileGrants = []string{online, offline, incompatible}
	if _, err := operator.CreateTenant(t.Context(), tenant, "switch-http-tenant"); err != nil {
		t.Fatal(err)
	}
	controllerCredential, err := operator.CreateTenantControllerAuthority(t.Context(), tenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: now.Add(45 * time.Minute), Metadata: map[string]string{},
	}, "switch-http-controller")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controllerCredential.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{
		Ref: "switch-subject", Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 10, MaxMemoryBytes: 10 << 30,
			MaxSnapshots: 10, MaxPortSessions: 10, MaxConcurrentOperations: 10,
		}, Metadata: map[string]string{},
	}, "switch-http-subject"); err != nil {
		t.Fatal(err)
	}
	application := func(key string, grants ...string) *secondboxclient.Client {
		t.Helper()
		credential, err := controller.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
			SubjectRef: "switch-subject", Scopes: []string{"sandbox:read", "sandbox:lifecycle"},
			ProfileGrants: grants, Metadata: map[string]string{}, ExpiresAt: now.Add(30 * time.Minute),
		}, "switch-http-application-"+key)
		if err != nil {
			t.Fatal(err)
		}
		client, err := secondboxclient.NewSecondBoxSubjectClient(
			server.URL, credential.BearerToken, string(tenantRef), "switch-subject", server.Client(),
		)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	both := application("both", online, offline, incompatible)
	onlineOnly := application("online", online)
	offlineOnly := application("offline", offline)

	created, _, err := both.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{
		Profile: online, Metadata: map[string]string{},
	}, "switch-http-create")
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, created.Snapshot().ID)
	handle := func(client *secondboxclient.Client) *secondboxclient.SandboxHandle {
		t.Helper()
		handle := secondboxclient.NewSandboxHandle(client, created.Snapshot())
		if _, err := handle.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return handle
	}
	problemCode := func(err error) (int, string, []contracts.ProblemDetail) {
		t.Helper()
		var failure *secondboxclient.APIError
		if !errors.As(err, &failure) || failure.Problem == nil {
			t.Fatalf("switch error = %v, want a problem response", err)
		}
		return failure.StatusCode, string(failure.Problem.Code), failure.Problem.Details
	}
	request := secondboxclient.SwitchSandboxProfileRequest{Profile: offline}

	// The target grant is refused before the store; the left Profile's grant is
	// refused against the locked pin.
	if _, err := handle(onlineOnly).SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{}, request); err == nil {
		t.Fatal("switch without the target Profile grant succeeded")
	} else if status, code, _ := problemCode(err); status != http.StatusForbidden || code != "authorization_failed" {
		t.Fatalf("switch without the target grant = %d %s", status, code)
	}
	if _, err := handle(offlineOnly).SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{}, request); err == nil {
		t.Fatal("switch without the current Profile grant succeeded")
	} else if status, code, _ := problemCode(err); status != http.StatusForbidden || code != "authorization_failed" {
		t.Fatalf("switch without the current grant = %d %s", status, code)
	}
	switcher := handle(both)
	if _, err := switcher.SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{
		IfMatch: secondboxclient.RevisionETag(switcher.Snapshot().Revision + 1),
	}, request); err == nil {
		t.Fatal("switch with a stale If-Match succeeded")
	} else if status, code, _ := problemCode(err); status != http.StatusPreconditionFailed || code != "precondition_failed" {
		t.Fatalf("switch with a stale If-Match = %d %s", status, code)
	}
	if _, err := switcher.SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{},
		secondboxclient.SwitchSandboxProfileRequest{Profile: incompatible}); err == nil {
		t.Fatal("switch to an incompatible Profile succeeded")
	} else if status, code, details := problemCode(err); status != http.StatusConflict ||
		code != "profile_incompatible" || len(details) != 1 || details[0].Field != "startup.mode" {
		t.Fatalf("incompatible switch = %d %s %#v", status, code, details)
	}

	before := switcher.Snapshot()
	switched, err := switcher.SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{
		IdempotencyKey: "switch-http-offline",
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if switched.ID != before.ID || switched.Profile != offline || switched.Revision != before.Revision+1 ||
		switcher.Snapshot().Revision != switched.Revision {
		t.Fatalf("switched Sandbox = %#v, was %#v", switched, before)
	}

	// Raw replay exposes the replay header and the ETag.
	response := applicationSwitchRequest(t, both, switched.ID, before.Revision, "switch-http-offline", offline)
	if response.StatusCode != http.StatusOK || response.Header.Get("Idempotency-Replayed") != "true" ||
		response.Header.Get("ETag") != secondboxclient.RevisionETag(switched.Revision) {
		t.Fatalf("switch replay status=%d replayed=%q etag=%q",
			response.StatusCode, response.Header.Get("Idempotency-Replayed"), response.Header.Get("ETag"))
	}
	response.Body.Close()

	if _, err := controlPlane.StartSandbox(
		t.Context(), contracts.Principal{Kind: "platform", ID: "secondbox-admin", TenantRef: string(tenantRef), SubjectRef: "switch-subject"},
		switched.ID, "switch-http-start", switched.Revision, contracts.ExecutionImage{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := switcher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := switcher.SwitchProfile(t.Context(), secondboxclient.LifecycleOptions{},
		secondboxclient.SwitchSandboxProfileRequest{Profile: online}); err == nil {
		t.Fatal("switch of a Sandbox wanted running succeeded")
	} else if status, code, _ := problemCode(err); status != http.StatusConflict || code != "sandbox_not_stopped" {
		t.Fatalf("switch of a Sandbox wanted running = %d %s", status, code)
	}
}

// applicationSwitchRequest sends one raw switch so the test can read headers
// that the SDK does not surface.
func applicationSwitchRequest(
	t *testing.T,
	client *secondboxclient.Client,
	sandboxID string,
	revision int64,
	idempotencyKey string,
	profile string,
) *http.Response {
	t.Helper()
	response, err := client.Request(t.Context(), "switchSandboxProfile", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": sandboxID},
		Headers: http.Header{
			"Idempotency-Key": {idempotencyKey},
			"If-Match":        {secondboxclient.RevisionETag(revision)},
		},
		Body:        strings.NewReader(`{"profile":"` + profile + `"}`),
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}
