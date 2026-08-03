package integration_test

import (
	"crypto/sha256"
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
	"google.golang.org/protobuf/proto"
)

const directPortRunnerAddress = "10.9.8.7:7443"
const directPortCertificateSPKISHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestPostgresDirectPortTransportAdmissionAndCredentialConsumption proves that the
// direct transport changes only the endpoint and the byte path: admission stays
// transactional and fenced, and PostgreSQL remains the single authority that
// spends the single-use credential.
func TestPostgresDirectPortTransportAdmissionAndCredentialConsumption(t *testing.T) {
	// The fixture control plane and this PortSession service share one clock so
	// the session always sits inside the Lease that admitted it.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixture := newDirectPortFixture(t, directPortFixtureName(t), &now)

	direct, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-port", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-create",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Transport != contracts.PortTransportDirect {
		t.Fatalf("direct PortSession transport = %q", direct.Transport)
	}
	endpoint, err := url.Parse(direct.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Scheme != "secondbox+tcp" || endpoint.Host != directPortRunnerAddress ||
		endpoint.Path != "/v1/port-sessions/"+direct.ID || endpoint.Fragment == "" ||
		direct.CertificateSPKISHA256 != directPortCertificateSPKISHA256 {
		t.Fatalf("direct PortSession endpoint = %q", direct.Endpoint)
	}
	credential := endpoint.Fragment
	digest := sha256.Sum256([]byte(credential))

	// The home Runner is admitted with the assignment-bound session state and the
	// credential digest, never the credential itself.
	open := directPortOpenFrame(t, fixture.pool, direct.ID)
	if open.GetGuestPort() != 8080 || open.GetProtocol() != "tcp" ||
		open.GetPortName() != "web" || open.GetLeaseId() != fixture.leaseID ||
		open.GetDeadlineUnixMs() != uint64(direct.ExpiresAt.UTC().UnixMilli()) ||
		string(open.GetCredentialDigest()) != string(digest[:]) {
		t.Fatalf("direct Port Open = %#v", open)
	}
	if strings.Contains(prototext(open), credential) {
		t.Fatal("direct Port Open carries the single-use credential")
	}

	// The relay WebSocket cannot spend a direct session's credential.
	if _, err := fixture.portService.ConsumePortTunnelToken(
		t.Context(), direct.ID, credential,
	); !errors.Is(err, ports.ErrPortTokenInvalid) {
		t.Fatalf("relay consumption of a direct PortSession error = %v", err)
	}

	consumption := runnercontrol.DirectPortConsumption{
		RunnerID: fixture.runnerID, SessionID: direct.ID,
		AssignmentID: fixture.assignmentID, Generation: fixture.generation,
		FencingToken: fixture.fencingToken, CredentialDigest: digest[:], Now: now,
	}
	for name, mutate := range map[string]struct {
		mutate func(runnercontrol.DirectPortConsumption) runnercontrol.DirectPortConsumption
		want   error
	}{
		"foreign_runner": {
			mutate: func(input runnercontrol.DirectPortConsumption) runnercontrol.DirectPortConsumption {
				input.RunnerID = "run_not_home"
				return input
			},
			want: ports.ErrPortSessionNotFound,
		},
		"wrong_credential": {
			mutate: func(input runnercontrol.DirectPortConsumption) runnercontrol.DirectPortConsumption {
				other := sha256.Sum256([]byte("another-credential"))
				input.CredentialDigest = other[:]
				return input
			},
			want: ports.ErrPortTokenInvalid,
		},
		"superseded_generation": {
			mutate: func(input runnercontrol.DirectPortConsumption) runnercontrol.DirectPortConsumption {
				input.Generation++
				return input
			},
			want: ports.ErrPortTokenInvalid,
		},
		"stale_fence": {
			mutate: func(input runnercontrol.DirectPortConsumption) runnercontrol.DirectPortConsumption {
				input.FencingToken = []byte("stale-fence")
				return input
			},
			want: ports.ErrPortTokenInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.relay.ConsumeDirectPortSession(
				t.Context(), mutate.mutate(consumption),
			); !errors.Is(err, mutate.want) {
				t.Fatalf("consumption error = %v, want %v", err, mutate.want)
			}
		})
	}

	tunnel, err := fixture.relay.ConsumeDirectPortSession(t.Context(), consumption)
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.Session.ID != direct.ID || tunnel.GuestPort != 8080 ||
		tunnel.RunnerID != fixture.runnerID ||
		tunnel.Session.Transport != contracts.PortTransportDirect {
		t.Fatalf("consumed direct tunnel = %#v", tunnel)
	}
	var activeActivity int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.activity_sessions
		WHERE id=$1 AND kind='port' AND state='active'`, direct.ID,
	).Scan(&activeActivity); err != nil {
		t.Fatal(err)
	}
	if activeActivity != 1 {
		t.Fatalf("direct Port activity rows = %d", activeActivity)
	}
	if _, err := fixture.relay.ConsumeDirectPortSession(
		t.Context(), consumption,
	); !errors.Is(err, ports.ErrPortTokenConsumed) {
		t.Fatalf("replayed direct credential error = %v", err)
	}

	// No Port payload byte is persisted for a direct session: the only durable
	// frame is the admitting Open.
	var frameCount int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`, direct.ID,
	).Scan(&frameCount); err != nil {
		t.Fatal(err)
	}
	if frameCount != 1 {
		t.Fatalf("direct PortSession durable frame count = %d", frameCount)
	}
}

func TestPostgresDirectPortRejectsPolicyDeadlineAndLeaseFailures(t *testing.T) {
	// The fixture control plane and this PortSession service share one clock so
	// the session always sits inside the Lease that admitted it.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixture := newDirectPortFixture(t, directPortFixtureName(t), &now)

	if _, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-unnamed", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-unnamed",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "admin", DurationSeconds: 60},
	); !errors.Is(err, ports.ErrPortPolicyDenied) {
		t.Fatalf("undeclared named port error = %v", err)
	}

	expiring, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-expiry", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-expiry",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	expiringDigest := directPortCredentialDigest(t, expiring.Endpoint)
	now = now.Add(11 * time.Second)
	if _, err := fixture.relay.ConsumeDirectPortSession(
		t.Context(),
		runnercontrol.DirectPortConsumption{
			RunnerID: fixture.runnerID, SessionID: expiring.ID,
			AssignmentID: fixture.assignmentID, Generation: fixture.generation,
			FencingToken: fixture.fencingToken, CredentialDigest: expiringDigest[:], Now: now,
		},
	); !errors.Is(err, ports.ErrPortTokenInvalid) {
		t.Fatalf("post-deadline direct consumption error = %v", err)
	}
	now = now.Add(-11 * time.Second)

	leased, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-lease", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-lease",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	leasedDigest := directPortCredentialDigest(t, leased.Endpoint)
	if _, err := fixture.controlPlane.ReleaseSandboxLease(
		t.Context(), fixture.principal, fixture.leaseID, "direct-port-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.relay.ConsumeDirectPortSession(
		t.Context(),
		runnercontrol.DirectPortConsumption{
			RunnerID: fixture.runnerID, SessionID: leased.ID,
			AssignmentID: fixture.assignmentID, Generation: fixture.generation,
			FencingToken: fixture.fencingToken, CredentialDigest: leasedDigest[:], Now: now,
		},
	); !errors.Is(err, ports.ErrLeaseInactive) {
		t.Fatalf("inactive Lease direct consumption error = %v", err)
	}
}

// TestPostgresPortTransportGrantGovernsRunnerAddressExposure proves the public
// surface property: only a caller holding the direct grant learns a Runner
// address, and an ungranted caller keeps today's relay endpoint shape.
func TestPostgresPortTransportGrantGovernsRunnerAddressExposure(t *testing.T) {
	// The fixture control plane and this PortSession service share one clock so
	// the session always sits inside the Lease that admitted it.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixture := newDirectPortFixture(t, directPortFixtureName(t), &now)

	relaySession, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-relay-port", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "relay-port-create",
		contracts.PortTransportRelay,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	if relaySession.Transport != contracts.PortTransportRelay ||
		!strings.HasPrefix(relaySession.Endpoint, "wss://secondbox.example/v1/port-tunnels/") ||
		strings.Contains(relaySession.Endpoint, directPortRunnerAddress) ||
		strings.Contains(relaySession.Endpoint, fixture.runnerID) {
		t.Fatalf("relay PortSession endpoint = %q", relaySession.Endpoint)
	}
	// An ungranted caller reading a relay session observes the identical shape.
	read, err := fixture.portService.GetSandboxPortSession(
		t.Context(), fixture.principal, fixture.sandboxID, relaySession.ID,
		contracts.PortTransportRelay,
	)
	if err != nil || read.Endpoint != relaySession.Endpoint {
		t.Fatalf("relay PortSession read endpoint = %q, %v", read.Endpoint, err)
	}

	direct, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-grant", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-grant",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.portService.GetSandboxPortSession(
		t.Context(), fixture.principal, fixture.sandboxID, direct.ID,
		contracts.PortTransportRelay,
	); !errors.Is(err, ports.ErrAuthorizationDenied) {
		t.Fatalf("ungranted read of a direct PortSession error = %v", err)
	}
	granted, err := fixture.portService.GetSandboxPortSession(
		t.Context(), fixture.principal, fixture.sandboxID, direct.ID,
		contracts.PortTransportDirect,
	)
	if err != nil || granted.Endpoint != direct.Endpoint {
		t.Fatalf("granted read of a direct PortSession = %q, %v", granted.Endpoint, err)
	}
}

func TestPostgresDirectPortRequiresAnAdvertisedRunnerAddressAndCertificatePin(t *testing.T) {
	// The fixture control plane and this PortSession service share one clock so
	// the session always sits inside the Lease that admitted it.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fixture := newDirectPortFixture(t, directPortFixtureName(t), &now)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET data_plane_address=$2 WHERE id=$1`,
		fixture.runnerID,
		directPortRunnerAddress,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-unpinned", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-unpinned",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	); !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("direct PortSession without a certificate pin error = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET data_plane_address='' WHERE id=$1`,
		fixture.runnerID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.portService.CreateSandboxPortSession(
		t.Context(), fixture.principal, "request-direct-unadvertised", fixture.sandboxID,
		fixture.generation, fixture.leaseID, "direct-port-unadvertised",
		contracts.PortTransportDirect,
		contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 60},
	); !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("direct PortSession without an advertised Runner address error = %v", err)
	}
}

type directPortFixture struct {
	controlPlane *service.ControlPlaneService
	portService  *service.ControlPlaneService
	relay        *runnercontrol.PostgresFrameRelay
	pool         *pgxpool.Pool
	principal    contracts.Principal
	sandboxID    string
	generation   int64
	leaseID      string
	runnerID     string
	assignmentID string
	fencingToken []byte
}

// directPortFixtureName derives a Profile-safe suffix so each test owns its own
// Profile, account, and Sandbox.
func directPortFixtureName(t *testing.T) string {
	t.Helper()
	lowered := strings.Map(func(letter rune) rune {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9':
			return letter
		case letter >= 'A' && letter <= 'Z':
			return letter + ('a' - 'A')
		default:
			return -1
		}
	}, t.Name())
	return lowered[max(len(lowered)-24, 0):]
}

func newDirectPortFixture(t *testing.T, name string, now *time.Time) directPortFixture {
	t.Helper()
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "direct-port-"+name)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "direct-port-profile-"+name,
	)
	scopes := []string{"sandbox:read", "sandbox:lifecycle", "sandbox:ports"}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "direct-port-" + name, Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "direct-port-sandbox-"+name,
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seed := seedRelayReadyAssignment(t, sandbox, *now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "direct-port-lease-"+name, 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	seedAdvertisedDataPlaneRunner(t, pool, seed.RunnerID, *now)
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(),
		runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
			Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return *now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PortSessionRelay: relay, PublicBaseURL: "https://secondbox.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	return directPortFixture{
		controlPlane: controlPlane, portService: portService, relay: relay, pool: pool,
		principal: principal, sandboxID: sandbox.ID, generation: sandbox.Generation,
		leaseID: lease.ID, runnerID: seed.RunnerID,
		assignmentID: seed.Fence.AssignmentId, fencingToken: seed.Fence.FencingToken,
	}
}

// seedAdvertisedDataPlaneRunner records the administrative capacity evidence a
// direct endpoint resolves against.
func seedAdvertisedDataPlaneRunner(
	t *testing.T,
	pool *pgxpool.Pool,
	runnerID string,
	now time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ('direct-port-pool','ready','["amd64"]','["compute"]','{}',1,1,$1,$1)
		ON CONFLICT (name) DO NOTHING`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,data_plane_address,last_seen_at,revision,
			created_at,updated_at
		) VALUES (
			$1,'direct-port-pool',$1,'ready','["amd64"]','["compute","port-data-plane"]','{}',
			'["1"]',1,1,'1.0.0','connection_direct',0,'active','{}','[]',0,0,$2,$3,1,$3,$3
		)
		ON CONFLICT (id) DO UPDATE SET data_plane_address=EXCLUDED.data_plane_address`,
		runnerID, fmt.Sprintf(
			`{"address":%q,"certificateSpkiSha256":%q}`,
			directPortRunnerAddress,
			directPortCertificateSPKISHA256,
		), now,
	); err != nil {
		t.Fatal(err)
	}
}

func directPortOpenFrame(
	t *testing.T,
	pool *pgxpool.Pool,
	sessionID string,
) *runnerv1.PortDirectOpen {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT payload FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='outbound' AND sequence=1`,
		sessionID,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var message runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	open := message.GetPort().GetDirectOpen()
	if open == nil {
		t.Fatalf("direct PortSession admitting frame = %#v", message.GetPort())
	}
	return open
}

func directPortCredentialDigest(t *testing.T, endpoint string) [sha256.Size]byte {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Fragment == "" {
		t.Fatalf("direct PortSession endpoint %q carries no credential", endpoint)
	}
	return sha256.Sum256([]byte(parsed.Fragment))
}

func prototext(message proto.Message) string {
	return message.(interface{ String() string }).String()
}
