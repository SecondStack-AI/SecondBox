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
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// wakeupTerminalPollInterval is long enough that polling cannot explain a prompt
// delivery. If the notification path regresses, the test fails on its read
// deadline rather than passing slowly.
const wakeupTerminalPollInterval = 60 * time.Second

// TestPublicTerminalDeliversOnNotificationRatherThanPollInterval proves the whole
// inbound chain: a durable inbound frame fires the migration trigger, the
// PostgreSQL listener decodes it, the hub fans it out by session, and the
// caller-facing loop wakes. The poll interval remains configured as the recovery
// bound and is deliberately too long to account for the result.
func TestPublicTerminalDeliversOnNotificationRatherThanPollInterval(t *testing.T) {
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
	seed := seedRelayReadyAssignment(t, sandbox, now)
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
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)

	// The real listener is used rather than a hand-published hub, so a broken
	// trigger, payload, or kind registration fails this test.
	hub := worknotify.NewHub()
	listener, err := worknotify.NewPostgresListener(t.Context(), integrationDatabaseURL, hub)
	if err != nil {
		t.Fatal(err)
	}
	listenerContext, stopListener := context.WithCancel(t.Context())
	defer stopListener()
	listenerErrors := make(chan error, 1)
	go func() { listenerErrors <- listener.Run(listenerContext) }()

	server := httptest.NewUnstartedServer(nil)
	publicBaseURL := "http://" + server.Listener.Addr().String()
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return time.Now().UTC() }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: wakeupTerminalPollInterval,
		DataPlaneWakeups: hub,
		PublicBaseURL:    publicBaseURL,
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

	fake := newTerminalHTTPFakeRunner(relay, seed.RunnerID, seed.ConnectionTwo)
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

	// Granting credit makes the fake Runner emit output, which lands as a durable
	// inbound frame and is the event under test.
	if err := connection.WriteJSON(map[string]any{
		"type": "credit", "sequence": 0, "bytes": 8,
	}); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "credit:terminal-order:8")

	// A deadline well inside the poll interval: only the notification path can
	// deliver in time.
	if err := connection.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	assertTerminalOutput(t, connection, 0, []byte{0x00, 0x01, 0xfe, 0xff})
	if elapsed := time.Since(started); elapsed >= wakeupTerminalPollInterval {
		t.Fatalf("Terminal output took %s, which the poll interval could explain", elapsed)
	}

	stopListener()
	if err := <-listenerErrors; err != nil && listenerContext.Err() == nil {
		t.Fatalf("work listener: %v", err)
	}
}
