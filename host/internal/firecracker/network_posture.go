package firecracker

import (
	"context"
	"fmt"
	"strings"
)

// NetworkPostureReport is the machine-readable privileged-launch admission
// contract. Missing contains stable invariant names, never command output.
type NetworkPostureReport struct {
	Healthy       bool              `json:"healthy"`
	Missing       []string          `json:"missing,omitempty"`
	FailureCounts map[string]uint64 `json:"failureCounts,omitempty"`
}

type networkPostureCheck struct {
	name     string
	command  string
	args     []string
	contains string
}

type firewallOrderingCheck struct {
	name                  string
	command               string
	chain                 string
	requiredBeforeAccept  [][]string
	terminalDenyComment   string
	allowedAcceptComments []string
}

func probeNetworkPosture(ctx context.Context, cfg PrivilegedLauncherConfig, run commandRunner) NetworkPostureReport {
	bridge := strings.TrimSpace(cfg.BridgeName)
	tapPattern := strings.TrimSpace(cfg.TapPrefix) + "+"
	ipCommand := strings.TrimSpace(cfg.HarnessIPCommand)
	if ipCommand == "" {
		ipCommand = "ip"
	}
	checks := []networkPostureCheck{
		// `show up` checks the administrative UP flag. A bridge with no attached
		// carrier can legitimately report operational `state DOWN` before the
		// first tap is configured.
		{name: "bridge_link_up", command: ipCommand, args: []string{"-o", "link", "show", "up", "dev", bridge}, contains: bridge},
		{name: "bridge_ipv4_address", command: ipCommand, args: []string{"-4", "-o", "addr", "show", "dev", bridge}, contains: strings.TrimSpace(cfg.BridgeCIDR)},
		{name: "ipv4_forwarding", command: "sysctl", args: []string{"-n", "net.ipv4.ip_forward"}, contains: "1"},
		{name: "guest_host_input_hook", command: "iptables", args: []string{"-t", "filter", "-C", "INPUT", "-i", bridge, "-m", "comment", "--comment", "sandbox-host-guest-input", "-j", "SANDBOX_HOST_GUEST_IN"}},
		{name: "guest_host_input_tap_hook", command: "iptables", args: []string{"-t", "filter", "-C", "INPUT", "-i", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-input", "-j", "SANDBOX_HOST_GUEST_IN"}},
		{name: "guest_host_input_default_deny", command: "iptables", args: []string{"-t", "filter", "-C", "SANDBOX_HOST_GUEST_IN", "-m", "comment", "--comment", "sandbox-host-guest-input-deny", "-j", "DROP"}},
		{name: "guest_forward_hook", command: "iptables", args: []string{"-t", "filter", "-C", "FORWARD", "-i", bridge, "-m", "comment", "--comment", "sandbox-host-guest-forward-policy", "-j", "SANDBOX_HOST_GUEST_FWD"}},
		{name: "guest_forward_tap_hook", command: "iptables", args: []string{"-t", "filter", "-C", "FORWARD", "-i", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-forward-policy", "-j", "SANDBOX_HOST_GUEST_FWD"}},
		{name: "guest_forward_default_deny", command: "iptables", args: []string{"-t", "filter", "-C", "SANDBOX_HOST_GUEST_FWD", "-m", "comment", "--comment", "sandbox-host-guest-forward-deny", "-j", "DROP"}},
		{name: "guest_forward_input_hook", command: "iptables", args: []string{"-t", "filter", "-C", "FORWARD", "-o", bridge, "-m", "comment", "--comment", "sandbox-host-guest-forward-input-policy", "-j", "SANDBOX_HOST_GUEST_FWD_IN"}},
		{name: "guest_forward_input_tap_hook", command: "iptables", args: []string{"-t", "filter", "-C", "FORWARD", "-o", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-forward-input-policy", "-j", "SANDBOX_HOST_GUEST_FWD_IN"}},
		{name: "guest_forward_input_default_deny", command: "iptables", args: []string{"-t", "filter", "-C", "SANDBOX_HOST_GUEST_FWD_IN", "-m", "comment", "--comment", "sandbox-host-guest-forward-in-deny", "-j", "DROP"}},
		{name: "guest_ipv6_input_hook", command: "ip6tables", args: []string{"-C", "INPUT", "-i", bridge, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-input", "-j", "SANDBOX_HOST_GUEST6_IN"}},
		{name: "guest_ipv6_input_tap_hook", command: "ip6tables", args: []string{"-C", "INPUT", "-i", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-input", "-j", "SANDBOX_HOST_GUEST6_IN"}},
		{name: "guest_ipv6_input_deny", command: "ip6tables", args: []string{"-C", "SANDBOX_HOST_GUEST6_IN", "-m", "comment", "--comment", "sandbox-host-guest-ipv6-deny", "-j", "DROP"}},
		{name: "guest_ipv6_forward_hook", command: "ip6tables", args: []string{"-C", "FORWARD", "-i", bridge, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-forward", "-j", "SANDBOX_HOST_GUEST6_FWD"}},
		{name: "guest_ipv6_forward_tap_hook", command: "ip6tables", args: []string{"-C", "FORWARD", "-i", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-forward", "-j", "SANDBOX_HOST_GUEST6_FWD"}},
		{name: "guest_ipv6_forward_input_hook", command: "ip6tables", args: []string{"-C", "FORWARD", "-o", bridge, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-forward-in", "-j", "SANDBOX_HOST_GUEST6_FWD"}},
		{name: "guest_ipv6_forward_input_tap_hook", command: "ip6tables", args: []string{"-C", "FORWARD", "-o", tapPattern, "-m", "comment", "--comment", "sandbox-host-guest-ipv6-forward-in", "-j", "SANDBOX_HOST_GUEST6_FWD"}},
		{name: "guest_ipv6_forward_deny", command: "ip6tables", args: []string{"-C", "SANDBOX_HOST_GUEST6_FWD", "-m", "comment", "--comment", "sandbox-host-guest-ipv6-forward-deny", "-j", "DROP"}},
	}
	if bridge == "" || strings.TrimSpace(cfg.TapPrefix) == "" || strings.TrimSpace(cfg.BridgeCIDR) == "" {
		return NetworkPostureReport{Healthy: false, Missing: []string{"bridge_contract_config"}}
	}
	report := NetworkPostureReport{Healthy: true}
	for _, check := range checks {
		out, err := run(ctx, check.command, check.args...)
		if err != nil || (check.contains != "" && !outputContainsPostureValue(out, check.contains)) {
			report.Healthy = false
			report.Missing = append(report.Missing, check.name)
		}
	}
	orderingChecks := []firewallOrderingCheck{
		{name: "guest_host_input_precedence", command: "iptables", chain: "INPUT", requiredBeforeAccept: [][]string{{"-i " + bridge, "--comment sandbox-host-guest-input", "-j SANDBOX_HOST_GUEST_IN"}, {"-i " + tapPattern, "--comment sandbox-host-guest-input", "-j SANDBOX_HOST_GUEST_IN"}}},
		{name: "guest_forward_precedence", command: "iptables", chain: "FORWARD", requiredBeforeAccept: [][]string{{"-i " + bridge, "--comment sandbox-host-guest-forward-policy", "-j SANDBOX_HOST_GUEST_FWD"}, {"-i " + tapPattern, "--comment sandbox-host-guest-forward-policy", "-j SANDBOX_HOST_GUEST_FWD"}, {"-o " + bridge, "--comment sandbox-host-guest-forward-input-policy", "-j SANDBOX_HOST_GUEST_FWD_IN"}, {"-o " + tapPattern, "--comment sandbox-host-guest-forward-input-policy", "-j SANDBOX_HOST_GUEST_FWD_IN"}}},
		{name: "guest_forward_default_deny_order", command: "iptables", chain: "SANDBOX_HOST_GUEST_FWD", terminalDenyComment: "sandbox-host-guest-forward-deny", allowedAcceptComments: []string{"sandbox-host-guest-forward-out"}},
		{name: "guest_forward_input_default_deny_order", command: "iptables", chain: "SANDBOX_HOST_GUEST_FWD_IN", terminalDenyComment: "sandbox-host-guest-forward-in-deny", allowedAcceptComments: []string{"sandbox-host-guest-forward-in"}},
		{name: "guest_ipv6_input_precedence", command: "ip6tables", chain: "INPUT", requiredBeforeAccept: [][]string{{"-i " + bridge, "--comment sandbox-host-guest-ipv6-input", "-j SANDBOX_HOST_GUEST6_IN"}, {"-i " + tapPattern, "--comment sandbox-host-guest-ipv6-input", "-j SANDBOX_HOST_GUEST6_IN"}}},
		{name: "guest_ipv6_forward_precedence", command: "ip6tables", chain: "FORWARD", requiredBeforeAccept: [][]string{{"-i " + bridge, "--comment sandbox-host-guest-ipv6-forward", "-j SANDBOX_HOST_GUEST6_FWD"}, {"-i " + tapPattern, "--comment sandbox-host-guest-ipv6-forward", "-j SANDBOX_HOST_GUEST6_FWD"}, {"-o " + bridge, "--comment sandbox-host-guest-ipv6-forward-in", "-j SANDBOX_HOST_GUEST6_FWD"}, {"-o " + tapPattern, "--comment sandbox-host-guest-ipv6-forward-in", "-j SANDBOX_HOST_GUEST6_FWD"}}},
		{name: "guest_ipv6_input_deny_order", command: "ip6tables", chain: "SANDBOX_HOST_GUEST6_IN", terminalDenyComment: "sandbox-host-guest-ipv6-deny"},
		{name: "guest_ipv6_forward_deny_order", command: "ip6tables", chain: "SANDBOX_HOST_GUEST6_FWD", terminalDenyComment: "sandbox-host-guest-ipv6-forward-deny"},
	}
	for _, check := range orderingChecks {
		out, err := run(ctx, check.command, "-t", "filter", "-S", check.chain)
		if err != nil || !firewallOrderingHealthy(out, check) {
			report.Healthy = false
			report.Missing = append(report.Missing, check.name)
		}
	}
	return report
}

func firewallOrderingHealthy(output []byte, check firewallOrderingCheck) bool {
	lines := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" && strings.HasPrefix(line, "-A ") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return false
	}
	firstAccept := len(lines)
	for i, line := range lines {
		if strings.Contains(line, "-j ACCEPT") && i < firstAccept {
			firstAccept = i
		}
	}
	for _, fragments := range check.requiredBeforeAccept {
		found := -1
		for i, line := range lines {
			matches := true
			for _, fragment := range fragments {
				if !strings.Contains(line, fragment) {
					matches = false
					break
				}
			}
			if matches {
				found = i
				break
			}
		}
		if found < 0 || found >= firstAccept {
			return false
		}
	}
	if check.terminalDenyComment == "" {
		return true
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "--comment "+check.terminalDenyComment) || !strings.Contains(last, "-j DROP") {
		return false
	}
	for _, line := range lines[:len(lines)-1] {
		if !strings.Contains(line, "-j ACCEPT") {
			continue
		}
		allowed := false
		for _, comment := range check.allowedAcceptComments {
			if strings.Contains(line, "--comment "+comment) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func outputContainsPostureValue(output []byte, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if want == "1" {
		return strings.TrimSpace(string(output)) == want
	}
	return strings.Contains(string(output), want)
}

func (r NetworkPostureReport) admissionError() error {
	if r.Healthy {
		return nil
	}
	return fmt.Errorf("microVM host network posture is incomplete: %s", strings.Join(r.Missing, ","))
}
