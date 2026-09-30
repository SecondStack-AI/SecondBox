package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// attributedExecForwarder is one exec's listener and relays.
type attributedExecForwarder interface {
	ListenerAddress() netip.AddrPort
	Done() <-chan struct{}
	Wait() error
	Revoke()
	Close(context.Context) error
}

// AttributedExecGateway is an Assignment's attributed route, resolved when the
// Assignment starts: the Runner-local gateway socket and the per-exec bound.
type AttributedExecGateway struct {
	Socket             string
	MaximumConnections int
}

// ResolveAttributedExecGateway resolves an Assignment's permitted gateway inside
// its pinned egress context, failing closed when this Runner has no attributed
// route for it. Nil means the Profile permits no attributed execution.
func ResolveAttributedExecGateway(contexts networkpolicy.EgressContextConfig, assignment *runnerprotocol.AssignmentCommand) (*AttributedExecGateway, error) {
	permission := assignment.GetAttributedExecutionPermission()
	if permission == nil {
		return nil, nil
	}
	socket, err := contexts.AttributedGatewaySocket(assignment.EgressContext, permission.Gateway)
	if err != nil {
		return nil, err
	}
	return &AttributedExecGateway{Socket: socket, MaximumConnections: int(permission.MaximumConnections)}, nil
}

// AttributedExecWindowConfig opens one exec's forwarder on the Runner side of
// its Instance interface. PolicyInstanceID keys the installed Instance policy.
type AttributedExecWindowConfig struct {
	NFTPath          string
	Policy           ExecutionListenerPolicyEnforcer
	PolicyInstanceID string
	Gateway          AttributedExecGateway
	Listener         egressforwarder.ExecutionListenerPolicy
	Fence            *runnerprotocol.AssignmentFence
	Open             *runnerprotocol.ExecOpen
}

// AttributedExecWindow is the lifetime of one attributed exec's gateway. Any
// process in the Instance can reach the listener while the window is open; the
// window closes when the exec ends or is cancelled, at its deadline or expiry,
// or when its forwarder fails.
type AttributedExecWindow struct {
	policy           ExecutionListenerPolicyEnforcer
	policyInstanceID string
	forwarder        attributedExecForwarder
	ctx              context.Context
	cancel           context.CancelCauseFunc
	revokeOnce       sync.Once
	revokeErr        error
	monitored        chan struct{}
	failure          error
	// stopRevokers releases the cancellation and deadline revocation hooks.
	stopRevokers func()
}

func OpenAttributedExecWindow(ctx context.Context, config AttributedExecWindowConfig) (*AttributedExecWindow, error) {
	return openAttributedExecWindowWithForwarder(ctx, config, startAttributedExecForwarder)
}

func openAttributedExecWindowWithForwarder(
	ctx context.Context,
	config AttributedExecWindowConfig,
	startForwarder func(context.Context, AttributedExecWindowConfig, egressattribution.ExecutionAttribution) (attributedExecForwarder, error),
) (*AttributedExecWindow, error) {
	attribution, err := attributedExecAttribution(config.Fence, config.Open)
	if err != nil {
		return nil, err
	}
	if config.Policy == nil || config.PolicyInstanceID == "" {
		return nil, fmt.Errorf("SecondBox attributed exec window requires the Instance policy")
	}
	config.Listener.InstanceID = config.Fence.InstanceId
	forwarder, err := startForwarder(ctx, config, attribution)
	if err != nil {
		return nil, err
	}
	if err := config.Policy.AllowExecutionListener(ctx, config.PolicyInstanceID, forwarder.ListenerAddress()); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return nil, errors.Join(err, forwarder.Close(cleanupCtx))
	}
	windowCtx, cancel := context.WithCancelCause(ctx)
	window := &AttributedExecWindow{
		policy: config.Policy, policyInstanceID: config.PolicyInstanceID, forwarder: forwarder,
		ctx: windowCtx, cancel: cancel, monitored: make(chan struct{}),
	}
	// Cancellation, control-plane loss, and the exec deadline revoke the
	// window at once, even while the guest has not yet returned the exec.
	revokeNow := func() { window.revokeOnce.Do(window.revoke) }
	stopOnCancel := context.AfterFunc(ctx, revokeNow)
	deadline := time.AfterFunc(time.Until(time.UnixMilli(int64(config.Open.DeadlineUnixMs))), revokeNow)
	window.stopRevokers = func() {
		stopOnCancel()
		deadline.Stop()
	}
	go window.monitor()
	return window, nil
}

// Context cancels the exec when its forwarder fails.
func (window *AttributedExecWindow) Context() context.Context { return window.ctx }

func (window *AttributedExecWindow) Gateway() netip.AddrPort {
	return window.forwarder.ListenerAddress()
}

// monitor revokes the window as soon as the forwarder stops, including at
// expiry while the exec still runs, rather than when the exec ends.
func (window *AttributedExecWindow) monitor() {
	defer close(window.monitored)
	<-window.forwarder.Done()
	if err := window.forwarder.Wait(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		window.failure = fmt.Errorf("SecondBox attributed execution gateway failed: %w", err)
		window.cancel(window.failure)
	}
	window.revokeOnce.Do(window.revoke)
}

// revoke closes the listener and its relays before removing the rules that
// admit guest traffic to it. The Instance itself keeps running.
func (window *AttributedExecWindow) revoke() {
	window.forwarder.Revoke()
	<-window.forwarder.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	window.revokeErr = errors.Join(
		window.policy.RevokeExecutionListener(ctx, window.policyInstanceID, window.forwarder.ListenerAddress()),
		window.forwarder.Close(ctx),
	)
}

// Close revokes the window and reports a forwarder failure or incomplete
// revocation. It is safe to call once the exec has returned.
func (window *AttributedExecWindow) Close() error {
	window.stopRevokers()
	window.revokeOnce.Do(window.revoke)
	<-window.monitored
	window.cancel(nil)
	return errors.Join(window.failure, window.revokeErr)
}

// RunAttributedExec runs execute with a window opener when the Open carries
// attribution, and with none otherwise. The guest session opens the window only
// once the exec owns the session, so a queued exec holds no listener. A window
// failure replaces the exec outcome, because the exec lost its admitted gateway.
func RunAttributedExec[T any](
	ctx context.Context,
	open *runnerprotocol.ExecOpen,
	openWindow func(context.Context) (*AttributedExecWindow, error),
	execute func(context.Context, ExecutionGatewayOpener) (T, error),
) (T, error) {
	if open.GetAttributedExecution() == nil {
		return execute(ctx, nil)
	}
	var window *AttributedExecWindow
	result, err := execute(ctx, func(ctx context.Context) (context.Context, netip.AddrPort, error) {
		opened, err := openWindow(ctx)
		if err != nil {
			return nil, netip.AddrPort{}, err
		}
		window = opened
		return opened.Context(), opened.Gateway(), nil
	})
	if window == nil {
		return result, err
	}
	var zero T
	if closeErr := window.Close(); closeErr != nil {
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		return zero, errors.Join(closeErr, err)
	}
	return result, err
}

func attributedExecAttribution(fence *runnerprotocol.AssignmentFence, open *runnerprotocol.ExecOpen) (egressattribution.ExecutionAttribution, error) {
	execution := open.GetAttributedExecution()
	if fence == nil || execution == nil || open.AllocatePty {
		return egressattribution.ExecutionAttribution{}, fmt.Errorf("SecondBox attributed exec requires a fenced non-PTY Open")
	}
	if execution.TenantRef == "" || execution.SubjectRef == "" || execution.AuthorizationRef == "" ||
		open.DeadlineUnixMs == 0 || open.DeadlineUnixMs > execution.ExpiresAtUnixMs ||
		time.Now().UnixMilli() >= int64(execution.ExpiresAtUnixMs) {
		return egressattribution.ExecutionAttribution{}, fmt.Errorf("SecondBox attributed exec requires identity and a live expiry that bounds its deadline")
	}
	return egressattribution.ExecutionAttribution{
		TenantRef: execution.TenantRef, SubjectRef: execution.SubjectRef,
		SandboxID: fence.SandboxId, InstanceID: fence.InstanceId, AssignmentID: fence.AssignmentId,
		Generation: int64(fence.SandboxGeneration), AuthorizationRef: execution.AuthorizationRef,
		ExpiresAt: time.UnixMilli(int64(execution.ExpiresAtUnixMs)),
	}, nil
}

func (b *AssignmentBackend) attributedExecGateway(assignment *runnerprotocol.AssignmentCommand) (*AttributedExecGateway, error) {
	return ResolveAttributedExecGateway(b.manager.cfg.NetworkPolicyEgressContexts, assignment)
}

// openAttributedExecWindow binds the window listener to the Runner bridge
// address and admits it only from this Instance's TAP and guest address.
func (b *AssignmentBackend) openAttributedExecWindow(ctx context.Context, fence *runnerprotocol.AssignmentFence, open *runnerprotocol.ExecOpen) (*AttributedExecWindow, error) {
	b.mu.Lock()
	active, exists := b.assignments[fence.AssignmentId]
	b.mu.Unlock()
	if !exists || !sameAssignmentFence(active.fence, fence) {
		return nil, fmt.Errorf("SecondBox Firecracker operation fence is stale")
	}
	if active.attributedGateway == nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment does not permit attributed execution")
	}
	inst := b.manager.lookup(active.backendReference)
	if inst == nil || inst.tapName == "" {
		return nil, fmt.Errorf("SecondBox Firecracker attributed execution requires a networked Instance")
	}
	guestAddress, err := netip.ParseAddr(inst.guestIP)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker attributed execution guest address: %w", err)
	}
	return OpenAttributedExecWindow(ctx, AttributedExecWindowConfig{
		NFTPath: b.manager.cfg.NetworkPolicyNFTPath, Policy: b.manager.networkPolicy,
		PolicyInstanceID: inst.id, Gateway: *active.attributedGateway, Fence: fence, Open: open,
		Listener: egressforwarder.ExecutionListenerPolicy{
			GuestInterface: inst.tapName, BridgeInterface: b.manager.cfg.MicroVMBridgeName,
			GuestAddress:    guestAddress,
			ListenerAddress: netip.AddrPortFrom(bridgeAddress(b.manager.cfg.MicroVMBridgeCIDR), 0),
		},
	})
}
