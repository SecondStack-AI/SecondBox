package integration_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
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
