package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"google.golang.org/protobuf/proto"
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
	seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "port-session-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PortSessionRelay: relay, PublicBaseURL: "https://secondbox.example",
	})
	if err != nil {
		t.Fatal(err)
	}

	session, replayed, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-session", sandbox.ID, sandbox.Generation,
		lease.ID, "port-session-create", contracts.CreatePortSessionRequest{
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
		lease.ID, "port-session-create", contracts.CreatePortSessionRequest{
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
		lease.ID, "port-session-create", contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 31,
		},
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-disabled", sandbox.ID, sandbox.Generation,
		lease.ID, "port-disabled", contracts.CreatePortSessionRequest{
			Name: "admin", DurationSeconds: 30,
		},
	); !errors.Is(err, ports.ErrPortPolicyDenied) {
		t.Fatalf("disabled port error = %v", err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-stale-generation", sandbox.ID, sandbox.Generation+1,
		lease.ID, "port-stale-generation", contracts.CreatePortSessionRequest{
			Name: "web", DurationSeconds: 30,
		},
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale generation error = %v", err)
	}
	crossProject := principal
	crossProject.TenantRef = "project-outside-port-authority"
	crossProject.TenantRef = "tenant-outside-port-authority"
	if _, err := portService.GetSandboxPortSession(
		t.Context(), crossProject, sandbox.ID, session.ID,
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
		leaseSweep.ID, "port-lease-sweep",
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
	if changed, err := relay.SweepDataPlane(t.Context(), now.Add(time.Second), 100); err != nil || !changed {
		t.Fatalf("inactive Lease Port sweep = %t, %v", changed, err)
	}
	var portCancellationPayload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT payload FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='outbound'
		ORDER BY sequence DESC LIMIT 1`,
		leaseSwept.ID,
	).Scan(&portCancellationPayload); err != nil {
		t.Fatal(err)
	}
	var portCancellation runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(portCancellationPayload, &portCancellation); err != nil {
		t.Fatal(err)
	}
	if cancel := portCancellation.GetPort().GetCancel(); cancel == nil ||
		cancel.Reason != "operation Lease is inactive" {
		t.Fatalf("inactive Lease Port cancellation = %#v", portCancellation.GetPort())
	}
	leaseSweptState, err := portService.GetSandboxPortSession(
		t.Context(), principal, sandbox.ID, leaseSwept.ID,
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
		lease.ID, "port-expiry", contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 10},
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
		t.Context(), principal, sandbox.ID, expiring.ID,
	)
	if err != nil || expired.State != contracts.PortSessionStateExpired {
		t.Fatalf("expired PortSession = %#v, %v", expired, err)
	}
	now = now.Add(-11 * time.Second)

	disconnected, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-runner-disconnect", sandbox.ID, sandbox.Generation,
		lease.ID, "port-runner-disconnect",
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	disconnectedTunnel, err := portService.ConsumePortTunnel(t.Context(), disconnected.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runner_connections
		SET state='closed',disconnected_at=$2
		WHERE runner_id=$1 AND state='active'`,
		disconnectedTunnel.RunnerID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := portService.NextPortTunnelEvent(
		t.Context(), disconnectedTunnel, -1,
	); !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("runner disconnect Port error = %v", err)
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
	if disconnectedState != contracts.PortSessionStateFenced || disconnectedActivity != "closed" {
		t.Fatalf("runner disconnect state=%q activity=%q", disconnectedState, disconnectedActivity)
	}

	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, lease.ID, "port-stale-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := portService.CreateSandboxPortSession(
		t.Context(), principal, "request-port-stale-lease", sandbox.ID, sandbox.Generation,
		lease.ID, "port-stale-lease",
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 10},
	); !errors.Is(err, ports.ErrLeaseInactive) {
		t.Fatalf("stale Lease PortSession error = %v", err)
	}
}
