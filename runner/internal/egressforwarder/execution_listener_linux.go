package egressforwarder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/networkpolicycontract"
	"golang.org/x/sys/unix"
)

type ExecutionForwarderConfig struct {
	NFTPath            string
	GatewaySocket      string
	Policy             ExecutionListenerPolicy
	Attribution        egressattribution.ExecutionAttribution
	MaximumConnections int
}

type ExecutionForwarder struct {
	address      netip.AddrPort
	nftPath      string
	policy       ExecutionListenerPolicy
	cancel       context.CancelFunc
	done         chan struct{}
	forwardErr   error
	closeMu      sync.Mutex
	rulesRemoved bool
}

// The caller owns exclusive Instance construction and must Close before releasing
// its network slot. Wait reports forwarding failure separately from cleanup.
func StartExecutionForwarder(ctx context.Context, config ExecutionForwarderConfig) (_ *ExecutionForwarder, resultErr error) {
	ctx, cancelStartup := context.WithDeadline(ctx, config.Attribution.ExpiresAt)
	defer cancelStartup()
	if !filepath.IsAbs(config.NFTPath) || config.MaximumConnections < 1 || config.MaximumConnections > 4096 || config.Policy.InstanceID != config.Attribution.InstanceID {
		return nil, fmt.Errorf("attributed forwarder requires explicit nftables, matching Instance identity, and bounded connections")
	}
	if err := networkpolicycontract.ValidateAttributedGatewaySocket(config.GatewaySocket); err != nil {
		return nil, err
	}
	if err := egressattribution.WriteExecutionAttribution(io.Discard, config.Attribution); err != nil {
		return nil, err
	}
	info, err := os.Stat(config.GatewaySocket)
	if err != nil {
		return nil, fmt.Errorf("attributed forwarder gateway socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("attributed forwarder gateway path is not a Unix socket")
	}
	if !executionListenerIPv4(config.Policy.ListenerAddress.Addr()) {
		return nil, fmt.Errorf("attributed forwarder bind address is invalid")
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return nil, fmt.Errorf("create attributed listener socket: %w", err)
	}
	file := os.NewFile(uintptr(fd), "attributed-listener")
	defer file.Close()
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: int(config.Policy.ListenerAddress.Port()), Addr: config.Policy.ListenerAddress.Addr().As4()}); err != nil {
		return nil, fmt.Errorf("bind attributed listener socket: %w", err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		return nil, fmt.Errorf("inspect attributed listener socket: %w", err)
	}
	address := bound.(*unix.SockaddrInet4)
	config.Policy.ListenerAddress = netip.AddrPortFrom(netip.AddrFrom4(address.Addr), uint16(address.Port))
	rules, err := RenderExecutionListenerPolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, RemoveExecutionListenerRules(cleanupCtx, config.NFTPath, []string{config.Policy.GuestInterface}))
		}
	}()
	if err := applyExecutionListenerRules(ctx, config.NFTPath, rules); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Bind reserves the port without accepting connections until policy is installed.
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, fmt.Errorf("listen on attributed socket: %w", err)
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("open attributed TCP listener: %w", err)
	}
	forwardCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	forwarder := &ExecutionForwarder{address: config.Policy.ListenerAddress, nftPath: config.NFTPath, policy: config.Policy, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(forwarder.done)
		forwarder.forwardErr = ForwardAttributedExecution(forwardCtx, listener.(*net.TCPListener), config.GatewaySocket, config.Attribution, config.MaximumConnections)
	}()
	return forwarder, nil
}

func (forwarder *ExecutionForwarder) ListenerAddress() netip.AddrPort { return forwarder.address }
func (forwarder *ExecutionForwarder) Done() <-chan struct{}           { return forwarder.done }

// Revoke stops accepts and relays immediately; Close still owns rule cleanup.
func (forwarder *ExecutionForwarder) Revoke() { forwarder.cancel() }

// Wait reports the forwarding outcome; Close owns resource cleanup separately.
func (forwarder *ExecutionForwarder) Wait() error {
	<-forwarder.done
	return forwarder.forwardErr
}

func (forwarder *ExecutionForwarder) Close(ctx context.Context) error {
	forwarder.cancel()
	select {
	case <-forwarder.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	forwarder.closeMu.Lock()
	defer forwarder.closeMu.Unlock()
	if forwarder.rulesRemoved {
		return nil
	}
	if err := RemoveExecutionListenerRules(ctx, forwarder.nftPath, []string{forwarder.policy.GuestInterface}); err != nil {
		return err
	}
	forwarder.rulesRemoved = true
	return nil
}

// Call only after revoking the listener or reclaiming a stopped Runner's interfaces.
// Table identity follows the exclusive interface, so restart needs no authority journal.
func RemoveExecutionListenerRules(ctx context.Context, nftPath string, guestInterfaces []string) error {
	ownedTables := make(map[string]bool, len(guestInterfaces))
	for _, name := range guestInterfaces {
		if !executionInterfaceName.MatchString(name) {
			return fmt.Errorf("attributed listener cleanup requires valid guest interfaces")
		}
		ownedTables[executionListenerTable(name)] = true
	}
	if len(ownedTables) == 0 {
		return nil
	}
	output, err := exec.CommandContext(ctx, nftPath, "-j", "list", "tables").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect attributed listener firewall: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var document struct {
		NFTables []struct {
			Table *struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode attributed listener firewall inventory: %w", err)
	}
	if document.NFTables == nil {
		return fmt.Errorf("attributed listener firewall inventory omitted nftables")
	}
	var rules strings.Builder
	for _, entry := range document.NFTables {
		if entry.Table == nil || !ownedTables[entry.Table.Name] {
			continue
		}
		if entry.Table.Family == "inet" || entry.Table.Family == "bridge" {
			fmt.Fprintf(&rules, "delete table %s %s\n", entry.Table.Family, entry.Table.Name)
		}
	}
	if rules.Len() == 0 {
		return nil
	}
	return applyExecutionListenerRules(ctx, nftPath, rules.String())
}

func applyExecutionListenerRules(ctx context.Context, nftPath, rules string) error {
	command := exec.CommandContext(ctx, nftPath, "-f", "-")
	command.Stdin = strings.NewReader(rules)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("attributed listener firewall: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
