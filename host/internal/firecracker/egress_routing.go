package microvm

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HostEgressRouter interface {
	RegisterTransparentRoute(context.Context, TransparentRoute) error
	UnregisterContainer(instanceID string)
}

type TransparentRoute struct {
	AgentID     string
	InstanceID  string
	SourceIP    string
	HTTPPort    int
	InterfaceID string
}

type iptablesRunner func(context.Context, string, ...string) error

type IPTablesEgressRouter struct {
	bin    string
	run    iptablesRunner
	mu     sync.Mutex
	routes map[string]TransparentRoute
}

func NewIPTablesEgressRouter() *IPTablesEgressRouter {
	return &IPTablesEgressRouter{
		bin:    "iptables",
		run:    runIPTables,
		routes: make(map[string]TransparentRoute),
	}
}

func runIPTables(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", bin, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *IPTablesEgressRouter) RegisterTransparentRoute(ctx context.Context, route TransparentRoute) error {
	if err := validateTransparentRoute(route); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior, ok := r.routes[route.InstanceID]; ok {
		if err := r.deleteRouteLocked(ctx, prior); err != nil {
			return err
		}
	}
	// Retain the exact cleanup intent before invoking iptables. If the command
	// reports an error after mutating the host, callers can still remove the
	// rule and launcher state can restore this intent after a restart.
	r.routes[route.InstanceID] = route
	if err := r.run(ctx, r.bin, transparentRouteArgs("-A", route)...); err != nil {
		return err
	}
	return nil
}

func (r *IPTablesEgressRouter) UnregisterContainer(instanceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.UnregisterContainerContext(ctx, instanceID); err != nil {
		slog.Warn("failed to remove microVM transparent egress route", "instance", instanceID, "error", err)
	}
}

func (r *IPTablesEgressRouter) UnregisterContainerContext(ctx context.Context, instanceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	route, ok := r.routes[instanceID]
	if !ok {
		return nil
	}
	if err := r.deleteRouteLocked(ctx, route); err != nil {
		return err
	}
	delete(r.routes, instanceID)
	return nil
}

func (r *IPTablesEgressRouter) deleteRouteLocked(ctx context.Context, route TransparentRoute) error {
	err := r.run(ctx, r.bin, transparentRouteArgs("-D", route)...)
	if err != nil && !missingIPTablesRule(err) {
		return err
	}
	return nil
}

func missingIPTablesRule(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "does a matching rule exist") ||
		strings.Contains(message, "bad rule") ||
		strings.Contains(message, "no chain/target/match by that name")
}

func validateTransparentRoute(route TransparentRoute) error {
	if strings.TrimSpace(route.InstanceID) == "" {
		return fmt.Errorf("instance id is required")
	}
	if net.ParseIP(strings.TrimSpace(route.SourceIP)) == nil {
		return fmt.Errorf("source IP %q is invalid", route.SourceIP)
	}
	if route.HTTPPort <= 0 || route.HTTPPort > 65535 {
		return fmt.Errorf("transparent HTTP port %d is invalid", route.HTTPPort)
	}
	return nil
}

func transparentRouteArgs(action string, route TransparentRoute) []string {
	args := []string{
		"-t", "nat",
		action, "PREROUTING",
		"-s", strings.TrimSpace(route.SourceIP) + "/32",
		"-p", "tcp",
		"--dport", "80",
		"-m", "comment",
		"--comment", "agentcy-microvm-egress:" + route.InstanceID,
	}
	if iface := strings.TrimSpace(route.InterfaceID); iface != "" {
		args = append(args, "-i", iface)
	}
	return append(args,
		"-j", "REDIRECT",
		"--to-ports", strconv.Itoa(route.HTTPPort),
	)
}
