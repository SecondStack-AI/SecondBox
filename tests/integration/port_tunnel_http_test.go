package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	seed := seedRelayReadyAssignment(t, sandbox, now)
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
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	server := httptest.NewUnstartedServer(nil)
	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PortSessionRelay: relay, PublicBaseURL: "http://" + server.Listener.Addr().String(),
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

	session := createPortSessionHTTP(t, server.URL, key.Credential, sandbox, lease.ID)
	firstOpen, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	)
	if err != nil || !found || firstOpen.Message.GetPort() == nil {
		t.Fatalf("first Port Open claim = %#v found=%t error=%v", firstOpen, found, err)
	}
	reclaimedOpen, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(51*time.Millisecond),
	)
	if err != nil || !found || reclaimedOpen.ID != firstOpen.ID {
		t.Fatalf("reclaimed Port Open = %#v found=%t error=%v", reclaimedOpen, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), firstOpen.ID, seed.ConnectionOne, firstOpen.ClaimAttempt,
		now.Add(51*time.Millisecond),
	); !errors.Is(err, runnercontrol.ErrRelayDeliveryClaim) {
		t.Fatalf("expired Port Open claim delivery error = %v, want ErrRelayDeliveryClaim", err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), reclaimedOpen.ID, seed.ConnectionOne, reclaimedOpen.ClaimAttempt,
		now.Add(51*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	open := reclaimedOpen.Message.GetPort()
	if open.GetOpen() == nil || open.GetOpen().GuestPort != 8080 || open.GetOpen().Protocol != "tcp" {
		t.Fatalf("Port Open = %#v", open.GetOpen())
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

	initialCredit := claimPortFrame(t, relay, seed, now)
	if initialCredit.GetCredit().GetByteCount() != 65536 {
		t.Fatalf("initial Port response credit = %#v", initialCredit.GetCredit())
	}
	clientPayload := []byte{0, 1, 0xff, 2}
	if err := connection.WriteMessage(websocket.BinaryMessage, clientPayload); err != nil {
		t.Fatal(err)
	}
	assertNoRunnerBoundPortBytes(t, relay, seed, now)
	persistRunnerPortFrame(t, relay, seed, open, 1, &runnerv1.PortFrame_Credit{
		Credit: &runnerv1.StreamCredit{ByteCount: 65536},
	}, now)
	clientBytes := claimPortFrameEventually(t, relay, seed, now)
	if !bytes.Equal(clientBytes.GetBytes().GetData(), clientPayload) {
		t.Fatalf("runner-bound Port frame = %v, want bytes %v", clientBytes, clientPayload)
	}

	runnerPayload := []byte{0xff, 0, 3, 0}
	persistRunnerPortFrame(t, relay, seed, open, 2, &runnerv1.PortFrame_Bytes{
		Bytes: &runnerv1.PortBytes{Data: runnerPayload},
	}, now)
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
	returnedCredit := claimPortFrameEventually(t, relay, seed, now)
	if returnedCredit.GetCredit().GetByteCount() != uint64(len(runnerPayload)) {
		t.Fatalf("post-delivery Port credit = %#v", returnedCredit.GetCredit())
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

func claimPortFrame(
	t *testing.T,
	relay *runnercontrol.PostgresFrameRelay,
	seed relayReadySeed,
	now time.Time,
) *runnerv1.PortFrame {
	t.Helper()
	delivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	)
	if err != nil || !found || delivery.Message.GetPort() == nil {
		t.Fatalf("claim Port frame = %#v found=%t error=%v", delivery, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now,
	); err != nil {
		t.Fatal(err)
	}
	return delivery.Message.GetPort()
}

func claimPortFrameEventually(
	t *testing.T,
	relay *runnercontrol.PostgresFrameRelay,
	seed relayReadySeed,
	now time.Time,
) *runnerv1.PortFrame {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), seed.RunnerID, seed.ConnectionOne, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if delivery.Message.GetPort() == nil {
				t.Fatalf("claimed non-Port frame = %#v", delivery.Message)
			}
			if err := relay.MarkOutboundFrameDelivered(
				t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now,
			); err != nil {
				t.Fatal(err)
			}
			return delivery.Message.GetPort()
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for outbound Port frame")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNoRunnerBoundPortBytes(
	t *testing.T,
	relay *runnercontrol.PostgresFrameRelay,
	seed relayReadySeed,
	now time.Time,
) {
	t.Helper()
	delivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	)
	if err != nil || found {
		if err != nil {
			t.Fatal(err)
		}
		port := delivery.Message.GetPort()
		if port != nil && port.GetBytes() != nil {
			t.Fatalf("runner-bound Port bytes bypassed credit: sequence=%d bytes=%v", port.Sequence, port.GetBytes().Data)
		}
		t.Fatalf(
			"unexpected Port control before runner credit: sequence=%d open=%v credit=%v cancel=%v",
			port.GetSequence(), port.GetOpen(), port.GetCredit(), port.GetCancel(),
		)
	}
}

func persistRunnerPortFrame(
	t *testing.T,
	relay *runnercontrol.PostgresFrameRelay,
	seed relayReadySeed,
	open *runnerv1.PortFrame,
	sequence uint64,
	payload any,
	now time.Time,
) {
	t.Helper()
	frame := &runnerv1.PortFrame{
		Fence: open.Fence, OperationId: open.OperationId, StreamId: open.StreamId, Sequence: sequence,
		Correlation: proto.Clone(open.Correlation).(*runnerv1.Correlation),
	}
	switch value := payload.(type) {
	case *runnerv1.PortFrame_Credit:
		frame.Payload = value
	case *runnerv1.PortFrame_Bytes:
		frame.Payload = value
	default:
		t.Fatalf("unsupported test Port payload %T", payload)
	}
	inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Port{Port: frame},
		},
	}, now)
	if err != nil || !inserted {
		t.Fatalf("persist runner Port frame = %t, %v", inserted, err)
	}
}
