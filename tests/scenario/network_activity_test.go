//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"os"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

func TestScenarioPortSessionsLeasesAndGenerationFencing(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Ports = []contracts.PortPolicy{{
		Name:                  "web",
		Port:                  8080,
		Protocol:              "tcp",
		MaximumSessions:       1,
		MaximumSessionSeconds: 30,
	}}
	profile := createScenarioProfile(t, fixture, "scenario-port-lease", spec)
	handle, _ := createScenarioSandbox(t, fixture, profile, "port-lease")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)

	lease := acquireScenarioLease(t, ctx, fixture, handle, 30, "lease-acquire")
	if lease.SandboxID != ready.ID ||
		lease.Generation != ready.Generation ||
		lease.State != contracts.LeaseStateActive {
		t.Fatalf("SecondBox scenario acquired Lease = %#v", lease)
	}
	gotLease := scenarioJSON[contracts.Lease](
		t,
		ctx,
		fixture.subject,
		"getSandboxLease",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"leaseId": lease.ID},
		},
	)
	if gotLease.ID != lease.ID || gotLease.State != contracts.LeaseStateActive {
		t.Fatalf("SecondBox scenario get Lease = %#v", gotLease)
	}
	secondLeaseErr := createScenarioLease(
		ctx,
		fixture,
		handle,
		30,
		uniqueScenarioKey(t, "lease-second-acquire"),
	)
	assertScenarioAPIError(t, secondLeaseErr, http.StatusConflict, "state_conflict")

	renewed := scenarioJSON[contracts.Lease](
		t,
		ctx,
		fixture.subject,
		"renewSandboxLease",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"leaseId": lease.ID},
			Headers:        scenarioHeaders(uniqueScenarioKey(t, "lease-renew")),
			Body:           scenarioBody(t, contracts.RenewLeaseRequest{DurationSeconds: 45}),
		},
	)
	if renewed.ID != lease.ID ||
		renewed.State != contracts.LeaseStateActive ||
		!renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("SecondBox scenario renewed Lease = %#v, prior %#v", renewed, lease)
	}

	writeScenarioFile(
		t,
		ctx,
		fixture.subject,
		handle,
		"port-response.txt",
		[]byte("SecondBox real guest port response\n"),
	)
	serverCommand := `nohup python3 -m http.server 8080 --directory /workspace >/workspace/port-server.log 2>&1 </dev/null & python3 -c 'import socket, time
for _ in range(50):
    try:
        socket.create_connection(("127.0.0.1", 8080), 1).close()
        raise SystemExit(0)
    except OSError:
        time.sleep(0.1)
raise SystemExit(1)'`
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "microsandbox" {
		serverCommand = `nohup sh -c 'while :; do { printf "HTTP/1.0 200 OK\r\nContent-Length: 35\r\nConnection: close\r\n\r\n"; cat /workspace/port-response.txt; } | nc -l -p 8080; done' >/workspace/port-server.log 2>&1 </dev/null & sleep 1`
	}
	server := executeScenarioCommand(
		t,
		ctx,
		handle,
		serverCommand,
		4096,
		"port-http-server",
	)
	assertScenarioExited(t, server, 0, "", "")

	session := createScenarioPortSession(t, ctx, fixture, handle, renewed.ID, "port-create")
	if session.SandboxID != ready.ID ||
		session.Generation != ready.Generation ||
		session.Name != "web" ||
		session.Protocol != "tcp" ||
		session.State != contracts.PortSessionStateOpen ||
		session.Endpoint == "" {
		t.Fatalf("SecondBox scenario PortSession = %#v", session)
	}
	gotSession := scenarioJSON[contracts.PortSession](
		t,
		ctx,
		fixture.subject,
		"getSandboxPortSession",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{
				"sandboxId":     ready.ID,
				"portSessionId": session.ID,
			},
		},
	)
	if gotSession.ID != session.ID || gotSession.State != contracts.PortSessionStateOpen {
		t.Fatalf("SecondBox scenario get PortSession = %#v", gotSession)
	}
	_, err := requestScenarioPortSession(
		ctx,
		fixture,
		handle,
		renewed.ID,
		uniqueScenarioKey(t, "port-over-limit"),
	)
	assertScenarioAPIError(t, err, http.StatusTooManyRequests, "quota_exceeded")

	connection := dialScenarioPortTunnel(t, ctx, session.Endpoint)
	if err := connection.WriteMessage(
		websocket.BinaryMessage,
		[]byte("GET /port-response.txt HTTP/1.1\r\nHost: scenario\r\nConnection: close\r\n\r\n"),
	); err != nil {
		t.Fatal(err)
	}
	response := readScenarioPortResponse(t, connection)
	if !bytes.Contains(response, []byte("HTTP/1.0 200 OK")) ||
		!bytes.Contains(response, []byte("SecondBox real guest port response")) {
		t.Fatalf("SecondBox scenario guest HTTP response = %q", response)
	}

	scenarioVoid(
		t,
		ctx,
		fixture.subject,
		"closeSandboxPortSession",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{
				"sandboxId":     ready.ID,
				"portSessionId": session.ID,
			},
			Headers: scenarioHeaders(uniqueScenarioKey(t, "port-close")),
		},
	)
	waitForScenarioPortState(
		t,
		ctx,
		fixture,
		ready.ID,
		session.ID,
		contracts.PortSessionStateClosed,
	)
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("SecondBox scenario closed PortSession kept its tunnel open")
	}
	connection.Close()

	released := scenarioJSON[contracts.Lease](
		t,
		ctx,
		fixture.subject,
		"releaseSandboxLease",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"leaseId": lease.ID},
			Headers:        scenarioHeaders(uniqueScenarioKey(t, "lease-release")),
		},
	)
	if released.ID != lease.ID || released.State != contracts.LeaseStateReleased {
		t.Fatalf("SecondBox scenario released Lease = %#v", released)
	}

	staleHeaders := handle.GenerationHeaders("")
	stopScenarioSandbox(t, ctx, fixture, handle, "port-generation-stop")
	startScenarioSandbox(t, ctx, fixture, handle, "port-generation-start")
	var ping contracts.PingResult
	err = fixture.subject.RequestJSON(
		ctx,
		"pingSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": ready.ID},
			Headers:        staleHeaders,
		},
		&ping,
	)
	assertScenarioAPIError(t, err, http.StatusConflict, "generation_fenced")
}

func acquireScenarioLease(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	durationSeconds int64,
	key string,
) contracts.Lease {
	t.Helper()
	var lease contracts.Lease
	if err := createScenarioLease(
		ctx,
		fixture,
		handle,
		durationSeconds,
		uniqueScenarioKey(t, key),
		&lease,
	); err != nil {
		t.Fatalf("SecondBox scenario acquire Lease: %v", err)
	}
	return lease
}

func createScenarioLease(
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	durationSeconds int64,
	key string,
	destinations ...*contracts.Lease,
) error {
	var lease contracts.Lease
	err := fixture.subject.RequestJSON(
		ctx,
		"acquireSandboxLease",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID},
			Headers:        scenarioLeaseHeaders(handle, "", key),
			Body: scenarioBodyWithoutTestFailure(
				contracts.AcquireLeaseRequest{DurationSeconds: durationSeconds},
			),
		},
		&lease,
	)
	if err == nil && len(destinations) > 0 {
		*destinations[0] = lease
	}
	return err
}

func scenarioBodyWithoutTestFailure(value any) io.Reader {
	body, err := secondboxclient.EncodeJSONBody(value)
	if err != nil {
		panic(err)
	}
	return body
}

func scenarioLeaseHeaders(
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	key string,
) http.Header {
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", key)
	return headers
}

func createScenarioPortSession(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	key string,
) contracts.PortSession {
	t.Helper()
	session, err := requestScenarioPortSession(
		ctx,
		fixture,
		handle,
		leaseID,
		uniqueScenarioKey(t, key),
	)
	if err != nil {
		t.Fatalf("SecondBox scenario create PortSession: %v", err)
	}
	return session
}

func requestScenarioPortSession(
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	key string,
) (contracts.PortSession, error) {
	var session contracts.PortSession
	err := fixture.subject.RequestJSON(
		ctx,
		"createSandboxPortSession",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID},
			Headers:        scenarioLeaseHeaders(handle, leaseID, key),
			Body: scenarioBodyWithoutTestFailure(
				contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
			),
		},
		&session,
	)
	return session, err
}

func dialScenarioPortTunnel(
	t *testing.T,
	ctx context.Context,
	endpoint string,
) *websocket.Conn {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Fragment == "" {
		t.Fatalf("SecondBox scenario PortSession endpoint = %q, error=%v", endpoint, err)
	}
	token := parsed.Fragment
	parsed.Fragment = ""
	dialer := websocket.Dialer{
		Subprotocols: []string{"secondbox.port.v1", "secondbox.port.token." + token},
	}
	connection, response, err := dialer.DialContext(ctx, parsed.String(), nil)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf(
				"SecondBox scenario Port tunnel status=%d body=%s: %v",
				response.StatusCode,
				body,
				err,
			)
		}
		t.Fatal(err)
	}
	return connection
}

func readScenarioPortResponse(t *testing.T, connection *websocket.Conn) []byte {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response []byte
	for !bytes.Contains(response, []byte("SecondBox real guest port response")) {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("SecondBox scenario read Port tunnel: %v; response=%q", err, response)
		}
		if messageType != websocket.BinaryMessage {
			t.Fatalf("SecondBox scenario Port tunnel message type = %d", messageType)
		}
		response = append(response, payload...)
		if len(response) > 1<<20 {
			t.Fatal("SecondBox scenario Port tunnel response exceeded 1 MiB")
		}
	}
	return response
}

func waitForScenarioPortState(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	sandboxID string,
	sessionID string,
	state string,
) contracts.PortSession {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		session := scenarioJSON[contracts.PortSession](
			t,
			ctx,
			fixture.subject,
			"getSandboxPortSession",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{
					"sandboxId":     sandboxID,
					"portSessionId": sessionID,
				},
			},
		)
		if session.State == state {
			return session
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario PortSession %s state = %s, want %s",
				session.ID,
				session.State,
				state,
			)
		case <-ticker.C:
		}
	}
}

func TestScenarioNetworkPolicyDenyAndAllowList(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	denyProfile := createScenarioProfile(
		t,
		fixture,
		"scenario-network-deny",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	denied, _ := createScenarioSandbox(t, fixture, denyProfile, "network-deny")
	waitForSandbox(t, ctx, denied, secondboxclient.SandboxStateReady)
	networkProbe := "curl --silent --show-error --connect-timeout 3 --max-time 5 http://example.com/ >/dev/null"
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "microsandbox" {
		networkProbe = "wget -q -T 5 -O /dev/null http://example.com/"
	}
	deniedOutcome := executeScenarioCommand(
		t,
		ctx,
		denied,
		networkProbe,
		4096,
		"network-denied",
	)
	if deniedOutcome.ExecExited == nil || deniedOutcome.ExecExited.ExitCode == 0 {
		t.Fatalf("SecondBox scenario deny-all network outcome = %#v", deniedOutcome)
	}
	stopScenarioSandbox(t, ctx, fixture, denied, "network-deny-stop")

	allowSpec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	allowSpec.Network = contracts.NetworkPolicy{
		Mode: "allow_list",
		Destinations: []contracts.NetworkDestination{{
			Protocol: "http",
			Domain:   "example.com",
			Port:     80,
		}},
	}
	allowProfile := createScenarioProfile(t, fixture, "scenario-network-allow", allowSpec)
	allowed, _ := createScenarioSandbox(t, fixture, allowProfile, "network-allow")
	waitForSandbox(t, ctx, allowed, secondboxclient.SandboxStateReady)
	allowedOutcome := executeScenarioCommand(
		t,
		ctx,
		allowed,
		networkProbe,
		16<<10,
		"network-allowed",
	)
	if allowedOutcome.ExecExited == nil ||
		allowedOutcome.ExecExited.ExitCode != 0 {
		t.Fatalf(
			"SecondBox scenario allow-list network outcome = %s",
			describeScenarioExecOutcome(allowedOutcome),
		)
	}
}

func TestScenarioIsolatedAndNetworkEnabledProfilesRemainFencedConcurrently(t *testing.T) {
	if backend := os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND"); backend != "" && backend != "firecracker" {
		// The network-enabled egress gateway listens on the Runner's host
		// guest bridge, which only the Firecracker backend creates; the
		// Microsandbox and gVisor backends have no host bridge to bind
		// the gateway on.
		t.Skip("SecondBox scenario network-enabled gateway requires the Firecracker host guest bridge")
	}
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	bridgeAddress := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_BRIDGE_ADDRESS")
	const gatewayPort = int64(18443)
	const gatewayContext = "network-enabled-egress-context"
	gatewayAddress := net.JoinHostPort(bridgeAddress, strconv.FormatInt(gatewayPort, 10))
	managementAddress := net.JoinHostPort(bridgeAddress, "18444")
	startScenarioNetworkTarget(t, gatewayAddress, gatewayContext)
	startScenarioNetworkTarget(t, managementAddress, "management-network")

	runtimeDigest := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST")
	toolchainDigest := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST")
	isolatedLineage, err := standardresources.ProfileLineage(standardresources.AgentCompartmentIsolated, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	isolatedProfile := createScenarioProfile(t, fixture, standardresources.AgentCompartmentIsolated, isolatedLineage.Revisions[0].Spec)

	networkLineage, err := standardresources.ProfileLineage(standardresources.AgentCompartment, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	networkSpec := networkLineage.Revisions[len(networkLineage.Revisions)-1].Spec
	networkSpec.Network = contracts.NetworkPolicy{Mode: "allow_list", Destinations: []contracts.NetworkDestination{{Protocol: "http", CIDR: "1.1.1.1/32", Port: 80}}}
	networkProfile := createScenarioProfile(t, fixture, "scenario-agent-compartment-network-enabled", networkSpec)

	isolated, _ := createScenarioSandbox(t, fixture, isolatedProfile, "isolated-network-policy")
	networkEnabled, _ := createScenarioSandbox(t, fixture, networkProfile, "network-enabled-policy")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, isolated, secondboxclient.SandboxStateReady)
	waitForSandbox(t, ctx, networkEnabled, secondboxclient.SandboxStateReady)

	probes := []struct {
		name    string
		key     string
		command string
	}{
		{name: "configured Runner gateway", key: "runner-gateway", command: "curl --silent --show-error --connect-timeout 2 --max-time 4 http://" + gatewayAddress},
		{name: "management network", key: "management-network", command: "curl --silent --show-error --connect-timeout 2 --max-time 4 http://" + managementAddress},
		{name: "metadata endpoint", key: "metadata-endpoint", command: "curl --silent --show-error --connect-timeout 2 --max-time 4 http://169.254.169.254/latest/meta-data/"},
		{name: "DNS resolver", key: "dns-resolver", command: `python3 -c 'import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(3); s.sendto(bytes.fromhex("000101000001000000000000076578616d706c6503636f6d0000010001"),("` + bridgeAddress + `",53)); s.recvfrom(512)'`},
		{name: "arbitrary Internet", key: "internet", command: "curl --silent --show-error --connect-timeout 2 --max-time 4 http://1.1.1.1/cdn-cgi/trace"},
	}
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			outcome := executeScenarioCommand(t, ctx, isolated, probe.command, 4096, "isolated-"+probe.key)
			if outcome.ExecExited == nil || outcome.ExecExited.ExitCode == 0 {
				t.Fatalf("isolated %s probe = %s", probe.name, describeScenarioExecOutcome(outcome))
			}
			if strings.Contains(decodeScenarioOutput(t, outcome.ExecExited.Output.StdoutBase64), gatewayContext) {
				t.Fatalf("isolated %s probe obtained the network-enabled gateway context", probe.name)
			}
		})
	}

	enabledOutcome := executeScenarioCommand(t, ctx, networkEnabled, "curl --silent --show-error --connect-timeout 2 --max-time 4 http://1.1.1.1/cdn-cgi/trace | tee /workspace/network-response >/dev/null", 4096, "network-enabled-internet")
	assertScenarioExited(t, enabledOutcome, 0, "", "")
	enabledResponse := executeScenarioCommand(t, ctx, networkEnabled, "test -s /workspace/network-response", 4096, "network-enabled-response-file")
	assertScenarioExited(t, enabledResponse, 0, "", "")
	isolationOutcome := executeScenarioCommand(t, ctx, isolated, "test ! -e /workspace/network-response", 4096, "isolated-network-response-file")
	assertScenarioExited(t, isolationOutcome, 0, "", "")
}

func startScenarioNetworkTarget(t *testing.T, address string, response string) {
	t.Helper()
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("SecondBox scenario listen on %s: %v", address, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(writer, response); err != nil {
			t.Errorf("SecondBox scenario write network target response: %v", err)
		}
	})}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("SecondBox scenario serve network target %s: %v", address, serveErr)
		}
	}()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			t.Errorf("SecondBox scenario stop network target %s: %v", address, err)
		}
	})
}

func TestScenarioTouchExtendsIdleExpiry(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)

	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Lifecycle.IdleSeconds = 15
	spec.Lifecycle.DrainGraceSeconds = 1
	spec.Lifecycle.MaximumDurationSeconds = 120
	profile := createScenarioProfile(t, fixture, "scenario-touch-idle", spec)
	handle, _ := createScenarioSandbox(t, fixture, profile, "touch-idle")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	if ready.LastActivityAt == nil {
		t.Fatal("SecondBox scenario ready Sandbox omitted lastActivityAt")
	}
	initialActivity := ready.LastActivityAt.UTC()
	originalExpiry := initialActivity.Add(time.Duration(spec.Lifecycle.IdleSeconds) * time.Second)
	waitForScenarioTime(t, ctx, initialActivity.Add(8*time.Second))

	touchHeaders := handle.GenerationHeaders("")
	touchHeaders.Set("Idempotency-Key", uniqueScenarioKey(t, "touch-idle-extend"))
	touch := scenarioJSON[contracts.TouchResult](
		t,
		ctx,
		fixture.subject,
		"touchSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": ready.ID},
			Headers:        touchHeaders,
		},
	)
	if touch.SandboxID != ready.ID ||
		touch.Generation != ready.Generation ||
		!touch.LastActivityAt.After(initialActivity) {
		t.Fatalf(
			"SecondBox scenario touch result = %#v, initial activity %s",
			touch,
			initialActivity,
		)
	}

	waitForScenarioTime(t, ctx, originalExpiry.Add(time.Second))
	extended, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatalf("SecondBox scenario refresh after original idle expiry: %v", err)
	}
	if extended.State != secondboxclient.SandboxStateReady ||
		extended.Generation != ready.Generation ||
		extended.LastActivityAt == nil ||
		!extended.LastActivityAt.Truncate(time.Microsecond).
			Equal(touch.LastActivityAt.Truncate(time.Microsecond)) {
		t.Fatalf(
			"SecondBox scenario Sandbox after original idle expiry = %#v, touch %#v",
			extended,
			touch,
		)
	}

	extendedExpiry := touch.LastActivityAt.Add(time.Duration(spec.Lifecycle.IdleSeconds) * time.Second)
	waitForScenarioTime(t, ctx, extendedExpiry.Add(-500*time.Millisecond))
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh awaiting idle expiry: %v", refreshErr)
		}
		if (current.State == secondboxclient.SandboxStateDraining ||
			current.State == secondboxclient.SandboxStateStopping) &&
			current.Instance != nil &&
			current.Instance.TerminationReason == contracts.TerminationReasonIdleTimeout {
			return
		}
		if current.Generation != ready.Generation {
			t.Fatalf(
				"SecondBox scenario idle expiry advanced generation before termination was observed: %#v",
				current,
			)
		}
		if current.State == secondboxclient.SandboxStateFailed {
			t.Fatalf("SecondBox scenario idle Sandbox failed: %#v", current)
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario idle expiry was not observed; last Sandbox = %#v: %v",
				current,
				ctx.Err(),
			)
		case <-ticker.C:
		}
	}
}

func waitForScenarioTime(t *testing.T, ctx context.Context, target time.Time) {
	t.Helper()
	delay := time.Until(target)
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatalf("SecondBox scenario timed wait for %s: %v", target, ctx.Err())
	case <-timer.C:
	}
}
