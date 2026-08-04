package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// wakeupTerminalPollInterval is long enough that PostgreSQL polling cannot
// explain prompt delivery over the live data plane.
const wakeupTerminalPollInterval = 60 * time.Second

func TestPublicTerminalDeliversOverLiveDataPlaneRatherThanPollInterval(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "terminal-wakeup")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-terminal-wakeup")
	scopes := []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec"}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "terminal-wakeup", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "terminal-wakeup-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := seedDataPlaneReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "terminal-wakeup-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	leasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leasePool.Close)
	if _, err := leasePool.Exec(
		t.Context(),
		`UPDATE secondbox.leases SET expires_at=$2, updated_at=$3 WHERE id=$1`,
		lease.ID, now.Add(time.Minute), now,
	); err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL,
		Retention:   time.Hour, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	liveDataPlane := runnercontrol.NewLiveDataPlaneBroker()

	server := httptest.NewUnstartedServer(nil)
	publicBaseURL := "http://" + server.Listener.Addr().String()
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return time.Now().UTC() }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        relay, DataPlanePollInterval: wakeupTerminalPollInterval,
		LiveDataPlane: liveDataPlane,
		PublicBaseURL: publicBaseURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, PlatformToken: testPlatformToken,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)

	fake, detachFake := newTerminalHTTPFakeRunner(
		t, liveDataPlane, seed.RunnerID, seed.ConnectionTwo,
	)
	defer detachFake()
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	session := createTerminalSession(
		t, server.URL, key.Credential, sandbox, lease.ID,
		"terminal-wakeup-create-session", "terminal-order", true,
	)
	connection := dialTerminal(t, session, key.Credential, sandbox.Generation)
	defer connection.Close()

	// Granting credit makes the fake Runner emit output on the live stream.
	if err := connection.WriteJSON(map[string]any{
		"type": "credit", "sequence": 0, "bytes": 8,
	}); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "credit:terminal-order:8")

	// A deadline well inside the poll interval proves PostgreSQL polling is not
	// carrying terminal output.
	if err := connection.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	assertTerminalOutput(t, connection, 0, []byte{0x00, 0x01, 0xfe, 0xff})
	if elapsed := time.Since(started); elapsed >= wakeupTerminalPollInterval {
		t.Fatalf("Terminal output took %s, which the poll interval could explain", elapsed)
	}

}
