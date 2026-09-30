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
		"PTY":              func(config *AttributedExecWindowConfig) { config.Open.AllocatePty = true },
		"deadline":         func(config *AttributedExecWindowConfig) { config.Open.DeadlineUnixMs = config.Open.AttributedExecution.ExpiresAtUnixMs + 1 },
		"expired":          func(config *AttributedExecWindowConfig) { config.Open.AttributedExecution.ExpiresAtUnixMs = uint64(time.Now().UnixMilli()) },
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

func TestRunAttributedExecBindsGatewayOnlyToAttributedOpen(t *testing.T) {
	opened := false
	result, err := RunAttributedExec(t.Context(), &runnerprotocol.ExecOpen{},
		func(context.Context) (*AttributedExecWindow, error) { opened = true; return nil, errors.New("unexpected") },
		func(_ context.Context, gateway netip.AddrPort) (string, error) {
			if gateway.IsValid() {
				t.Fatalf("ordinary exec received gateway %s", gateway)
			}
			return "ordinary", nil
		})
	if err != nil || result != "ordinary" || opened {
		t.Fatalf("ordinary exec result=%q opened=%v error=%v", result, opened, err)
	}
	events := &windowEvents{}
	config := testAttributedExecConfig(&fakeListenerEnforcer{events: events})
	result, err = RunAttributedExec(t.Context(), config.Open,
		func(context.Context) (*AttributedExecWindow, error) {
			window, _, _ := openFakeExecWindow(t, config, events)
			return window, nil
		},
		func(_ context.Context, gateway netip.AddrPort) (string, error) {
			if gateway != netip.MustParseAddrPort("10.0.0.1:41000") || slices.Contains(events.snapshot(), "revoke listener") {
				t.Fatalf("attributed exec gateway=%s events=%q", gateway, events.snapshot())
			}
			return "attributed", nil
		})
	if err != nil || result != "attributed" || !slices.Contains(events.snapshot(), "remove listener table") {
		t.Fatalf("attributed exec result=%q error=%v events=%q", result, err, events.snapshot())
	}
}
