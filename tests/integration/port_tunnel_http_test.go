package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestPublicPortTunnelIsBinarySingleUseBackpressuredAndAccounted(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "port-tunnel-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-port-tunnel-http")
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle", "sandbox:ports",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "port-tunnel-http", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "port-tunnel-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := seedDataPlaneReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "port-tunnel-http-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.leases SET expires_at=$2,updated_at=$1 WHERE id=$3`,
		now, now.Add(time.Minute), lease.ID,
	); err != nil {
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
	liveDataPlane := runnercontrol.NewLiveDataPlaneBroker()
	server := httptest.NewUnstartedServer(nil)
	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        dataPlaneStore, DataPlanePollInterval: time.Millisecond,
		LiveDataPlane:    liveDataPlane,
		PortSessionStore: dataPlaneStore, PublicBaseURL: "http://" + server.Listener.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: portService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	fake, detachFake := newPortTunnelFakeRunner(
		t, liveDataPlane, dataPlaneStore, seed.RunnerID, seed.ConnectionOne,
	)
	defer detachFake()
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	session := createPortSessionHTTP(t, server.URL, key.Credential, sandbox, lease.ID)
	if session.Transport != contracts.PortTransportProxied ||
		!strings.HasPrefix(session.Endpoint, "ws://"+server.Listener.Addr().String()+"/v1/port-tunnels/") ||
		strings.Contains(session.Endpoint, seed.RunnerID) {
		t.Fatalf("proxied PortSession = %#v", session)
	}
	connection := dialPortTunnel(t, session.Endpoint)
	defer connection.Close()
	if connection.Subprotocol() != "secondbox.port.v1" {
		t.Fatalf("Port tunnel subprotocol = %q", connection.Subprotocol())
	}

	replayDialer, replayURL := portTunnelDialer(t, session.Endpoint)
	if replay, response, err := replayDialer.Dial(replayURL, nil); err == nil {
		replay.Close()
		t.Fatal("single-use Port tunnel token replay succeeded")
	} else if response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("Port tunnel token replay response = %#v, error=%v", response, err)
	} else {
		response.Body.Close()
	}

	open := waitPortFakeEvent(t, fake.events, "open")
	if open.frame.GetOpen() == nil || open.frame.GetOpen().GuestPort != 8080 ||
		open.frame.GetOpen().Protocol != "tcp" || open.frame.Sequence != 1 {
		t.Fatalf("live Port Open = %#v", open.frame)
	}
	clientPayload := []byte{0, 1, 0xff, 2}
	if err := connection.WriteMessage(websocket.BinaryMessage, clientPayload); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-fake.events:
		t.Fatalf("runner received Port event before credit = %#v", event.frame)
	case <-time.After(20 * time.Millisecond):
	}
	close(fake.grantClientCredit)
	initialCredit := waitPortFakeEvent(t, fake.events, "credit")
	if initialCredit.frame.GetCredit().ByteCount != 65536 || initialCredit.frame.Sequence != 2 {
		t.Fatalf("initial Port response credit = %#v", initialCredit.frame)
	}
	clientBytes := waitPortFakeEvent(t, fake.events, "bytes")
	if !bytes.Equal(clientBytes.frame.GetBytes().GetData(), clientPayload) ||
		clientBytes.frame.Sequence != 3 {
		t.Fatalf("runner-bound Port frame = %#v, want bytes %v", clientBytes.frame, clientPayload)
	}

	runnerPayload := []byte{0xff, 0, 3, 0}
	if err := fake.output(t.Context(), runnerPayload); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || !bytes.Equal(payload, runnerPayload) {
		t.Fatalf("public Port message type=%d payload=%v", messageType, payload)
	}
	returnedCredit := waitPortFakeEvent(t, fake.events, "credit")
	if returnedCredit.frame.GetCredit().GetByteCount() != uint64(len(runnerPayload)) ||
		returnedCredit.frame.Sequence != 4 {
		t.Fatalf("post-delivery Port credit = %#v", returnedCredit.frame)
	}

	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var closed int64
		if err := pool.QueryRow(t.Context(), `
			SELECT count(*) FROM secondbox.activity_sessions
			WHERE id=$1 AND kind='port' AND state='closed'`, session.ID,
		).Scan(&closed); err != nil {
			t.Fatal(err)
		}
		if closed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Port tunnel disconnect did not close activity accounting")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-fakeErrors:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
}

func createPortSessionHTTP(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
) contracts.PortSession {
	t.Helper()
	body, err := json.Marshal(contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, baseURL+"/v1/sandboxes/"+sandbox.ID+"/port-sessions", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("SecondBox-Generation", fmt.Sprintf("%d", sandbox.Generation))
	request.Header.Set("SecondBox-Lease-ID", leaseID)
	request.Header.Set("Idempotency-Key", "port-tunnel-http-create")
	response := doHTTP(t, request)
	assertHTTPStatus(t, response, http.StatusCreated)
	var session contracts.PortSession
	decodeHTTPJSON(t, response, &session)
	return session
}

func dialPortTunnel(t *testing.T, endpoint string) *websocket.Conn {
	t.Helper()
	dialer, dialURL := portTunnelDialer(t, endpoint)
	connection, response, err := dialer.Dial(dialURL, nil)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("dial Port tunnel: status=%d body=%s error=%v", response.StatusCode, body, err)
		}
		t.Fatal(err)
	}
	return connection
}

func portTunnelDialer(t *testing.T, endpoint string) (websocket.Dialer, string) {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Fragment == "" {
		t.Fatalf("Port endpoint = %q, error=%v", endpoint, err)
	}
	token := parsed.Fragment
	parsed.Fragment = ""
	return websocket.Dialer{Subprotocols: []string{
		"secondbox.port.v1", "secondbox.port.token." + token,
	}}, parsed.String()
}

type portFakeEvent struct {
	kind  string
	frame *runnerv1.PortFrame
}

type portTunnelFakeRunner struct {
	broker            *runnercontrol.LiveDataPlaneBroker
	dataPlaneStore    *runnercontrol.PostgresDataPlaneStore
	session           *runnercontrol.Session
	runnerID          string
	connectionID      string
	incoming          chan *runnerv1.ControlPlaneToRunner
	events            chan portFakeEvent
	grantClientCredit chan struct{}
	mu                sync.Mutex
	open              *runnerv1.PortFrame
	nextSequence      uint64
}

func newPortTunnelFakeRunner(
	t *testing.T,
	broker *runnercontrol.LiveDataPlaneBroker,
	dataPlaneStore *runnercontrol.PostgresDataPlaneStore,
	runnerID string,
	connectionID string,
) (*portTunnelFakeRunner, func()) {
	t.Helper()
	features := []runnerv1.RunnerFeature{
		runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
	}
	session := runnercontrol.NewSession(runnercontrol.SessionConfig{
		AuthenticatedRunnerID: runnerID,
		SupportedVersions:     runnercontrol.VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures:       features, HeartbeatInterval: 10 * time.Second,
		ConnectionID: connectionID,
	})
	if response, err := session.Accept(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{Hello: &runnerv1.RunnerHello{
			RunnerId: runnerID, ConnectionNonce: bytes.Repeat([]byte{0x51}, 32),
			SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			MandatoryFeatures: features,
		}},
	}); err != nil || response.GetWelcome() == nil {
		t.Fatalf("fake Port Runner Hello = %#v, %v", response, err)
	}
	if _, err := session.Accept(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{Registration: &runnerv1.RunnerRegistration{
			MessageId: "registration", Sequence: 1, RunnerId: runnerID,
			ConnectionId: connectionID, RunnerPoolId: "default-pool",
			SoftwareVersion: "integration", ProtocolVersion: 1,
			Capabilities: &runnerv1.RunnerCapabilities{
				Architecture: "amd64", ComputeBackendVersion: "integration",
				HypervisorReady: true, IsolationReady: true, ResourceLimitsReady: true,
				NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
				DataPlaneReady:           true,
				GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			},
			Allocatable: &runnerv1.Capacity{VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8},
			Reserved:    &runnerv1.Capacity{}, StartupTiming: &runnerv1.StartupTiming{},
			DataPlaneAdvertisedAddress:     "10.0.0.5:7443",
			DataPlaneCertificateSpkiSha256: strings.Repeat("a", 64),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &portTunnelFakeRunner{
		broker: broker, dataPlaneStore: dataPlaneStore, session: session,
		runnerID: runnerID, connectionID: connectionID,
		incoming: make(chan *runnerv1.ControlPlaneToRunner, 16),
		events:   make(chan portFakeEvent, 16), grantClientCredit: make(chan struct{}),
		nextSequence: 1,
	}
	detach, err := broker.AttachConnection(runnerID, connectionID, fake, session)
	if err != nil {
		t.Fatal(err)
	}
	return fake, detach
}

func (fake *portTunnelFakeRunner) Send(message *runnerv1.ControlPlaneToRunner) error {
	fake.incoming <- message
	return nil
}

func (fake *portTunnelFakeRunner) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case message := <-fake.incoming:
			if err := fake.handle(ctx, message.GetPort()); err != nil {
				return err
			}
		}
	}
}

func (fake *portTunnelFakeRunner) handle(ctx context.Context, frame *runnerv1.PortFrame) error {
	if frame == nil {
		return nil
	}
	fake.events <- portFakeEvent{kind: portFakeEventKind(frame), frame: proto.Clone(frame).(*runnerv1.PortFrame)}
	if frame.GetOpen() == nil {
		return nil
	}
	fake.mu.Lock()
	fake.open = proto.Clone(frame).(*runnerv1.PortFrame)
	fake.mu.Unlock()
	select {
	case <-fake.grantClientCredit:
		return fake.deliver(ctx, &runnerv1.PortFrame_Credit{Credit: &runnerv1.StreamCredit{
			ByteCount: 65536,
		}})
	case <-ctx.Done():
		return nil
	}
}

func (fake *portTunnelFakeRunner) output(ctx context.Context, payload []byte) error {
	return fake.deliver(ctx, &runnerv1.PortFrame_Bytes{Bytes: &runnerv1.PortBytes{
		Data: bytes.Clone(payload),
	}})
}

func (fake *portTunnelFakeRunner) deliver(ctx context.Context, payload any) error {
	fake.mu.Lock()
	open := proto.Clone(fake.open).(*runnerv1.PortFrame)
	sequence := fake.nextSequence
	fake.nextSequence++
	fake.mu.Unlock()
	frame := &runnerv1.PortFrame{
		Fence: open.Fence, OperationId: open.OperationId, StreamId: open.StreamId,
		Sequence: sequence, Correlation: open.Correlation,
	}
	switch value := payload.(type) {
	case *runnerv1.PortFrame_Credit:
		frame.Payload = value
	case *runnerv1.PortFrame_Bytes:
		frame.Payload = value
	default:
		return fmt.Errorf("fake Port Runner payload %T is invalid", payload)
	}
	message := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Port{Port: frame},
	}
	event, err := fake.session.Accept(message)
	if err != nil {
		return err
	}
	deliver, err := fake.dataPlaneStore.RecordPortSessionFrame(ctx, runnercontrol.RunnerDataPlaneFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID, Message: message,
	}, time.Now().UTC())
	if err != nil || !deliver {
		return errors.Join(err, errors.New("fake proxied Port frame was not deliverable"))
	}
	return fake.broker.Deliver(ctx, event)
}

func portFakeEventKind(frame *runnerv1.PortFrame) string {
	switch {
	case frame.GetOpen() != nil:
		return "open"
	case frame.GetCredit() != nil:
		return "credit"
	case frame.GetBytes() != nil:
		return "bytes"
	default:
		return "unknown"
	}
}

func waitPortFakeEvent(
	t *testing.T,
	events <-chan portFakeEvent,
	kind string,
) portFakeEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.kind != kind {
			t.Fatalf("fake Port event kind = %q, want %q: %#v", event.kind, kind, event.frame)
		}
		return event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for fake Port event %q", kind)
		return portFakeEvent{}
	}
}
