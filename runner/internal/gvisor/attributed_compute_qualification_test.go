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
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func newAttributedComputeQualification(t *testing.T, lifetime time.Duration) (qualificationFixture, *net.UnixListener, time.Time) {
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
	if err != nil || !readiness.Capabilities.GetAttributedExecutionReady() {
		t.Fatalf("attributed readiness: %+v %v", readiness, err)
	}
	expiry := time.Now().Add(lifetime)
	assignment.EgressContext = "tenant-a"
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
	assignment.AttributedExecution = &runnerprotocol.AttributedExecution{TenantRef: "tenant-a", SubjectRef: "owner-a", AuthorizationRef: "authorization-a", Gateway: "tools.internal", MaximumConnections: 2, ExpiresAtUnixMs: uint64(expiry.UnixMilli())}
	if _, err := backend.StartAssignment(t.Context(), assignment, func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := backend.MarkAssignmentReady(fixture.fence); err != nil {
		t.Fatal(err)
	}
	return fixture, gateway, expiry
}

func TestQualifiedGVisorAttributedCommand(t *testing.T) {
	fixture, gateway, expiry := newAttributedComputeQualification(t, time.Minute)
	backend := fixture.backend
	t.Cleanup(func() {
		if err := backend.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	result := make(chan error, 1)
	go func() {
		connection, err := gateway.AcceptUnix()
		if err != nil {
			result <- err
			return
		}
		defer connection.Close()
		attribution, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry)
		if err == nil && (attribution.AssignmentID != fixture.fence.AssignmentId || attribution.SubjectRef != "owner-a" || attribution.AuthorizationRef != "authorization-a" || attribution.Generation != 1) {
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
			_, err = connection.Write([]byte("qualified-attributed"))
		}
		result <- err
	}()
	execRequest := &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", `endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf 'guest-context: forged\n' | nc -w 2 "${endpoint%:*}" "${endpoint##*:}"`}}},
		Cwd:     ".", DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
	}
	executed, err := backend.ExecuteBuffered(t.Context(), fixture.fence, execRequest)
	if err != nil || executed.Terminal.GetExitCode() != 0 || string(executed.Stdout) != "qualified-attributed" {
		t.Fatalf("attributed command: %v stdout=%q stderr=%q terminal=%+v", err, executed.Stdout, executed.Stderr, executed.Terminal)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ExecuteBuffered(t.Context(), fixture.fence, execRequest); err == nil {
		t.Fatal("second command admitted in attributed generation")
	}
	stopped, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{Fence: fixture.fence, DeadlineUnixMs: uint64(time.Now().Add(10 * time.Second).UnixMilli())})
	if err != nil || stopped.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED {
		t.Fatalf("attributed teardown: %+v %v", stopped, err)
	}
}

func TestQualifiedGVisorAttributedRevocation(t *testing.T) {
	for _, trigger := range []string{"expiry", "fence", "supervisor-loss"} {
		t.Run(trigger, func(t *testing.T) {
			lifetime := time.Minute
			if trigger == "expiry" {
				lifetime = 8 * time.Second
			}
			fixture, gateway, expiry := newAttributedComputeQualification(t, lifetime)
			backend := fixture.backend
			t.Cleanup(func() {
				err := backend.Shutdown(context.Background())
				if trigger == "supervisor-loss" {
					if err == nil || !strings.Contains(err.Error(), "supervisor teardown reported failure: signal: killed") {
						t.Errorf("supervisor loss must report unconfirmed workspace cleanup: %v", err)
					}
				} else if err != nil {
					t.Error(err)
				}
			})
			if err := gateway.SetDeadline(expiry); err != nil {
				t.Fatal(err)
			}
			commandDone := make(chan struct{})
			go func() {
				defer close(commandDone)
				// The descendant keeps an established connection open until host teardown.
				_, _ = backend.ExecuteBuffered(t.Context(), fixture.fence, &runnerprotocol.ExecOpen{
					Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`}}},
					Cwd:     ".", DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
				})
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
			switch trigger {
			case "fence":
				stopped, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{Fence: fixture.fence, DeadlineUnixMs: uint64(time.Now().Add(10 * time.Second).UnixMilli())})
				if err != nil || stopped.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED {
					t.Fatalf("active attributed teardown: %+v %v", stopped, err)
				}
			case "supervisor-loss":
				backend.mu.Lock()
				active := backend.assignments[fixture.fence.AssignmentId]
				backend.mu.Unlock()
				if err := syscallKillGroup(active.handles.Command.Process.Pid); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("gateway connection survived revocation: bytes=%d error=%v", n, err)
			}
			select {
			case <-commandDone:
			case <-time.After(15 * time.Second):
				t.Fatal("attributed descendant command survived compute teardown")
			}
			if trigger != "fence" {
				select {
				case terminal := <-backend.InstanceTerminals():
					if !sameFence(terminal.Fence, fixture.fence) {
						t.Fatalf("wrong revoked instance terminal: %+v", terminal)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("no compute terminal after attributed revocation")
				}
			}
		})
	}
}
