//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/portdirect"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

// directPortEchoRoundTrips is the interactive-echo sample count. An SSH session
// completes several round trips before its first prompt, so the qualification
// measures per-round-trip latency rather than one aggregate transfer.
const directPortEchoRoundTrips = 20

// TestScenarioDirectPortTransportQualification measures connect time and
// interactive echo for both Port transports against one live Sandbox, and proves
// that only an authority holding the exact grant learns a Runner address.
//
// The relay baseline is measured in the same run against the same guest listener
// so the comparison isolates the transport rather than the host, the guest, or
// the network. Both transports share one Sandbox so the qualification adds no
// compute footprint to the suite.
func TestScenarioDirectPortTransportQualification(t *testing.T) {
	fixture := newScenarioFixture(t)
	ingress := newScenarioDirectPortIngress(t, fixture)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)

	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Ports = []contracts.PortPolicy{{
		Name:                  "web",
		Port:                  8080,
		Protocol:              "tcp",
		MaximumSessions:       4,
		MaximumSessionSeconds: 60,
	}}
	profile := createScenarioProfile(t, fixture, ingress.profileName, spec)
	handle, _ := createScenarioSandbox(t, fixture, profile, "direct-port")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)

	lease := acquireScenarioLease(t, ctx, fixture, handle, 60, "direct-port-lease")
	startScenarioEchoListener(t, ctx, handle)

	relay := measureScenarioRelayPort(t, ctx, fixture, handle, lease.ID)
	direct := measureScenarioDirectPort(t, ctx, ingress, handle, lease.ID)

	t.Logf(
		"SecondBox Port transport qualification: relay connect=%v echo_mean=%v echo_max=%v session=%v; direct connect=%v echo_mean=%v echo_max=%v session=%v",
		relay.connect, relay.echoMean(), relay.echoMaximum(), relay.session(),
		direct.connect, direct.echoMean(), direct.echoMaximum(), direct.session(),
	)

	// The plan's success criterion is that interactive latency stops being
	// governed by the data-plane poll interval. The relay applies that interval
	// twice per round trip, so a direct round trip must land well inside one.
	pollInterval := scenarioDataPlanePollInterval(t)
	if direct.echoMean() >= pollInterval {
		t.Fatalf(
			"direct Port echo mean = %v, want below one %v data-plane poll interval",
			direct.echoMean(), pollInterval,
		)
	}
	if direct.echoMaximum() >= pollInterval {
		t.Fatalf(
			"direct Port echo maximum = %v, want below one %v data-plane poll interval",
			direct.echoMaximum(), pollInterval,
		)
	}
	// Connect is measured and logged but deliberately not gated. It pays one
	// control-plane consumption round trip plus one guest-protocol stream setup,
	// neither of which this plan changes, and both vary with host load. Raising
	// SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS is how to re-check
	// that connect does not track the poll interval; a fixed threshold on it
	// would assert host speed rather than a transport property.
	//
	// An SSH connection completes several round trips before its first prompt.
	// Comparing whole interactive sessions rather than the bare handshake is
	// what shows the transport difference: the relay defers its cost to every
	// round trip instead of charging it at connect.
	if direct.session() >= relay.session() {
		t.Fatalf(
			"direct Port interactive session = %v, relay baseline = %v",
			direct.session(), relay.session(),
		)
	}

	assertScenarioRunnerAddressNeedsTheGrant(t, ctx, fixture, ingress, handle, lease.ID)

	// Deletion reports terminal before the home Runner's reservation is released,
	// so returning here would hand the next test a starved Runner. Stopping waits
	// for the Instance to be gone, which releases the reservation synchronously
	// and keeps this qualification's compute footprint invisible to the suite.
	if stopped := stopScenarioSandbox(
		t, ctx, fixture, handle, "direct-port-stop",
	); stopped.Instance != nil {
		t.Fatalf("SecondBox scenario stopped Sandbox retained its Instance = %#v", stopped)
	}
}

// assertScenarioRunnerAddressNeedsTheGrant proves on a live deployment that the
// platform token, which grants every operation, still never learns a Runner
// data-plane address.
func assertScenarioRunnerAddressNeedsTheGrant(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	ingress scenarioDirectPortIngress,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
) {
	t.Helper()
	platform := createScenarioPortSession(t, ctx, fixture, handle, leaseID, "ungranted-relay")
	if platform.Transport != contracts.PortTransportRelay ||
		!strings.HasPrefix(platform.Endpoint, "ws") {
		t.Fatalf("ungranted PortSession = %#v", platform)
	}
	granted := createScenarioDirectPortSession(t, ctx, ingress, handle, leaseID, "granted-direct")
	if granted.Transport != contracts.PortTransportDirect || granted.CertificateSPKISHA256 == "" {
		t.Fatalf("granted PortSession = %#v", granted)
	}
	endpoint, err := url.Parse(granted.Endpoint)
	if err != nil || endpoint.Scheme != "secondbox+tcp" || endpoint.Host == "" {
		t.Fatalf("direct PortSession endpoint = %q, error = %v", granted.Endpoint, err)
	}
	if strings.Contains(platform.Endpoint, endpoint.Host) {
		t.Fatalf("relay endpoint %q leaked the Runner address", platform.Endpoint)
	}
}

type scenarioPortMeasurement struct {
	connect time.Duration
	echoes  []time.Duration
}

func (measurement scenarioPortMeasurement) echoMean() time.Duration {
	if len(measurement.echoes) == 0 {
		return 0
	}
	total := time.Duration(0)
	for _, sample := range measurement.echoes {
		total += sample
	}
	return total / time.Duration(len(measurement.echoes))
}

func (measurement scenarioPortMeasurement) echoMaximum() time.Duration {
	return slices.Max(measurement.echoes)
}

// session is connect plus every round trip: what an interactive client actually
// waits for before it is usable.
func (measurement scenarioPortMeasurement) session() time.Duration {
	total := measurement.connect
	for _, sample := range measurement.echoes {
		total += sample
	}
	return total
}

func measureScenarioRelayPort(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
) scenarioPortMeasurement {
	t.Helper()
	session := createScenarioPortSession(t, ctx, fixture, handle, leaseID, "relay-baseline")
	startedAt := time.Now()
	connection := dialScenarioPortTunnel(t, ctx, session.Endpoint)
	defer connection.Close()
	measurement := scenarioPortMeasurement{connect: time.Since(startedAt)}
	if err := connection.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for round := range directPortEchoRoundTrips {
		probe := scenarioEchoProbe(round)
		sentAt := time.Now()
		if err := connection.WriteMessage(websocket.BinaryMessage, probe); err != nil {
			t.Fatalf("relay baseline write: %v", err)
		}
		received := make([]byte, 0, len(probe))
		for len(received) < len(probe) {
			messageType, payload, err := connection.ReadMessage()
			if err != nil {
				t.Fatalf("relay baseline read: %v", err)
			}
			if messageType != websocket.BinaryMessage {
				t.Fatalf("relay baseline message type = %d", messageType)
			}
			received = append(received, payload...)
		}
		if !bytes.Equal(received, probe) {
			t.Fatalf("relay baseline echo = %q, want %q", received, probe)
		}
		measurement.echoes = append(measurement.echoes, time.Since(sentAt))
	}
	return measurement
}

func measureScenarioDirectPort(
	t *testing.T,
	ctx context.Context,
	ingress scenarioDirectPortIngress,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
) scenarioPortMeasurement {
	t.Helper()
	session := createScenarioDirectPortSession(t, ctx, ingress, handle, leaseID, "direct-measure")
	startedAt := time.Now()
	connection := dialScenarioDirectPort(t, ctx, session)
	defer connection.Close()
	measurement := scenarioPortMeasurement{connect: time.Since(startedAt)}
	if err := connection.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for round := range directPortEchoRoundTrips {
		probe := scenarioEchoProbe(round)
		sentAt := time.Now()
		if _, err := connection.Write(probe); err != nil {
			t.Fatalf("direct Port write: %v", err)
		}
		received := make([]byte, len(probe))
		if _, err := io.ReadFull(connection, received); err != nil {
			t.Fatalf("direct Port read: %v", err)
		}
		if !bytes.Equal(received, probe) {
			t.Fatalf("direct Port echo = %q, want %q", received, probe)
		}
		measurement.echoes = append(measurement.echoes, time.Since(sentAt))
	}
	return measurement
}

// dialScenarioDirectPort performs the bounded credential handshake an ingress
// tier performs before any payload byte.
func dialScenarioDirectPort(
	t *testing.T,
	ctx context.Context,
	session contracts.PortSession,
) net.Conn {
	t.Helper()
	parsed, err := url.Parse(session.Endpoint)
	if err != nil || parsed.Scheme != "secondbox+tcp" || parsed.Fragment == "" {
		t.Fatalf("direct PortSession endpoint = %q, error = %v", session.Endpoint, err)
	}
	tlsConfig, err := portdirect.TLSConfigForSPKIPin(session.CertificateSPKISHA256)
	if err != nil {
		t.Fatal(err)
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{}, Config: tlsConfig}
	connection, err := dialer.DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		t.Fatalf("dial Runner data plane %q: %v", parsed.Host, err)
	}
	if err := connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := portdirect.WriteCredential(connection, portdirect.SessionKindPort, parsed.Fragment); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	verdict, detail, err := portdirect.ReadVerdict(connection)
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if verdict != portdirect.VerdictAdmitted {
		connection.Close()
		t.Fatalf("direct Port admission denied: %s", detail)
	}
	return connection
}

// startScenarioEchoListener runs a real guest TCP echo listener on the approved
// named port. Echo is the shape interactive SSH traffic takes: one small write
// answered by one small read.
func startScenarioEchoListener(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
) {
	t.Helper()
	listener := executeScenarioCommand(
		t,
		ctx,
		handle,
		`nohup python3 -c '
import socketserver

class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        while True:
            chunk = self.request.recv(65536)
            if not chunk:
                return
            self.request.sendall(chunk)

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True

Server(("127.0.0.1", 8080), Echo).serve_forever()
' >/workspace/echo-server.log 2>&1 </dev/null &
python3 -c 'import socket, time
for _ in range(50):
    try:
        socket.create_connection(("127.0.0.1", 8080), 1).close()
        raise SystemExit(0)
    except OSError:
        time.sleep(0.1)
raise SystemExit(1)'`,
		4096,
		"direct-port-echo-listener",
	)
	assertScenarioExited(t, listener, 0, "", "")
}

func scenarioEchoProbe(round int) []byte {
	probe := make([]byte, 8)
	binary.BigEndian.PutUint64(probe, uint64(round)+1)
	return probe
}

// scenarioDirectPortIngress is the explicitly provisioned application authority
// that holds the exact direct-endpoint grant.
type scenarioDirectPortIngress struct {
	client      *secondboxclient.Client
	profileName string
}

func newScenarioDirectPortIngress(
	t *testing.T,
	fixture scenarioFixture,
) scenarioDirectPortIngress {
	t.Helper()
	token := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_DIRECT_PORT_TOKEN")
	profileName := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_DIRECT_PORT_PROFILE")
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		fixture.baseURL, token, "scenario-tenant", "scenario-subject", fixture.httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioDirectPortIngress{client: client, profileName: profileName}
}

func createScenarioDirectPortSession(
	t *testing.T,
	ctx context.Context,
	ingress scenarioDirectPortIngress,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	key string,
) contracts.PortSession {
	t.Helper()
	var session contracts.PortSession
	if err := ingress.client.RequestJSON(
		ctx,
		"createSandboxPortSession",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID},
			Headers:        scenarioLeaseHeaders(handle, leaseID, uniqueScenarioKey(t, key)),
			Body: scenarioBodyWithoutTestFailure(
				// The same duration as the relay baseline, and inside the Lease
				// that admitted it, so the comparison isolates the transport.
				contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
			),
		},
		&session,
	); err != nil {
		t.Fatalf("SecondBox scenario create direct PortSession: %v", err)
	}
	return session
}

// scenarioDataPlanePollInterval reads the deployed relay interval so the
// assertion compares against the configured baseline rather than a constant.
func scenarioDataPlanePollInterval(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS"))
	if raw == "" {
		t.Fatal("SecondBox scenario requires SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS")
	}
	milliseconds, err := time.ParseDuration(raw + "ms")
	if err != nil || milliseconds <= 0 {
		t.Fatalf("data-plane poll interval = %q", raw)
	}
	return milliseconds
}
