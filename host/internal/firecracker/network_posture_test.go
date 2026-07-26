package microvm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func completeNetworkPostureRunner(cfg PrivilegedLauncherConfig) commandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == cfg.HarnessIPCommand && strings.Contains(joined, "link show"):
			return []byte("9: " + cfg.BridgeName + ": <BROADCAST,UP> state DOWN"), nil
		case name == cfg.HarnessIPCommand && strings.Contains(joined, "addr show"):
			return []byte("inet " + cfg.BridgeCIDR + " scope global " + cfg.BridgeName), nil
		case name == "sysctl":
			return []byte("1\n"), nil
		case name == "iptables" && strings.Contains(joined, "-S INPUT"):
			return []byte("-A INPUT -i " + cfg.BridgeName + " -m comment --comment agent-manager-sandbox-input -j AGENT_MANAGER_SANDBOX_IN\n-A INPUT -i " + cfg.TapPrefix + "+ -m comment --comment agent-manager-sandbox-input -j AGENT_MANAGER_SANDBOX_IN\n-A INPUT -j ACCEPT\n"), nil
		case name == "iptables" && strings.Contains(joined, "-S FORWARD"):
			return []byte("-A FORWARD -o " + cfg.TapPrefix + "+ -m comment --comment agent-manager-microvm-forward-input-policy -j AGENT_MANAGER_GUEST_FWD_IN\n-A FORWARD -o " + cfg.BridgeName + " -m comment --comment agent-manager-microvm-forward-input-policy -j AGENT_MANAGER_GUEST_FWD_IN\n-A FORWARD -i " + cfg.TapPrefix + "+ -m comment --comment agent-manager-microvm-forward-policy -j AGENT_MANAGER_GUEST_FWD\n-A FORWARD -i " + cfg.BridgeName + " -m comment --comment agent-manager-microvm-forward-policy -j AGENT_MANAGER_GUEST_FWD\n-A FORWARD -j ACCEPT\n"), nil
		case name == "iptables" && strings.Contains(joined, "-S AGENT_MANAGER_GUEST_FWD_IN"):
			return []byte("-A AGENT_MANAGER_GUEST_FWD_IN -m comment --comment agent-manager-microvm-forward-in-deny -j DROP\n"), nil
		case name == "iptables" && strings.Contains(joined, "-S AGENT_MANAGER_GUEST_FWD"):
			return []byte("-A AGENT_MANAGER_GUEST_FWD -m comment --comment agent-manager-microvm-forward-deny -j DROP\n"), nil
		case name == "ip6tables" && strings.Contains(joined, "-S INPUT"):
			return []byte("-A INPUT -i " + cfg.BridgeName + " -m comment --comment agent-manager-sandbox-ipv6-input -j AGENT_MANAGER_SANDBOX6_IN\n-A INPUT -i " + cfg.TapPrefix + "+ -m comment --comment agent-manager-sandbox-ipv6-input -j AGENT_MANAGER_SANDBOX6_IN\n-A INPUT -j ACCEPT\n"), nil
		case name == "ip6tables" && strings.Contains(joined, "-S FORWARD"):
			return []byte("-A FORWARD -o " + cfg.TapPrefix + "+ -m comment --comment agent-manager-sandbox-ipv6-forward-in -j AGENT_MANAGER_SANDBOX6_FWD\n-A FORWARD -o " + cfg.BridgeName + " -m comment --comment agent-manager-sandbox-ipv6-forward-in -j AGENT_MANAGER_SANDBOX6_FWD\n-A FORWARD -i " + cfg.TapPrefix + "+ -m comment --comment agent-manager-sandbox-ipv6-forward -j AGENT_MANAGER_SANDBOX6_FWD\n-A FORWARD -i " + cfg.BridgeName + " -m comment --comment agent-manager-sandbox-ipv6-forward -j AGENT_MANAGER_SANDBOX6_FWD\n-A FORWARD -j ACCEPT\n"), nil
		case name == "ip6tables" && strings.Contains(joined, "-S AGENT_MANAGER_SANDBOX6_IN"):
			return []byte("-A AGENT_MANAGER_SANDBOX6_IN -m comment --comment agent-manager-sandbox-ipv6-deny -j DROP\n"), nil
		case name == "ip6tables" && strings.Contains(joined, "-S AGENT_MANAGER_SANDBOX6_FWD"):
			return []byte("-A AGENT_MANAGER_SANDBOX6_FWD -m comment --comment agent-manager-sandbox-ipv6-forward-deny -j DROP\n"), nil
		default:
			return nil, nil
		}
	}
}

func TestProbeNetworkPostureComplete(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	report := probeNetworkPosture(context.Background(), cfg, completeNetworkPostureRunner(cfg))
	if !report.Healthy || len(report.Missing) != 0 {
		t.Fatalf("complete posture = %+v", report)
	}
}

func TestProbeNetworkPostureReportsMissingFirewallInvariant(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "iptables" && strings.Contains(strings.Join(args, " "), "agent-manager-microvm-forward-deny") {
			return nil, errors.New("rule missing")
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || len(report.Missing) != 1 || report.Missing[0] != "guest_forward_default_deny" {
		t.Fatalf("missing posture = %+v", report)
	}
}

func TestProbeNetworkPostureReportsMissingTapPrefixHook(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "iptables" && strings.Contains(joined, "FORWARD -i agtap+") {
			return nil, errors.New("tap hook missing")
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || len(report.Missing) != 1 || report.Missing[0] != "guest_forward_tap_hook" {
		t.Fatalf("missing tap posture = %+v", report)
	}
}

func TestProbeNetworkPostureReportsMissingInboundForwardHook(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "iptables" && strings.Contains(joined, "-C FORWARD -o agtap+") {
			return nil, errors.New("inbound tap hook missing")
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || !strings.Contains(strings.Join(report.Missing, ","), "guest_forward_input_tap_hook") {
		t.Fatalf("missing inbound posture = %+v", report)
	}
}

func TestProbeNetworkPostureRejectsBypassingAcceptBeforeHooks(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "iptables" && strings.Contains(joined, "-S FORWARD") {
			out, err := complete(ctx, name, args...)
			return append([]byte("-A FORWARD -j ACCEPT\n"), out...), err
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || !strings.Contains(strings.Join(report.Missing, ","), "guest_forward_precedence") {
		t.Fatalf("bypassing accept posture = %+v", report)
	}
}

func TestProbeNetworkPostureRejectsUnexpectedAcceptBeforeTerminalDeny(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "iptables" && strings.Contains(joined, "-S AGENT_MANAGER_GUEST_FWD") && !strings.Contains(joined, "_IN") {
			return []byte("-A AGENT_MANAGER_GUEST_FWD -j ACCEPT\n-A AGENT_MANAGER_GUEST_FWD -m comment --comment agent-manager-microvm-forward-deny -j DROP\n"), nil
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || !strings.Contains(strings.Join(report.Missing, ","), "guest_forward_default_deny_order") {
		t.Fatalf("unexpected accept posture = %+v", report)
	}
}

func TestProbeNetworkPostureRejectsMalformedHostState(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if name == "sysctl" {
			return []byte("unexpected"), nil
		}
		if name == cfg.HarnessIPCommand && strings.Contains(joined, "link show") {
			return []byte("state DOWN"), nil
		}
		return complete(ctx, name, args...)
	}
	report := probeNetworkPosture(context.Background(), cfg, runner)
	if report.Healthy || !strings.Contains(strings.Join(report.Missing, ","), "bridge_link_up") || !strings.Contains(strings.Join(report.Missing, ","), "ipv4_forwarding") {
		t.Fatalf("malformed posture = %+v", report)
	}
}

func TestRequireNetworkPostureMetersMissingInvariant(t *testing.T) {
	cfg := PrivilegedLauncherConfig{BridgeName: "agfc0", BridgeCIDR: "172.30.0.1/24", TapPrefix: "agtap", HarnessIPCommand: "ip"}
	complete := completeNetworkPostureRunner(cfg)
	server := &PrivilegedLauncherServer{
		cfg:             cfg,
		postureFailures: map[string]uint64{},
		runHost: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "ip6tables" && strings.Contains(strings.Join(args, " "), "agent-manager-sandbox-ipv6-forward-deny") {
				return nil, errors.New("rule missing")
			}
			return complete(ctx, name, args...)
		},
	}
	if err := server.requireNetworkPosture(context.Background()); err == nil || !strings.Contains(err.Error(), "guest_ipv6_forward_deny") {
		t.Fatalf("require posture = %v", err)
	}
	report := server.networkPosture(context.Background())
	if report.FailureCounts["guest_ipv6_forward_deny"] != 1 {
		t.Fatalf("posture failure counts = %+v", report.FailureCounts)
	}
}
