package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresPortSessionAuthorityPolicyTokenAndAccounting(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "port-session")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "port-session-profile")
	scopes := []string{"sandbox:read", "sandbox:lifecycle", "sandbox:ports"}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "port-session", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "port-session-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seed := seedDataPlaneReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "port-session-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	dataPlaneStore, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL,
		Retention:   time.Hour, MaximumSessionBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dataPlaneStore.Close)
	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		Now: func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        dataPlaneStore, DataPlanePollInterval: time.Millisecond,
		PortSessionStore: dataPlaneStore, PublicBaseURL: "https://secondbox.example",
	})
	if err != nil {
		t.Fatal(err)
	}

	session, replayed, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-session", sandbox.ID, sandbox.Generation,
		lease.ID, "port-session-create", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 30,
		},
	)
	if err != nil || replayed {
		t.Fatalf("create port session = %#v replayed=%t error=%v", session, replayed, err)
	}
	if session.SandboxID != sandbox.ID || session.Generation != sandbox.Generation ||
		session.Name != "web" || session.Protocol != "tcp" || session.State != "open" ||
		!session.ExpiresAt.Equal(now.Add(30*time.Second)) ||
		!strings.HasPrefix(session.Endpoint, "wss://secondbox.example/v1/port-tunnels/") {
		t.Fatalf("port session = %#v", session)
	}
	replayedSession, replayed, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-session", sandbox.ID, sandbox.Generation,
		lease.ID, "port-session-create", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 30,
		},
	)
	if err != nil || !replayed || replayedSession.ID != session.ID ||
		replayedSession.Endpoint != session.Endpoint ||
		!replayedSession.CreatedAt.Equal(session.CreatedAt) ||
		!replayedSession.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("replay = %#v replayed=%t error=%v", replayedSession, replayed, err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-session", sandbox.ID, sandbox.Generation,
		lease.ID, "port-session-create", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 31,
		},
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-disabled", sandbox.ID, sandbox.Generation,
		lease.ID, "port-disabled", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{
			Name: "admin", DurationSeconds: 30,
		},
	); !errors.Is(err, ports.ErrPortPolicyDenied) {
		t.Fatalf("disabled port error = %v", err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-stale-generation", sandbox.ID, sandbox.Generation+1,
		lease.ID, "port-stale-generation", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 30,
		},
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale generation error = %v", err)
	}
	crossProject := principal
	crossProject.TenantRef = "project-outside-port-authority"
	crossProject.TenantRef = "tenant-outside-port-authority"
	if _, err := portService.GetSandboxPortSession(
		t.Context(), crossProject, sandbox.ID, session.ID, contracts.PortTransportProxied,
	); !errors.Is(err, ports.ErrPortSessionNotFound) {
		t.Fatalf("cross-Project PortSession lookup error = %v", err)
	}
	if err := portService.CloseSandboxPortSession(
		t.Context(), crossProject, sandbox.ID, session.ID, "cross-project-port-close",
	); !errors.Is(err, ports.ErrPortSessionNotFound) {
		t.Fatalf("cross-Project PortSession close error = %v", err)
	}
	parsedEndpoint, err := url.Parse(session.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portService.ConsumePortTunnelToken(
		t.Context(), "port-session-mismatch", parsedEndpoint.Fragment,
	); !errors.Is(err, ports.ErrPortTokenInvalid) {
		t.Fatalf("mismatched PortSession token error = %v", err)
	}
	payloadPart, signaturePart, found := strings.Cut(parsedEndpoint.Fragment, ".")
	if !found {
		t.Fatal("PortSession token is missing its signature")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["sub"] = "another-subject"
	alteredPayload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	anotherSubjectToken := base64.RawURLEncoding.EncodeToString(alteredPayload) + "." + signaturePart
	if _, err := portService.ConsumePortTunnelToken(
		t.Context(), session.ID, anotherSubjectToken,
	); !errors.Is(err, ports.ErrPortTokenInvalid) {
		t.Fatalf("cross-subject PortSession token error = %v", err)
	}

	tunnel, err := portService.ConsumePortTunnel(t.Context(), session.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.Session.ID != session.ID || tunnel.GuestPort != 8080 || tunnel.StreamWindowBytes != 65536 {
		t.Fatalf("tunnel = %#v", tunnel)
	}
	if _, err := portService.ConsumePortTunnel(t.Context(), session.Endpoint); !errors.Is(err, ports.ErrPortTokenConsumed) {
		t.Fatalf("token replay error = %v", err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var activeActivity int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.activity_sessions
		WHERE id=$1 AND kind='port' AND state='active'`, session.ID,
	).Scan(&activeActivity); err != nil {
		t.Fatal(err)
	}
	if activeActivity != 1 {
		t.Fatalf("active port activity rows = %d", activeActivity)
	}
	if err := portService.ClosePortTunnel(t.Context(), tunnel, "client disconnected"); err != nil {
		t.Fatal(err)
	}
	var closedActivity int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.activity_sessions
		WHERE id=$1 AND kind='port' AND state='closed'`, session.ID,
	).Scan(&closedActivity); err != nil {
		t.Fatal(err)
	}
	if closedActivity != 1 {
		t.Fatalf("closed port activity rows = %d", closedActivity)
	}

	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, lease.ID, "port-session-initial-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	leaseSweep, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "port-session-sweep-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseSwept, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-lease-sweep", sandbox.ID, sandbox.Generation,
		leaseSweep.ID, "port-lease-sweep", contracts.PortTransportProxied,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, leaseSweep.ID, "port-session-sweep-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	if changed, err := dataPlaneStore.SweepDataPlane(t.Context(), now.Add(time.Second), 100); err != nil || !changed {
		t.Fatalf("inactive Lease Port sweep = %t, %v", changed, err)
	}
	// The tunnel never connected, so no Runner holds state to cancel and the
	// session completes without waiting for a confirmation.
	var leaseSweptDataPlaneState, leaseSweptTerminal, leaseSweptDetail string
	var leaseSweptCommands int64
	if err := pool.QueryRow(t.Context(), `
		SELECT session.state,session.terminal_kind,session.terminal_detail,
		       (SELECT count(*) FROM secondbox.runner_commands WHERE id LIKE session.id||'%')
		FROM secondbox.data_plane_sessions AS session WHERE session.id=$1`,
		leaseSwept.ID,
	).Scan(&leaseSweptDataPlaneState, &leaseSweptTerminal, &leaseSweptDetail, &leaseSweptCommands); err != nil {
		t.Fatal(err)
	}
	if leaseSweptDataPlaneState != "completed" ||
		leaseSweptTerminal != runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String() ||
		leaseSweptDetail != "operation Lease is inactive" || leaseSweptCommands != 0 {
		t.Fatalf("unconsumed inactive-Lease Port = %q %q %q with %d Runner commands",
			leaseSweptDataPlaneState, leaseSweptTerminal, leaseSweptDetail, leaseSweptCommands)
	}
	leaseSweptState, err := portService.GetSandboxPortSession(
		t.Context(), principal, sandbox.ID, leaseSwept.ID, contracts.PortTransportProxied,
	)
	if err != nil || leaseSweptState.State != contracts.PortSessionStateClosed {
		t.Fatalf("inactive Lease Port state = %#v, %v", leaseSweptState, err)
	}

	lease, err = controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "port-session-final-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	expiring, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-expiry", sandbox.ID, sandbox.Generation,
		lease.ID, "port-expiry", contracts.PortTransportProxied, contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	if _, err := portService.ConsumePortTunnel(
		t.Context(), expiring.Endpoint,
	); !errors.Is(err, ports.ErrPortTokenInvalid) {
		t.Fatalf("expired Port token error = %v", err)
	}
	expired, err := portService.GetSandboxPortSession(
		t.Context(), principal, sandbox.ID, expiring.ID, contracts.PortTransportProxied,
	)
	if err != nil || expired.State != contracts.PortSessionStateExpired {
		t.Fatalf("expired PortSession = %#v, %v", expired, err)
	}
	now = now.Add(-11 * time.Second)

	disconnected, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-runner-disconnect", sandbox.ID, sandbox.Generation,
		lease.ID, "port-runner-disconnect", contracts.PortTransportProxied,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	disconnectedTunnel, err := portService.ConsumePortTunnel(t.Context(), disconnected.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	seedAdvertisedDataPlaneRunner(t, pool, seed.RunnerID, now)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET active_connection_id=$2 WHERE id=$1`,
		seed.RunnerID, seed.ConnectionOne,
	); err != nil {
		t.Fatal(err)
	}
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	if err := stateStore.CloseConnection(
		t.Context(), disconnectedTunnel.RunnerID, seed.ConnectionOne, now,
	); err != nil {
		t.Fatal(err)
	}
	var disconnectedState, disconnectedActivity string
	if err := pool.QueryRow(t.Context(), `
		SELECT port.state,activity.state
		FROM secondbox.port_sessions AS port
		JOIN secondbox.activity_sessions AS activity ON activity.id=port.id
		WHERE port.id=$1`, disconnected.ID,
	).Scan(&disconnectedState, &disconnectedActivity); err != nil {
		t.Fatal(err)
	}
	if disconnectedState != contracts.PortSessionStateClosed || disconnectedActivity != "closed" {
		t.Fatalf("runner disconnect state=%q activity=%q", disconnectedState, disconnectedActivity)
	}

	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, lease.ID, "port-stale-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-stale-lease", sandbox.ID, sandbox.Generation,
		lease.ID, "port-stale-lease", contracts.PortTransportProxied,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 10},
	); !errors.Is(err, ports.ErrLeaseInactive) {
		t.Fatalf("stale Lease PortSession error = %v", err)
	}
}

// fixtureControlPlaneNow is the fixed clock of newControlPlaneFixture, which
// issues the Leases a Port fixture admits sessions under.
var fixtureControlPlaneNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

type portSessionFixture struct {
	controlPlane   *service.ControlPlaneService
	portService    *service.ControlPlaneService
	dataPlaneStore *runnercontrol.PostgresDataPlaneStore
	pool           *pgxpool.Pool
	principal      contracts.Principal
	sandbox        contracts.Sandbox
	seed           dataPlaneReadySeed
	now            *time.Time
}

func newPortSessionFixture(t *testing.T, name string, start time.Time) portSessionFixture {
	t.Helper()
	now := start
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, name)
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, name+"-profile")
	scopes := []string{"sandbox:read", "sandbox:lifecycle", "sandbox:ports"}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: name, Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, name+"-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seed := seedDataPlaneReadyAssignment(t, sandbox, now)
	dataPlaneStore, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL,
		Retention:   time.Hour, MaximumSessionBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dataPlaneStore.Close)
	fixture := portSessionFixture{
		controlPlane: controlPlane, dataPlaneStore: dataPlaneStore,
		principal: principal, sandbox: sandbox, seed: seed, now: &now,
	}
	fixture.portService, err = service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		Now: func() time.Time { return *fixture.now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        dataPlaneStore, DataPlanePollInterval: time.Millisecond,
		PortSessionStore: dataPlaneStore, PublicBaseURL: "https://secondbox.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.pool, err = pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fixture.pool.Close)
	return fixture
}

// openConsumedPortTunnel admits and consumes one proxied session, as a public
// WebSocket connection does before any byte is relayed.
func (fixture portSessionFixture) openConsumedPortTunnel(
	t *testing.T,
	leaseID string,
	idempotencyKey string,
) runnercontrol.PortTunnel {
	t.Helper()
	session, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-"+idempotencyKey, fixture.sandbox.ID,
		fixture.sandbox.Generation, leaseID, idempotencyKey, contracts.PortTransportProxied,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := fixture.portService.ConsumePortTunnel(t.Context(), session.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel
}

// admittedOperations counts the Sandbox's sessions that hold operation
// admission, with the predicate data-plane admission uses.
func (fixture portSessionFixture) admittedOperations(t *testing.T) int64 {
	t.Helper()
	var count int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_sessions
		WHERE sandbox_id=$1 AND state IN ('pending','running','cancelling')`,
		fixture.sandbox.ID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (fixture portSessionFixture) dataPlaneState(t *testing.T, sessionID string) (string, string) {
	t.Helper()
	var state, terminalKind string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state,terminal_kind FROM secondbox.data_plane_sessions WHERE id=$1`, sessionID,
	).Scan(&state, &terminalKind); err != nil {
		t.Fatal(err)
	}
	return state, terminalKind
}

// A tunnel that closes while the Runner's read pump waits for credit it will
// never receive must still terminate and release its operation admission,
// whether or not the Runner ever confirms the cancellation.
func TestPostgresClosedPortSessionReleasesAdmissionWithoutRunnerConfirmation(t *testing.T) {
	fixture := newPortSessionFixture(t, "port-close-ack", fixtureControlPlaneNow)
	lease, err := fixture.controlPlane.AcquireSandboxLease(
		t.Context(), fixture.principal, fixture.sandbox.ID, fixture.sandbox.Generation, "port-close-ack-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := fixture.openConsumedPortTunnel(t, lease.ID, "port-close-ack")
	if got := fixture.admittedOperations(t); got != 1 {
		t.Fatalf("admitted operations after connect = %d", got)
	}
	if err := fixture.portService.ClosePortTunnel(t.Context(), tunnel, "public port tunnel disconnected"); err != nil {
		t.Fatal(err)
	}
	if state, _ := fixture.dataPlaneState(t, tunnel.Session.ID); state != "cancelling" {
		t.Fatalf("closed tunnel data-plane state = %q", state)
	}
	// Closing again, as the sweep or an application may, is idempotent.
	if err := fixture.portService.ClosePortTunnel(t.Context(), tunnel, "public port tunnel disconnected"); err != nil {
		t.Fatal(err)
	}
	// The Runner is still owed its confirmation within the grace.
	if _, err := fixture.dataPlaneStore.SweepDataPlane(
		t.Context(), fixture.now.Add(runnercontrol.PortCancellationConfirmationGrace-time.Second), 100,
	); err != nil {
		t.Fatal(err)
	}
	if got := fixture.admittedOperations(t); got != 1 {
		t.Fatalf("admitted operations within the confirmation grace = %d", got)
	}
	if changed, err := fixture.dataPlaneStore.SweepDataPlane(
		t.Context(), fixture.now.Add(runnercontrol.PortCancellationConfirmationGrace), 100,
	); err != nil || !changed {
		t.Fatalf("unconfirmed Port cancellation sweep = %t, %v", changed, err)
	}
	state, terminalKind := fixture.dataPlaneState(t, tunnel.Session.ID)
	if state != "completed" || terminalKind != runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("unconfirmed Port cancellation = %q %q", state, terminalKind)
	}
	if got := fixture.admittedOperations(t); got != 0 {
		t.Fatalf("admitted operations after the confirmation grace = %d", got)
	}
	var portState, activityState string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT port.state,activity.state FROM secondbox.port_sessions AS port
		JOIN secondbox.activity_sessions AS activity ON activity.id=port.id WHERE port.id=$1`,
		tunnel.Session.ID,
	).Scan(&portState, &activityState); err != nil {
		t.Fatal(err)
	}
	if portState != contracts.PortSessionStateClosed || activityState != "closed" {
		t.Fatalf("unconfirmed Port projection = %q, activity %q", portState, activityState)
	}
	// Later sweeps leave the completed session as it is.
	if _, err := fixture.dataPlaneStore.SweepDataPlane(
		t.Context(), fixture.now.Add(2*runnercontrol.PortCancellationConfirmationGrace), 100,
	); err != nil {
		t.Fatal(err)
	}
	if repeated, _ := fixture.dataPlaneState(t, tunnel.Session.ID); repeated != "completed" {
		t.Fatalf("repeated sweep state = %q", repeated)
	}
}

// A cancelling session whose Instance has ended can never be confirmed by a
// Runner. It completes at the next sweep instead of holding admission forever.
func TestPostgresCancellingSessionOfEndedAssignmentReleasesAdmission(t *testing.T) {
	fixture := newPortSessionFixture(t, "port-ended-assignment", fixtureControlPlaneNow)
	lease, err := fixture.controlPlane.AcquireSandboxLease(
		t.Context(), fixture.principal, fixture.sandbox.ID, fixture.sandbox.Generation, "port-ended-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := fixture.openConsumedPortTunnel(t, lease.ID, "port-ended-assignment")
	if err := fixture.portService.ClosePortTunnel(t.Context(), tunnel, "public port tunnel disconnected"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.dataPlaneStore.SweepDataPlane(t.Context(), *fixture.now, 100); err != nil {
		t.Fatal(err)
	}
	if state, _ := fixture.dataPlaneState(t, tunnel.Session.ID); state != "cancelling" {
		t.Fatalf("session with a live Assignment swept to %q", state)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.assignments SET state='released' WHERE id=$1`, tunnel.AssignmentID,
	); err != nil {
		t.Fatal(err)
	}
	if changed, err := fixture.dataPlaneStore.SweepDataPlane(t.Context(), *fixture.now, 100); err != nil || !changed {
		t.Fatalf("sweep after the Assignment ended = %t, %v", changed, err)
	}
	if state, _ := fixture.dataPlaneState(t, tunnel.Session.ID); state != "completed" {
		t.Fatalf("ended-Assignment session state = %q", state)
	}
	if got := fixture.admittedOperations(t); got != 0 {
		t.Fatalf("admitted operations after the Assignment ended = %d", got)
	}
}

// A PortSession admitted under a Lease outlives the Lease's current grant: it
// lives while the Lease is renewed, bounded by its own duration, and ends
// promptly when the Lease is released, lapses, or is fenced.
func TestPostgresPortSessionLivesWhileItsLeaseIsRenewed(t *testing.T) {
	fixture := newPortSessionFixture(t, "port-lease-lifetime", fixtureControlPlaneNow)
	start := *fixture.now
	sweep := func() {
		t.Helper()
		if _, err := fixture.dataPlaneStore.SweepDataPlane(t.Context(), *fixture.now, 100); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(key string) contracts.Lease {
		t.Helper()
		lease, err := fixture.portService.AcquireSandboxLease(
			t.Context(), fixture.principal, fixture.sandbox.ID, fixture.sandbox.Generation, key, 60,
		)
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	openLongSession := func(leaseID string, key string) runnercontrol.PortTunnel {
		t.Helper()
		// The session asks for five minutes under a one-minute Lease grant.
		session, _, err := fixture.portService.CreateSandboxPortSession(
			t.Context(), fixture.principal, "request-"+key, fixture.sandbox.ID, fixture.sandbox.Generation,
			leaseID, key, contracts.PortTransportProxied,
			contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 300},
		)
		if err != nil {
			t.Fatalf("PortSession longer than the Lease grant: %v", err)
		}
		if !session.ExpiresAt.Equal(fixture.now.Add(300 * time.Second)) {
			t.Fatalf("PortSession expiresAt = %s", session.ExpiresAt)
		}
		tunnel, err := fixture.portService.ConsumePortTunnel(t.Context(), session.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		return tunnel
	}

	lease := acquire("port-lifetime-renewed")
	renewed := openLongSession(lease.ID, "port-lifetime-renewed")
	for renewal := 1; renewal <= 4; renewal++ {
		*fixture.now = start.Add(time.Duration(renewal) * 50 * time.Second)
		if _, err := fixture.portService.RenewSandboxLease(
			t.Context(), fixture.principal, lease.ID, fmt.Sprintf("port-lifetime-renew-%d", renewal), 60,
		); err != nil {
			t.Fatalf("renewal %d: %v", renewal, err)
		}
		sweep()
		if state, _ := fixture.dataPlaneState(t, renewed.Session.ID); state != "running" {
			t.Fatalf("after renewal %d at +%s the session is %q", renewal, fixture.now.Sub(start), state)
		}
	}
	if _, err := fixture.portService.ReleaseSandboxLease(
		t.Context(), fixture.principal, lease.ID, "port-lifetime-release",
	); err != nil {
		t.Fatal(err)
	}
	sweep()
	if state, terminal := fixture.dataPlaneState(t, renewed.Session.ID); state != "cancelling" ||
		terminal != runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("released-Lease session = %q %q", state, terminal)
	}

	lapsing := acquire("port-lifetime-lapsed")
	lapsed := openLongSession(lapsing.ID, "port-lifetime-lapsed")
	*fixture.now = fixture.now.Add(61 * time.Second)
	sweep()
	if state, _ := fixture.dataPlaneState(t, lapsed.Session.ID); state != "cancelling" {
		t.Fatalf("lapsed-Lease session = %q", state)
	}

	fencing := acquire("port-lifetime-fenced")
	fenced := openLongSession(fencing.ID, "port-lifetime-fenced")
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.leases SET state='fenced' WHERE id=$1`, fencing.ID,
	); err != nil {
		t.Fatal(err)
	}
	sweep()
	if state, _ := fixture.dataPlaneState(t, fenced.Session.ID); state != "cancelling" {
		t.Fatalf("fenced-Lease session = %q", state)
	}
}
