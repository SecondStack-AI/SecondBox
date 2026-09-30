package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type windowEvents struct {
	mu     sync.Mutex
	events []string
}

func (recorder *windowEvents) record(event string) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *windowEvents) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.events)
}

type fakeExecForwarder struct {
	events  *windowEvents
	address netip.AddrPort
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	err     error
}

func (forwarder *fakeExecForwarder) ListenerAddress() netip.AddrPort { return forwarder.address }
func (forwarder *fakeExecForwarder) Done() <-chan struct{}           { return forwarder.done }
func (forwarder *fakeExecForwarder) Wait() error {
	<-forwarder.done
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	return forwarder.err
}

func (forwarder *fakeExecForwarder) stop(err error) {
	forwarder.once.Do(func() {
		forwarder.mu.Lock()
		forwarder.err = err
		forwarder.mu.Unlock()
		close(forwarder.done)
	})
}

func (forwarder *fakeExecForwarder) Revoke() {
	forwarder.events.record("revoke listener")
	forwarder.stop(context.Canceled)
}

func (forwarder *fakeExecForwarder) Close(context.Context) error {
	forwarder.events.record("remove listener table")
	return nil
}

type fakeListenerEnforcer struct {
	events    *windowEvents
	revokeErr error
}

func (enforcer *fakeListenerEnforcer) AllowExecutionListener(_ context.Context, instanceID string, listener netip.AddrPort) error {
	enforcer.events.record("allow " + instanceID + " " + listener.String())
	return nil
}

func (enforcer *fakeListenerEnforcer) FenceExecutionListeners(string) {}

func (enforcer *fakeListenerEnforcer) RevokeExecutionListener(_ context.Context, instanceID string, listener netip.AddrPort) error {
	enforcer.events.record("revoke " + instanceID + " " + listener.String())
	return enforcer.revokeErr
}

func testAttributedExecConfig(enforcer ExecutionListenerPolicyEnforcer) AttributedExecWindowConfig {
	expiresAt := time.Now().Add(time.Minute)
	return AttributedExecWindowConfig{
		NFTPath: "/usr/sbin/nft", Policy: enforcer, PolicyInstanceID: "policy-instance",
		Gateway: AttributedExecGateway{Socket: "/run/gateway.sock", MaximumConnections: 3},
		Fence: &runnerprotocol.AssignmentFence{
			AssignmentId: "assignment", SandboxId: "sandbox", InstanceId: "instance", SandboxGeneration: 4,
		},
		Open: &runnerprotocol.ExecOpen{
			DeadlineUnixMs: uint64(expiresAt.Add(-time.Second).UnixMilli()),
			AttributedExecution: &runnerprotocol.AttributedExecution{
				TenantRef: "tenant", SubjectRef: "subject", AuthorizationRef: "command",
				ExpiresAtUnixMs: uint64(expiresAt.UnixMilli()),
			},
		},
		Listener: egressforwarder.ExecutionListenerPolicy{
			GuestInterface: "tap0", GuestAddress: netip.MustParseAddr("10.0.0.2"),
			ListenerAddress: netip.MustParseAddrPort("10.0.0.1:0"),
		},
	}
}

func openFakeExecWindow(t *testing.T, config AttributedExecWindowConfig, events *windowEvents) (*AttributedExecWindow, *fakeExecForwarder, egressattribution.ExecutionAttribution) {
	t.Helper()
	forwarder := &fakeExecForwarder{events: events, address: netip.MustParseAddrPort("10.0.0.1:41000"), done: make(chan struct{})}
	var started AttributedExecWindowConfig
	var attribution egressattribution.ExecutionAttribution
	window, err := openAttributedExecWindowWithForwarder(t.Context(), config,
		func(_ context.Context, config AttributedExecWindowConfig, admitted egressattribution.ExecutionAttribution) (attributedExecForwarder, error) {
			started, attribution = config, admitted
			return forwarder, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if started.Listener.InstanceID != "instance" || started.Gateway.MaximumConnections != 3 {
		t.Fatalf("forwarder configuration = %+v", started)
	}
	return window, forwarder, attribution
}

func TestAttributedExecWindowRevokesListenerBeforeRulesWithoutStoppingInstance(t *testing.T) {
	events := &windowEvents{}
	window, _, attribution := openFakeExecWindow(t, testAttributedExecConfig(&fakeListenerEnforcer{events: events}), events)
	want := egressattribution.ExecutionAttribution{
		TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance",
		AssignmentID: "assignment", Generation: 4, AuthorizationRef: "command", ExpiresAt: attribution.ExpiresAt,
	}
	if attribution != want || attribution.ExpiresAt.IsZero() {
		t.Fatalf("SBXATTR1 attribution = %+v", attribution)
	}
	if window.Gateway() != netip.MustParseAddrPort("10.0.0.1:41000") || window.Context().Err() != nil {
		t.Fatalf("open window gateway=%s context=%v", window.Gateway(), window.Context().Err())
	}
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if got := events.snapshot(); !slices.Equal(got, []string{
		"allow policy-instance 10.0.0.1:41000", "revoke listener",
		"revoke policy-instance 10.0.0.1:41000", "remove listener table",
	}) {
		t.Fatalf("window lifecycle = %q", got)
	}
	if err := window.Close(); err != nil {
		t.Fatalf("repeated close = %v", err)
	}
}

func TestAttributedExecWindowForwarderFailureCancelsExec(t *testing.T) {
	events := &windowEvents{}
	window, forwarder, _ := openFakeExecWindow(t, testAttributedExecConfig(&fakeListenerEnforcer{events: events}), events)
	failure := errors.New("gateway socket refused")
	forwarder.stop(failure)
	select {
	case <-window.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("forwarder failure did not cancel the exec")
	}
	if !errors.Is(context.Cause(window.Context()), failure) {
		t.Fatalf("exec cancellation cause = %v", context.Cause(window.Context()))
	}
	deadline := time.Now().Add(time.Second)
	for !slices.Contains(events.snapshot(), "remove listener table") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !slices.Contains(events.snapshot(), "revoke policy-instance 10.0.0.1:41000") {
		t.Fatalf("failed window kept its rules until the exec ended: %q", events.snapshot())
	}
	if err := window.Close(); !errors.Is(err, failure) {
		t.Fatalf("window close = %v", err)
	}
}

func TestAttributedExecWindowExpiryRevokesWithoutFailingExec(t *testing.T) {
	events := &windowEvents{}
	window, forwarder, _ := openFakeExecWindow(t, testAttributedExecConfig(&fakeListenerEnforcer{events: events}), events)
	forwarder.stop(context.DeadlineExceeded)
	deadline := time.Now().Add(time.Second)
	for !slices.Contains(events.snapshot(), "remove listener table") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !slices.Contains(events.snapshot(), "revoke policy-instance 10.0.0.1:41000") {
		t.Fatalf("expired window kept its rules: %q", events.snapshot())
	}
	if window.Context().Err() != nil {
		t.Fatal("expiry cancelled the exec")
	}
	if err := window.Close(); err != nil {
		t.Fatalf("expired window close = %v", err)
	}
}

// A cancelled or overdue exec loses its gateway before the guest returns.
func TestAttributedExecWindowRevokesOnCancellationAndDeadline(t *testing.T) {
	waitForRevocation := func(t *testing.T, events *windowEvents) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !slices.Contains(events.snapshot(), "remove listener table") && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := events.snapshot(); !slices.Equal(got[1:], []string{
			"revoke listener", "revoke policy-instance 10.0.0.1:41000", "remove listener table",
		}) {
			t.Fatalf("window was not revoked while the exec was outstanding: %q", got)
		}
	}
	t.Run("cancellation", func(t *testing.T) {
		events := &windowEvents{}
		execCtx, cancelExec := context.WithCancel(t.Context())
		config := testAttributedExecConfig(&fakeListenerEnforcer{events: events})
		forwarder := &fakeExecForwarder{events: events, address: netip.MustParseAddrPort("10.0.0.1:41000"), done: make(chan struct{})}
		window, err := openAttributedExecWindowWithForwarder(execCtx, config,
			func(context.Context, AttributedExecWindowConfig, egressattribution.ExecutionAttribution) (attributedExecForwarder, error) {
				return forwarder, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		cancelExec()
		waitForRevocation(t, events)
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		events := &windowEvents{}
		config := testAttributedExecConfig(&fakeListenerEnforcer{events: events})
		config.Open.DeadlineUnixMs = uint64(time.Now().Add(50 * time.Millisecond).UnixMilli())
		window, _, _ := openFakeExecWindow(t, config, events)
		waitForRevocation(t, events)
		if window.Context().Err() != nil {
			t.Fatal("the exec deadline cancelled the exec; the guest reports its own deadline")
		}
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAttributedExecWindowReportsFailedRuleRevocation(t *testing.T) {
	events := &windowEvents{}
	revokeErr := errors.New("nft update failed")
	window, _, _ := openFakeExecWindow(t, testAttributedExecConfig(&fakeListenerEnforcer{events: events, revokeErr: revokeErr}), events)
	if err := window.Close(); !errors.Is(err, revokeErr) {
		t.Fatalf("window close = %v", err)
	}
}

func TestAttributedExecWindowRefusesInvalidBinding(t *testing.T) {
	for name, change := range map[string]func(*AttributedExecWindowConfig){
		"PTY": func(config *AttributedExecWindowConfig) { config.Open.AllocatePty = true },
		"deadline": func(config *AttributedExecWindowConfig) {
			config.Open.DeadlineUnixMs = config.Open.AttributedExecution.ExpiresAtUnixMs + 1
		},
		"expired": func(config *AttributedExecWindowConfig) {
			config.Open.AttributedExecution.ExpiresAtUnixMs = uint64(time.Now().UnixMilli())
		},
		"tenant":           func(config *AttributedExecWindowConfig) { config.Open.AttributedExecution.TenantRef = "" },
		"authorization":    func(config *AttributedExecWindowConfig) { config.Open.AttributedExecution.AuthorizationRef = "" },
		"Instance policy":  func(config *AttributedExecWindowConfig) { config.Policy = nil },
		"ordinary exec":    func(config *AttributedExecWindowConfig) { config.Open.AttributedExecution = nil },
		"missing instance": func(config *AttributedExecWindowConfig) { config.PolicyInstanceID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			config := testAttributedExecConfig(&fakeListenerEnforcer{events: &windowEvents{}})
			change(&config)
			if _, err := openAttributedExecWindowWithForwarder(t.Context(), config,
				func(context.Context, AttributedExecWindowConfig, egressattribution.ExecutionAttribution) (attributedExecForwarder, error) {
					t.Fatal("refused binding started a forwarder")
					return nil, nil
				}); err == nil {
				t.Fatal("invalid attributed binding opened a window")
			}
		})
	}
}

func TestRunAttributedExecOpensWindowOnlyWhenTheExecRuns(t *testing.T) {
	opened := false
	result, err := RunAttributedExec(t.Context(), &runnerprotocol.ExecOpen{},
		func(context.Context) (*AttributedExecWindow, error) {
			opened = true
			return nil, errors.New("unexpected")
		},
		func(_ context.Context, openGateway ExecutionGatewayOpener) (string, error) {
			if openGateway != nil {
				t.Fatal("ordinary exec received a gateway opener")
			}
			return "ordinary", nil
		})
	if err != nil || result != "ordinary" || opened {
		t.Fatalf("ordinary exec result=%q opened=%v error=%v", result, opened, err)
	}

	// A queued exec that never reaches the guest opens no window.
	result, err = RunAttributedExec(t.Context(), testAttributedExecConfig(nil).Open,
		func(context.Context) (*AttributedExecWindow, error) {
			t.Fatal("a queued exec opened its window")
			return nil, nil
		},
		func(context.Context, ExecutionGatewayOpener) (string, error) {
			return "", errors.New("guest session closed while queued")
		})
	if err == nil || result != "" {
		t.Fatalf("queued exec result=%q error=%v", result, err)
	}

	events := &windowEvents{}
	config := testAttributedExecConfig(&fakeListenerEnforcer{events: events})
	result, err = RunAttributedExec(t.Context(), config.Open,
		func(context.Context) (*AttributedExecWindow, error) {
			window, _, _ := openFakeExecWindow(t, config, events)
			return window, nil
		},
		func(ctx context.Context, openGateway ExecutionGatewayOpener) (string, error) {
			if len(events.snapshot()) != 0 {
				t.Fatalf("window opened before the exec owned the guest session: %q", events.snapshot())
			}
			windowCtx, gateway, err := openGateway(ctx)
			if err != nil || windowCtx.Err() != nil || gateway != netip.MustParseAddrPort("10.0.0.1:41000") || slices.Contains(events.snapshot(), "revoke listener") {
				t.Fatalf("attributed exec gateway=%s error=%v events=%q", gateway, err, events.snapshot())
			}
			return "attributed", nil
		})
	if err != nil || result != "attributed" || !slices.Contains(events.snapshot(), "remove listener table") {
		t.Fatalf("attributed exec result=%q error=%v events=%q", result, err, events.snapshot())
	}
}

// staticExecutionGateway hands an already-open listener to a guest exec, or
// nothing for an ordinary exec.
func staticExecutionGateway(gateway netip.AddrPort) ExecutionGatewayOpener {
	if gateway == (netip.AddrPort{}) {
		return nil
	}
	return func(ctx context.Context) (context.Context, netip.AddrPort, error) { return ctx, gateway, nil }
}
