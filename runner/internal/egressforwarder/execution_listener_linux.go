package egressforwarder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// Admission is checked under the listener table lock; Instance teardown
	// fences it before sweeping the interface.
	Admission func() error
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

// StartExecutionForwarder binds one attributed exec's listener and installs its
// listener table before accepting. The caller must Close it before releasing the
// Instance network. Wait reports forwarding failure separately from cleanup.
func StartExecutionForwarder(ctx context.Context, config ExecutionForwarderConfig) (*ExecutionForwarder, error) {
	ctx, cancelStartup := context.WithDeadline(ctx, config.Attribution.ExpiresAt)
	defer cancelStartup()
	if config.Admission == nil || !filepath.IsAbs(config.NFTPath) || config.MaximumConnections < 1 || config.MaximumConnections > 4096 || config.Policy.InstanceID != config.Attribution.InstanceID {
		return nil, fmt.Errorf("attributed forwarder requires explicit nftables, an admission check, matching Instance identity, and bounded connections")
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
	var listenerID [8]byte
	if _, err := rand.Read(listenerID[:]); err != nil {
		return nil, fmt.Errorf("create attributed listener identity: %w", err)
	}
	config.Policy.ListenerID = hex.EncodeToString(listenerID[:])
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
	// The admission check, the listener table, the listener, and the
	// registration happen under the sweep lock, so an interface sweep either
	// sees and revokes this forwarder or runs first and fences its admission.
	executionListenerTablesMu.Lock()
	defer executionListenerTablesMu.Unlock()
	if err := config.Admission(); err != nil {
		return nil, err
	}
	listener, err := installExecutionListener(ctx, config.NFTPath, rules, fd, file)
	if err != nil {
		// A failed nft invocation may still have applied the table.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return nil, errors.Join(err, removeExecutionListenerTablesLocked(cleanupCtx, config.NFTPath, map[string]bool{
			executionListenerTable(config.Policy.GuestInterface, config.Policy.ListenerID): true,
		}, nil))
	}
	forwardCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	forwarder := &ExecutionForwarder{address: config.Policy.ListenerAddress, nftPath: config.NFTPath, policy: config.Policy, cancel: cancel, done: make(chan struct{})}
	liveExecutionForwarders.add(forwarder)
	go func() {
		defer close(forwarder.done)
		defer liveExecutionForwarders.remove(forwarder)
		forwarder.forwardErr = ForwardAttributedExecution(forwardCtx, listener, config.GatewaySocket, config.Attribution, config.MaximumConnections)
	}()
	return forwarder, nil
}

// installExecutionListener starts accepting only after the listener table
// exists; bind alone reserves the port.
func installExecutionListener(ctx context.Context, nftPath, rules string, fd int, file *os.File) (*net.TCPListener, error) {
	if err := applyExecutionListenerRules(ctx, nftPath, rules); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, fmt.Errorf("listen on attributed socket: %w", err)
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("open attributed TCP listener: %w", err)
	}
	return listener.(*net.TCPListener), nil
}

// liveExecutionForwarders lets an interface sweep close every listener on the
// interface before it deletes the tables that restrict those listeners.
var liveExecutionForwarders = executionForwarderRegistry{forwarders: make(map[*ExecutionForwarder]struct{})}

type executionForwarderRegistry struct {
	mu         sync.Mutex
	forwarders map[*ExecutionForwarder]struct{}
}

func (registry *executionForwarderRegistry) add(forwarder *ExecutionForwarder) {
	registry.mu.Lock()
	registry.forwarders[forwarder] = struct{}{}
	registry.mu.Unlock()
}

func (registry *executionForwarderRegistry) remove(forwarder *ExecutionForwarder) {
	registry.mu.Lock()
	delete(registry.forwarders, forwarder)
	registry.mu.Unlock()
}

// revoke closes the listener and relays of every forwarder on the interfaces
// and waits until each has stopped.
func (registry *executionForwarderRegistry) revoke(ctx context.Context, guestInterfaces map[string]bool) error {
	registry.mu.Lock()
	var revoked []*ExecutionForwarder
	for forwarder := range registry.forwarders {
		if guestInterfaces[forwarder.policy.GuestInterface] {
			revoked = append(revoked, forwarder)
		}
	}
	registry.mu.Unlock()
	for _, forwarder := range revoked {
		forwarder.Revoke()
	}
	for _, forwarder := range revoked {
		select {
		case <-forwarder.done:
		case <-ctx.Done():
			return fmt.Errorf("attributed listener revocation did not finish: %w", ctx.Err())
		}
	}
	return nil
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
	if err := removeExecutionListenerTables(ctx, forwarder.nftPath, map[string]bool{
		executionListenerTable(forwarder.policy.GuestInterface, forwarder.policy.ListenerID): true,
	}, nil); err != nil {
		return err
	}
	forwarder.rulesRemoved = true
	return nil
}

// RemoveExecutionListenerRules closes every live listener of the given guest
// interfaces, then sweeps their listener tables. Call it at Instance teardown
// or when reclaiming a stopped Runner's interfaces. Table names derive from the
// interface, so restart needs no authority journal.
func RemoveExecutionListenerRules(ctx context.Context, nftPath string, guestInterfaces []string) error {
	prefixes := make([]string, 0, len(guestInterfaces))
	interfaces := make(map[string]bool, len(guestInterfaces))
	for _, name := range guestInterfaces {
		if !executionInterfaceName.MatchString(name) {
			return fmt.Errorf("attributed listener cleanup requires valid guest interfaces")
		}
		prefixes = append(prefixes, executionListenerTablePrefix(name))
		interfaces[name] = true
	}
	if len(prefixes) == 0 {
		return nil
	}
	executionListenerTablesMu.Lock()
	defer executionListenerTablesMu.Unlock()
	if err := liveExecutionForwarders.revoke(ctx, interfaces); err != nil {
		return err
	}
	return removeExecutionListenerTablesLocked(ctx, nftPath, nil, prefixes)
}

// executionListenerTablesMu orders listener table creation, forwarder
// registration, and table removal. A sweep therefore revokes every forwarder
// whose table it deletes, and an exec window closing during a sweep cannot make
// the sweep delete a table that no longer exists.
var executionListenerTablesMu sync.Mutex

func removeExecutionListenerTables(ctx context.Context, nftPath string, exact map[string]bool, prefixes []string) error {
	executionListenerTablesMu.Lock()
	defer executionListenerTablesMu.Unlock()
	return removeExecutionListenerTablesLocked(ctx, nftPath, exact, prefixes)
}

func removeExecutionListenerTablesLocked(ctx context.Context, nftPath string, exact map[string]bool, prefixes []string) error {
	owned := func(table string) bool {
		if exact[table] {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(table, prefix) {
				return true
			}
		}
		return false
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
		if entry.Table == nil || !owned(entry.Table.Name) {
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
