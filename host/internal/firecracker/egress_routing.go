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
	if err := r.run(ctx, r.bin, transparentRouteArgs("-A", route)...); err != nil {
		return err
	}
	r.routes[route.InstanceID] = route
	return nil
}

func (r *IPTablesEgressRouter) UnregisterContainer(instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	route, ok := r.routes[instanceID]
	if !ok {
		return
	}
	if err := r.deleteRouteLocked(context.Background(), route); err != nil {
		slog.Warn("failed to remove microVM transparent egress route", "instance", instanceID, "error", err)
		return
	}
	delete(r.routes, instanceID)
}

func (r *IPTablesEgressRouter) deleteRouteLocked(ctx context.Context, route TransparentRoute) error {
	return r.run(ctx, r.bin, transparentRouteArgs("-D", route)...)
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
