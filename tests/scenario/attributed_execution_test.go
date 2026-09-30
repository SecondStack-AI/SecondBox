//go:build scenario_live && linux

package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/egressattribution"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// scenarioAttributedGateway is the gateway logical name the scenario Runner
// routes to the attributed socket in every egress context.
const scenarioAttributedGateway = "execution.secondbox.internal"

// newScenarioAttributedSandbox creates a running Sandbox from the shared Profile
// that permits attributed execution, and binds the Runner-side gateway socket
// the scenario Runner forwards attributed connections to.
func newScenarioAttributedSandbox(t *testing.T, suffix string) (scenarioFixture, *secondboxclient.SandboxHandle, *net.UnixListener) {
	t.Helper()
	return newScenarioAttributedSandboxFromProfile(t, "scenario-attributed", contracts.SandboxDesiredStateRunning, scenarioAttributedGateway, suffix)
}

// Tests that name the same Profile must request an identical spec, because
// Profile creation replays by name.
func newScenarioAttributedSandboxFromProfile(t *testing.T, profileName, desiredState, gatewayName, suffix string) (scenarioFixture, *secondboxclient.SandboxHandle, *net.UnixListener) {
	t.Helper()
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "microsandbox" {
		t.Skip("Microsandbox does not support attributed execution")
	}
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, desiredState)
	*spec.Network.RequiresTenantEgressContext = true
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{Gateway: gatewayName, MaximumConnections: 4}
	spec.Ports = []contracts.PortPolicy{{Name: "web", Port: 8080, Protocol: "tcp", MaximumSessions: 1, MaximumSessionSeconds: 30}}
	profile := createScenarioProfile(t, fixture, profileName, spec)
	handle, created := createScenarioSandbox(t, fixture, profile, suffix)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_ATTRIBUTED_GATEWAY_DIR"), "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	return fixture, handle, gateway
}

func scenarioAttributedExecRequest(command, reference string, expiresAt time.Time) secondboxclient.BufferedExecRequest {
	request := scenarioExecRequest(command, 1024)
	request.AttributedExecution = &contracts.AttributedExecutionRequest{AuthorizationRef: reference, ExpiresAt: expiresAt}
	request.DeadlineMilliseconds = time.Until(expiresAt).Milliseconds() - 1000
	return request
}

type scenarioGatewayConnection struct {
	attribution egressattribution.ExecutionAttribution
	connection  *net.UnixConn
	firstLine   string
}

// acceptScenarioGateway accepts one forwarded connection and reads its SBXATTR1
// identity and the first guest line.
func acceptScenarioGateway(t *testing.T, gateway *net.UnixListener, expiresAt time.Time) scenarioGatewayConnection {
	t.Helper()
	if err := gateway.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := gateway.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	attribution, err := egressattribution.ReadRunnerExecutionAttribution(connection, 0, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var line strings.Builder
	for {
		var next [1]byte
		if _, err := io.ReadFull(connection, next[:]); err != nil {
			t.Fatalf("attributed guest line after %q: %v", line.String(), err)
		}
		if next[0] == '\n' {
			break
		}
		line.WriteByte(next[0])
	}
	return scenarioGatewayConnection{attribution: attribution, connection: connection, firstLine: line.String()}
}

func assertScenarioExecAttribution(t *testing.T, actual egressattribution.ExecutionAttribution, sandbox contracts.Sandbox, reference string) {
	t.Helper()
	if actual.TenantRef != os.Getenv("SECONDBOX_SCENARIO_TENANT_REF") || actual.SubjectRef != os.Getenv("SECONDBOX_SCENARIO_SUBJECT_REF") ||
		actual.SandboxID != sandbox.ID || sandbox.Instance == nil || actual.InstanceID != sandbox.Instance.ID ||
		actual.Generation != sandbox.Generation || actual.AuthorizationRef != reference || actual.AssignmentID == "" {
		t.Fatalf("attributed connection identity = %+v; want Sandbox %+v and reference %q", actual, sandbox, reference)
	}
}

// assertScenarioGatewayIdle proves that no connection reaches the gateway.
func assertScenarioGatewayIdle(t *testing.T, gateway *net.UnixListener, wait time.Duration) {
	t.Helper()
	if err := gateway.SetDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	connection, err := gateway.AcceptUnix()
	if err == nil {
		connection.Close()
		t.Fatal("a revoked attributed listener forwarded a connection")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("idle gateway check: %v", err)
	}
}

// scenarioAttributedDaemon runs from before the first attributed exec until the
// test ends. It reaches the first exec's gateway inside the window, which is
// the accepted residual risk, and records that it cannot reach it afterwards.
const scenarioAttributedDaemon = `while [ ! -s endpoint-1 ]; do sleep 0.1; done
endpoint=$(cat endpoint-1)
printf 'daemon-context\n' | nc -w 10 "${endpoint%:*}" "${endpoint##*:}" > daemon-reply
touch daemon-done
while [ ! -e window-closed ]; do sleep 0.1; done
if printf 'daemon-after\n' | nc -w 2 "${endpoint%:*}" "${endpoint##*:}" >/dev/null 2>&1; then echo reached > daemon-after; else echo refused > daemon-after; fi
while :; do sleep 1; done
`

func TestScenarioAttributedExecsShareOrdinaryInstance(t *testing.T) {
	fixture, handle, gateway := newScenarioAttributedSandbox(t, "attributed-shared")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	if ready.Instance == nil {
		t.Fatal("ordinary start returned no Instance")
	}
	writeScenarioFile(t, ctx, fixture.subject, handle, "daemon.sh", []byte(scenarioAttributedDaemon))
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle,
		`nohup sh daemon.sh >/dev/null 2>&1 & echo $! > daemon.pid`, 1024, "attributed-daemon"), 0, "", "")

	// The first exec waits for the daemon's connection so that it lands inside
	// the exec's window.
	firstExpiry := time.Now().Add(50 * time.Second)
	first := make(chan secondboxclient.ExecOutcome, 1)
	firstErr := make(chan error, 1)
	go func() {
		outcome, err := handle.Execute(ctx, scenarioAttributedExecRequest(`endpoint=$SECONDBOX_EXECUTION_GATEWAY
printf %s "$endpoint" > endpoint-1
for attempt in $(seq 1 200); do [ -e daemon-done ] && break; sleep 0.1; done
test -e daemon-done || exit 42
printf 'guest-context: exec\n' | nc -w 10 "${endpoint%:*}" "${endpoint##*:}"`, "command-1", firstExpiry), uniqueScenarioKey(t, "attributed-first"), "")
		first <- outcome
		firstErr <- err
	}()
	for _, want := range []struct{ line, reply string }{{"daemon-context", "daemon-reply"}, {"guest-context: exec", "exec-reply"}} {
		accepted := acceptScenarioGateway(t, gateway, firstExpiry)
		assertScenarioExecAttribution(t, accepted.attribution, ready, "command-1")
		if accepted.firstLine != want.line {
			t.Fatalf("attributed connection order: got %q, want %q", accepted.firstLine, want.line)
		}
		if _, err := accepted.connection.Write([]byte(want.reply)); err != nil {
			t.Fatal(err)
		}
		if err := accepted.connection.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	assertScenarioExited(t, <-first, 0, "exec-reply", "")

	// The listener is revoked with the exec: the daemon and any later exec
	// cannot reach it, and ordinary execs receive no execution gateway.
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle, `endpoint=$(cat endpoint-1)
if printf probe | nc -w 2 "${endpoint%:*}" "${endpoint##*:}" >/dev/null 2>&1; then exit 1; fi
touch window-closed
printf '%s|%s' "${SECONDBOX_EXECUTION_GATEWAY-absent}" "$(cat daemon-reply)"`, 1024, "attributed-after-window"), 0, "absent|daemon-reply", "")
	assertScenarioGatewayIdle(t, gateway, 4*time.Second)
	after := executeScenarioCommand(t, ctx, handle, `for attempt in $(seq 1 100); do [ -s daemon-after ] && break; sleep 0.1; done; cat daemon-after`, 1024, "attributed-daemon-after")
	assertScenarioExited(t, after, 0, "refused\n", "")

	secondExpiry := time.Now().Add(45 * time.Second)
	second := make(chan secondboxclient.ExecOutcome, 1)
	go func() {
		outcome, err := handle.Execute(ctx, scenarioAttributedExecRequest(`endpoint=$SECONDBOX_EXECUTION_GATEWAY
printf %s "$endpoint" > endpoint-2
printf 'guest-context: second\n' | nc -w 10 "${endpoint%:*}" "${endpoint##*:}"`, "command-2", secondExpiry), uniqueScenarioKey(t, "attributed-second"), "")
		if err != nil {
			t.Error(err)
		}
		second <- outcome
	}()
	accepted := acceptScenarioGateway(t, gateway, secondExpiry)
	assertScenarioExecAttribution(t, accepted.attribution, ready, "command-2")
	if _, err := accepted.connection.Write([]byte("second-reply")); err != nil {
		t.Fatal(err)
	}
	if err := accepted.connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	assertScenarioExited(t, <-second, 0, "second-reply", "")
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle,
		`test "$(cat endpoint-1)" != "$(cat endpoint-2)" && kill -0 "$(cat daemon.pid)" && printf same-instance`, 1024, "attributed-distinct"), 0, "same-instance", "")

	// Attributed execs neither retire nor restrict the ordinary generation.
	current, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contracts.SandboxStateReady || current.Generation != ready.Generation || current.Instance == nil || current.Instance.ID != ready.Instance.ID {
		t.Fatalf("attributed execs changed compute: before=%+v after=%+v", ready, current)
	}
	if content, err := handle.ReadFile(ctx, "endpoint-2", 1024, ""); err != nil || len(content) == 0 {
		t.Fatalf("file read on the attributed generation: %q %v", content, err)
	}
	lease := acquireScenarioLease(t, ctx, fixture, handle, 60, "attributed-lease")
	terminalSession, err := handle.CreateTerminal(ctx, secondboxclient.CreateTerminalRequest{
		Command:     secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{Mode: "shell", Command: "printf terminal-ready; sleep 5"}},
		Environment: secondboxclient.StringMap{}, Rows: 24, Columns: 80, DeadlineMilliseconds: 20000,
	}, uniqueScenarioKey(t, "attributed-terminal"), lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := handle.ConnectTerminal(ctx, terminalSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := terminal.GrantOutput(65536); err != nil {
		t.Fatal(err)
	}
	requireScenarioTerminalText(t, terminal, "terminal-ready")
	port := createScenarioPortSession(t, ctx, fixture, handle, lease.ID, "attributed-port")
	if port.State != contracts.PortSessionStateOpen || port.Generation != ready.Generation {
		t.Fatalf("Port session on the attributed generation = %+v", port)
	}
}

// Deadline expiry, cancellation, and control-plane loss each revoke the window
// and close a relay a descendant still holds, while the Instance keeps running.
func TestScenarioAttributedWindowRevocation(t *testing.T) {
	for _, trigger := range []string{"expiry", "cancel", "control-plane"} {
		t.Run(trigger, func(t *testing.T) {
			fixture, handle, gateway := newScenarioAttributedSandbox(t, "attributed-"+trigger)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
			lifetime := 50 * time.Second
			if trigger == "expiry" {
				lifetime = 10 * time.Second
			}
			expiresAt := time.Now().Add(lifetime)
			held := `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf 'held\n'; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`
			session, err := handle.CreateExecStream(ctx, secondboxclient.StreamingExecRequest{
				Command:     secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{Mode: "shell", Command: held}},
				Environment: secondboxclient.StringMap{}, DeadlineMilliseconds: time.Until(expiresAt).Milliseconds() - 1000,
				MaximumOutputBytes: 4096, WindowBytes: 4096,
				AttributedExecution: &contracts.AttributedExecutionRequest{AuthorizationRef: "held-" + trigger, ExpiresAt: expiresAt},
			}, uniqueScenarioKey(t, "attributed-held"), "")
			if err != nil {
				t.Fatal(err)
			}
			stream, err := handle.ConnectExecStream(ctx, session, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			accepted := acceptScenarioGateway(t, gateway, expiresAt)
			assertScenarioExecAttribution(t, accepted.attribution, ready, "held-"+trigger)
			switch trigger {
			case "cancel":
				scenarioJSON[secondboxclient.ExecStreamSession](t, ctx, fixture.subject, "cancelSandboxExecStream", secondboxclient.CallOptions{
					PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID), "execSessionId": string(session.ID)},
					Headers: func() http.Header {
						headers := handle.GenerationHeaders("")
						headers.Set("Idempotency-Key", uniqueScenarioKey(t, "attributed-cancel"))
						return headers
					}(),
				})
			case "control-plane":
				// The relay must close while the control plane is down, so only the
				// Runner's disconnect handling can have revoked it.
				scenarioCompose(t, "stop", "control-plane")
				t.Cleanup(func() { scenarioCompose(t, "start", "control-plane") })
			}
			// Cancellation and control loss must close the relay well before the
			// exec deadline and expiry could.
			revokedBy := expiresAt.Add(-10 * time.Second)
			if trigger == "expiry" {
				revokedBy = time.Now().Add(30 * time.Second)
			}
			if err := accepted.connection.SetReadDeadline(revokedBy); err != nil {
				t.Fatal(err)
			}
			if n, err := accepted.connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("attributed relay survived %s: bytes=%d error=%v", trigger, n, err)
			}
			if trigger == "control-plane" {
				scenarioCompose(t, "start", "control-plane")
				waitForScenarioControlPlaneReady(t, fixture, 60*time.Second)
				waitForScenarioRunner(t, fixture, 90*time.Second)
			}
			current := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
			// A control-plane outage longer than the scenario Runner heartbeat
			// timeout (5s) lets the control plane retire the generation as Runner
			// loss, so only expiry and cancellation prove the Instance survives.
			// That retirement needs the reconnected Runner, after the relay closed.
			if trigger != "control-plane" &&
				(current.Generation != ready.Generation || current.Instance == nil || current.Instance.ID != ready.Instance.ID) {
				t.Fatalf("attributed revocation on %s changed compute: before=%+v after=%+v", trigger, ready, current)
			}
			// The Runner reconnects after the control plane restarts; until then
			// data-plane admission reports the execution node unavailable.
			for {
				outcome, err := handle.Execute(ctx, scenarioExecRequest("printf still-running", 1024), uniqueScenarioKey(t, "attributed-after-"+trigger), "")
				if err == nil {
					assertScenarioExited(t, outcome, 0, "still-running", "")
					break
				}
				switch secondboxclient.ProblemCodeOf(err) {
				case string(secondboxclient.ProblemCodeExecutionNodeUnavailable):
				case string(secondboxclient.ProblemCodeGenerationFenced):
					if trigger != "control-plane" {
						t.Fatal(err)
					}
					if _, err := handle.Refresh(ctx); err != nil {
						t.Fatal(err)
					}
					waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
				default:
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatalf("Runner did not reconnect after %s: %v", trigger, err)
				case <-time.After(500 * time.Millisecond):
				}
			}
		})
	}
}

func TestScenarioAttributedConcurrentSandboxesKeepOwnAuthority(t *testing.T) {
	fixture, first, gateway := newScenarioAttributedSandbox(t, "attributed-concurrent-first")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	firstReady := waitForSandbox(t, ctx, first, secondboxclient.SandboxStateReady)
	second, created := createScenarioSandbox(t, fixture, contracts.Profile{Name: firstReady.Profile}, "attributed-concurrent-second")
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	type command struct {
		handle *secondboxclient.SandboxHandle
		ready  contracts.Sandbox
		done   chan secondboxclient.ExecOutcome
		conn   *net.UnixConn
	}
	commands := map[string]*command{
		"concurrent-first":  {handle: first, ready: firstReady, done: make(chan secondboxclient.ExecOutcome, 1)},
		"concurrent-second": {handle: second, ready: waitForSandbox(t, ctx, second, secondboxclient.SandboxStateReady), done: make(chan secondboxclient.ExecOutcome, 1)},
	}
	expiresAt := time.Now().Add(50 * time.Second)
	for reference, command := range commands {
		key := uniqueScenarioKey(t, reference)
		go func() {
			outcome, err := command.handle.Execute(ctx, scenarioAttributedExecRequest(
				`endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf 'guest-context: other-account\n' | nc -w 30 "${endpoint%:*}" "${endpoint##*:}"`,
				reference, expiresAt), key, "")
			if err != nil {
				t.Error(err)
			}
			command.done <- outcome
		}()
	}
	for range commands {
		accepted := acceptScenarioGateway(t, gateway, expiresAt)
		command := commands[accepted.attribution.AuthorizationRef]
		if command == nil || command.conn != nil || accepted.firstLine != "guest-context: other-account" {
			t.Fatalf("concurrent attributed connection mixed authority: %+v %q", accepted.attribution, accepted.firstLine)
		}
		assertScenarioExecAttribution(t, accepted.attribution, command.ready, accepted.attribution.AuthorizationRef)
		command.conn = accepted.connection
	}
	for _, reference := range []string{"concurrent-first", "concurrent-second"} {
		command := commands[reference]
		if _, err := command.conn.Write([]byte(reference)); err != nil {
			t.Fatal(err)
		}
		if err := command.conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		assertScenarioExited(t, <-command.done, 0, reference, "")
	}
}

// Invalid bindings are refused at admission and leave the generation usable.
func TestScenarioAttributedExecAdmissionRefusals(t *testing.T) {
	fixture, handle, _ := newScenarioAttributedSandbox(t, "attributed-refusals")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	refused := func(expiresAt time.Time, deadlineMilliseconds int64) secondboxclient.BufferedExecRequest {
		request := scenarioAttributedExecRequest("true", "refused", expiresAt)
		request.DeadlineMilliseconds = deadlineMilliseconds
		return request
	}
	for name, request := range map[string]secondboxclient.BufferedExecRequest{
		"expired":                 refused(time.Now().Add(-time.Second), 1000),
		"beyond Profile deadline": refused(time.Now().Add(2*time.Minute), 1000),
		"deadline after expiry":   refused(time.Now().Add(10*time.Second), 20000),
	} {
		if _, err := handle.Execute(ctx, request, uniqueScenarioKey(t, "refused"), ""); err == nil {
			t.Fatalf("%s attribution was admitted", name)
		} else {
			assertScenarioAPIError(t, err, http.StatusBadRequest, "invalid_request")
		}
	}
	unpermittedProfile := createScenarioProfile(t, fixture, "scenario-lifecycle", scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning))
	unpermitted, created := createScenarioSandbox(t, fixture, unpermittedProfile, "attributed-unpermitted")
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	waitForSandbox(t, ctx, unpermitted, secondboxclient.SandboxStateReady)
	if _, err := unpermitted.Execute(ctx, scenarioAttributedExecRequest("true", "unpermitted", time.Now().Add(20*time.Second)), uniqueScenarioKey(t, "unpermitted"), ""); err == nil {
		t.Fatal("a Profile without attributed permission admitted an attributed exec")
	} else {
		assertScenarioAPIError(t, err, http.StatusBadRequest, "invalid_request")
	}
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle, "printf usable", 1024, "after-refusals"), 0, "usable", "")
}

// A Profile whose attributed gateway has no route on the home Runner fails its
// start before compute. Public lifecycle intents must still progress without
// manual database repair.
func TestScenarioStartupFailureLifecycleRecovery(t *testing.T) {
	for _, action := range []string{"stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			fixture, handle, _ := newScenarioAttributedSandboxFromProfile(t, "scenario-attributed-unrouted", contracts.SandboxDesiredStateStopped, "unrouted.secondbox.internal", "unrouted-"+action)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
			start := requestScenarioLifecycle(t, ctx, handle, "unrouted-start", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Start(ctx, secondboxclient.StartSandboxRequest{}, options)
			})
			_, err := fixture.subject.WaitOperation(ctx, start.ID, 100*time.Millisecond)
			var failure *secondboxclient.OperationFailure
			if !errors.As(err, &failure) || failure.Operation.State != contracts.OperationStateFailed || failure.Operation.Error == nil {
				t.Fatalf("unrouted gateway start error=%v", err)
			}
			failed := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateFailed)
			recovery := requestScenarioLifecycle(t, ctx, handle, "recover-"+action, func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				if action == "stop" {
					return handle.Stop(ctx, options)
				}
				return handle.Delete(ctx, options)
			})
			waitForScenarioOperation(t, ctx, fixture.subject, recovery)
			wanted := secondboxclient.SandboxStateStopped
			if action == "delete" {
				wanted = secondboxclient.SandboxStateDeleted
			}
			recovered := waitForSandbox(t, ctx, handle, wanted)
			if recovered.Generation < failed.Generation || recovered.Instance != nil {
				t.Fatalf("recovery after startup failure: before=%+v after=%+v", failed, recovered)
			}
		})
	}
}

// Numeric connection policy applies when the next Assignment is committed.
// Each attributed exec holds its admitted streams, and one more is refused.
func TestScenarioAttributedConnectionPolicyAdoptsNextGeneration(t *testing.T) {
	const profileName = "scenario-attributed-connections"
	fixture, handle, gateway := newScenarioAttributedSandboxFromProfile(t, profileName, contracts.SandboxDesiredStateStopped, scenarioAttributedGateway, "attributed-connections")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	stopped := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	pinned := stopped.ProfileRevisionID
	profile, err := fixture.admin.GetProfile(ctx, profileName)
	if err != nil {
		t.Fatal(err)
	}
	spec := profile.CurrentRevision.Spec
	permission := *spec.AttributedExecution
	permission.MaximumConnections = 128
	spec.AttributedExecution = &permission
	spec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{MaximumConnections: 4096}
	if _, err := fixture.admin.ReviseProfile(ctx, profile.Name, profile.Revision, secondboxclient.ReviseProfileRequest{Spec: spec}, uniqueScenarioKey(t, "connection-grant")); err != nil {
		t.Fatal(err)
	}
	tenant, subject := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TENANT_REF"), requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SUBJECT_REF")
	credential, err := fixture.admin.CreateTenantControllerAuthority(ctx, tenant, secondboxclient.CreateTenantControllerAuthorityRequest{ExpiresAt: time.Now().Add(time.Hour), Metadata: map[string]string{}}, uniqueScenarioKey(t, "connection-controller"))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := secondboxclient.NewSecondBoxTenantControllerClient(fixture.baseURL, credential.BearerToken, fixture.httpClient)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := controller.GetSubjectSandboxPolicy(ctx, subject, profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	selection := contracts.SubjectSandboxPolicy{Profile: profile.Name, Lifecycle: contracts.SandboxLifecycleLimits{IdleSeconds: policy.Effective.IdleSeconds, MaximumDurationSeconds: policy.Effective.MaximumDurationSeconds}}
	for _, count := range []int{8, 3} {
		selection.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: int64(count)}
		policy, err = controller.UpdateSubjectSandboxPolicy(ctx, subject, selection, policy.Revision, uniqueScenarioKey(t, "connection-selection"))
		if err != nil || policy.AttributedExecution.MaximumConnections != int64(count) {
			t.Fatalf("connection selection: %+v %v", policy, err)
		}
		// The selection applies to the next Assignment only.
		operation := requestScenarioLifecycle(t, ctx, handle, "connection-start", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Start(ctx, secondboxclient.StartSandboxRequest{}, options)
		})
		waitForScenarioOperation(t, ctx, fixture.subject, operation)
		ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
		if ready.ProfileRevisionID != pinned {
			t.Fatal("numeric policy changed Sandbox pin")
		}
		expiresAt := time.Now().Add(45 * time.Second)
		reference := fmt.Sprintf("connections-%d", count)
		result := make(chan error, 1)
		go func() {
			result <- serveScenarioConnectionLimit(gateway, ready, reference, tenant, subject, count, expiresAt)
		}()
		script := fmt.Sprintf(`set -eu
endpoint=$SECONDBOX_EXECUTION_GATEWAY
work=$(mktemp -d)
pids=""
trap 'for pid in $pids; do kill "$pid" 2>/dev/null || :; done; rm -rf "$work"' EXIT
wait_line() {
  for attempt in $(seq 1 100); do
    if grep -qx "$1" "$2"; then return 0; fi
    sleep 0.05
  done
  return 1
}
for i in $(seq 1 %d); do
  mkfifo "$work/in-$i"
  exec 3<>"$work/in-$i"
  timeout 20 nc "${endpoint%%%%:*}" "${endpoint##*:}" <&3 >"$work/out-$i" 2>/dev/null &
  pids="$pids $!"
  exec 3>&-
done
for i in $(seq 1 %[1]d); do
  wait_line ready "$work/out-$i"
done
timeout 3 nc "${endpoint%%%%:*}" "${endpoint##*:}" </dev/null >"$work/extra-out" 2>/dev/null
test ! -s "$work/extra-out"
for i in $(seq 1 %[1]d); do printf ping >"$work/in-$i"; done
for i in $(seq 1 %[1]d); do wait_line ok "$work/out-$i"; printf ok; done`, count)
		outcome, err := handle.Execute(ctx, scenarioAttributedExecRequest(script, reference, expiresAt), uniqueScenarioKey(t, "connection-exec"), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		assertScenarioExited(t, outcome, 0, strings.Repeat("ok", count), "")
		stop := requestScenarioLifecycle(t, ctx, handle, "connection-stop", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Stop(ctx, options)
		})
		waitForScenarioOperation(t, ctx, fixture.subject, stop)
		waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	}
	selection.AttributedExecution = nil
	if _, err := controller.UpdateSubjectSandboxPolicy(ctx, subject, selection, policy.Revision, uniqueScenarioKey(t, "connection-inherit")); err != nil {
		t.Fatal(err)
	}
}

// serveScenarioConnectionLimit accepts exactly count attributed streams, then
// proves the excess stream never reached the gateway.
func serveScenarioConnectionLimit(gateway *net.UnixListener, ready contracts.Sandbox, reference, tenant, subject string, count int, expiresAt time.Time) error {
	if err := gateway.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	var conns []*net.UnixConn
	defer func() {
		for _, conn := range conns {
			conn.Close()
		}
	}()
	for range count {
		conn, err := gateway.AcceptUnix()
		if err != nil {
			return err
		}
		conns = append(conns, conn)
		if err := conn.SetDeadline(expiresAt); err != nil {
			return err
		}
		actual, err := egressattribution.ReadRunnerExecutionAttribution(conn, 0, expiresAt)
		if err != nil {
			return err
		}
		if actual.SandboxID != ready.ID || actual.Generation != ready.Generation || actual.AuthorizationRef != reference || actual.TenantRef != tenant || actual.SubjectRef != subject {
			return fmt.Errorf("connection attribution crossed exec: %+v", actual)
		}
	}
	for _, conn := range conns {
		if _, err := conn.Write([]byte("ready\n")); err != nil {
			return err
		}
	}
	for _, conn := range conns {
		// The guest sends this only after the bounded excess-connection probe
		// finishes. Refusal must not revoke admitted streams.
		ping := make([]byte, len("ping"))
		if _, err := io.ReadFull(conn, ping); err != nil || string(ping) != "ping" {
			return fmt.Errorf("admitted stream after refusal: %q, %v", ping, err)
		}
	}
	// Netcat variants differ in stdin EOF handling. Independently prove that
	// the extra connection never reached the gateway.
	if err := gateway.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	extra, err := gateway.AcceptUnix()
	if err == nil {
		extra.Close()
		return errors.New("connection above selected limit reached gateway")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		return fmt.Errorf("excess gateway connection check: %w", err)
	}
	for _, conn := range conns {
		if _, err := conn.Write([]byte("ok\n")); err != nil {
			return err
		}
	}
	return nil
}
