//go:build linux

package gvisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// newAttributedComputeQualification starts one ordinary gVisor Instance whose
// assignment permits attributed execs through a Runner-side gateway socket.
func newAttributedComputeQualification(t *testing.T) (qualificationFixture, *net.UnixListener) {
	t.Helper()
	qualificationBuild(t)
	directory, err := os.MkdirTemp("/run", "attr-gw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	configPath := filepath.Join(directory, "contexts.json")
	content := fmt.Sprintf(`{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"tenant-a","gateways":[{"logicalName":"tools.internal","attributedSocket":%q}]}]}`, gateway.Addr().String())
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	contexts, err := networkpolicy.LoadEgressContextConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newQualificationFixtureWithNetworkPolicy(t, "attributed", networkpolicy.RunnerConfig{
		CompileOptions: networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute},
		DNSUpstream:    netip.MustParseAddrPort("127.0.0.1:53"), EgressContexts: contexts,
	})
	backend, assignment := fixture.backend, fixture.command
	readiness, err := backend.Readiness(t.Context())
	if err != nil || !readiness.Capabilities.GetPerExecAttributionReady() {
		t.Fatalf("attributed readiness: %+v %v", readiness, err)
	}
	assignment.EgressContext = "tenant-a"
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "per-exec-attribution")
	assignment.AttributedExecutionPermission = &runnerprotocol.AttributedExecutionPermission{Gateway: "tools.internal", MaximumConnections: 2}
	if _, err := backend.StartAssignment(t.Context(), assignment, func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := backend.MarkAssignmentReady(fixture.fence); err != nil {
		t.Fatal(err)
	}
	return fixture, gateway
}

func attributedQualificationExec(reference string, lifetime time.Duration, script string) (*runnerprotocol.ExecOpen, time.Time) {
	expiry := time.Now().Add(lifetime)
	return &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", script}}},
		Cwd:     ".", DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
		AttributedExecution: &runnerprotocol.AttributedExecution{
			TenantRef: "tenant-a", SubjectRef: "owner-a", AuthorizationRef: reference, ExpiresAtUnixMs: uint64(expiry.UnixMilli()),
		},
	}, expiry
}

func ordinaryQualificationExec(t *testing.T, backend *AssignmentBackend, fence *runnerprotocol.AssignmentFence, script string) *runnerprotocol.ExecTerminal {
	t.Helper()
	result, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", script}}},
		Cwd:     ".", DeadlineUnixMs: uint64(time.Now().Add(20 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Terminal
}

// Sequential attributed execs share one ordinary Instance: each gets its own
// listener and identity, a background daemon survives, and a closed listener
// is unreachable.
func TestQualifiedGVisorAttributedExecWindows(t *testing.T) {
	fixture, gateway := newAttributedComputeQualification(t)
	backend := fixture.backend
	t.Cleanup(func() {
		if err := backend.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if terminal := ordinaryQualificationExec(t, backend, fixture.fence, `nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 & echo $! > daemon.pid`); terminal.GetExitCode() != 0 {
		t.Fatalf("daemon start: %+v", terminal)
	}
	for _, reference := range []string{"authorization-1", "authorization-2"} {
		execRequest, expiry := attributedQualificationExec(reference, time.Minute,
			`endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf %s "$endpoint" > gateway-`+reference+`; printf 'guest-context: forged\n' | nc -w 2 "${endpoint%:*}" "${endpoint##*:}"`)
		result := make(chan error, 1)
		go func() {
			connection, err := gateway.AcceptUnix()
			if err != nil {
				result <- err
				return
			}
			defer connection.Close()
			attribution, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry)
			if err == nil && (attribution.AssignmentID != fixture.fence.AssignmentId || attribution.SubjectRef != "owner-a" || attribution.AuthorizationRef != reference || attribution.Generation != 1) {
				err = fmt.Errorf("incorrect host attribution: %+v", attribution)
			}
			if err == nil {
				err = connection.SetDeadline(expiry)
			}
			request := make([]byte, len("guest-context: forged\n"))
			if err == nil {
				_, err = io.ReadFull(connection, request)
			}
			if err == nil && string(request) != "guest-context: forged\n" {
				err = fmt.Errorf("wrong guest bytes: %q", request)
			}
			if err == nil {
				_, err = connection.Write([]byte(reference))
			}
			result <- err
		}()
		executed, err := backend.ExecuteBuffered(t.Context(), fixture.fence, execRequest)
		if err != nil || executed.Terminal.GetExitCode() != 0 || string(executed.Stdout) != reference {
			t.Fatalf("attributed command: %v stdout=%q stderr=%q terminal=%+v", err, executed.Stdout, executed.Stderr, executed.Terminal)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if terminal := ordinaryQualificationExec(t, backend, fixture.fence, `endpoint=$(cat gateway-`+reference+`); nc -z -w 2 "${endpoint%:*}" "${endpoint##*:}"`); terminal.GetExitCode() == 0 {
			t.Fatalf("closed attributed listener for %s stayed reachable", reference)
		}
	}
	if terminal := ordinaryQualificationExec(t, backend, fixture.fence, `test "$(cat gateway-authorization-1)" != "$(cat gateway-authorization-2)"`); terminal.GetExitCode() != 0 {
		t.Fatal("sequential attributed execs shared one listener")
	}
	if terminal := ordinaryQualificationExec(t, backend, fixture.fence, `kill -0 "$(cat daemon.pid)"`); terminal.GetExitCode() != 0 {
		t.Fatal("background daemon did not survive attributed execs")
	}
	select {
	case terminal := <-backend.InstanceTerminals():
		t.Fatalf("attributed execs terminated the Instance: %+v", terminal)
	default:
	}
}

// Revoking a window closes an active relay held by a descendant process; the
// Instance keeps running unless it is fenced.
func TestQualifiedGVisorAttributedRevocation(t *testing.T) {
	for _, trigger := range []string{"exec-end", "expiry", "fence"} {
		t.Run(trigger, func(t *testing.T) {
			fixture, gateway := newAttributedComputeQualification(t)
			backend := fixture.backend
			t.Cleanup(func() {
				if err := backend.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			lifetime, script := time.Minute, `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`
			switch trigger {
			case "exec-end":
				script = `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & sleep 3`
			case "expiry":
				lifetime = 8 * time.Second
			}
			execRequest, expiry := attributedQualificationExec("authorization-"+trigger, lifetime, script)
			if err := gateway.SetDeadline(expiry); err != nil {
				t.Fatal(err)
			}
			commandDone := make(chan struct{})
			go func() {
				defer close(commandDone)
				_, _ = backend.ExecuteBuffered(t.Context(), fixture.fence, execRequest)
			}()
			connection, err := gateway.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(connection, ready); err != nil || string(ready) != "ready" {
				t.Fatalf("descendant readiness: %q %v", ready, err)
			}
			if trigger == "fence" {
				stopped, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{Fence: fixture.fence, DeadlineUnixMs: uint64(time.Now().Add(10 * time.Second).UnixMilli())})
				if err != nil || stopped.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED {
					t.Fatalf("active attributed teardown: %+v %v", stopped, err)
				}
			}
			if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("gateway connection survived revocation: bytes=%d error=%v", n, err)
			}
			select {
			case <-commandDone:
			case <-time.After(15 * time.Second):
				t.Fatal("attributed exec did not return after revocation")
			}
			if trigger != "fence" {
				if terminal := ordinaryQualificationExec(t, backend, fixture.fence, "true"); terminal.GetExitCode() != 0 {
					t.Fatalf("Instance stopped serving after %s: %+v", trigger, terminal)
				}
			}
		})
	}
}
