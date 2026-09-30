package egressforwarder

import (
	"net/netip"
	"strings"
	"testing"
)

func TestExecutionListenerPolicyValidation(t *testing.T) {
	valid := ExecutionListenerPolicy{InstanceID: "instance", GuestInterface: "gvh0", GuestAddress: netip.MustParseAddr("169.254.104.2"), ListenerAddress: netip.MustParseAddrPort("169.254.104.1:31000"), ListenerID: "0123456789abcdef"}
	if _, err := RenderExecutionListenerPolicy(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ExecutionListenerPolicy){
		"missing instance":      func(p *ExecutionListenerPolicy) { p.InstanceID = "" },
		"missing listener ID":   func(p *ExecutionListenerPolicy) { p.ListenerID = "" },
		"listener ID injection": func(p *ExecutionListenerPolicy) { p.ListenerID = "0123456789abcde;" },
		"interface injection":   func(p *ExecutionListenerPolicy) { p.GuestInterface = `eth0" accept` },
		"long interface":        func(p *ExecutionListenerPolicy) { p.GuestInterface = strings.Repeat("x", 16) },
		"invalid bridge":        func(p *ExecutionListenerPolicy) { p.BridgeInterface = "bad interface" },
		"same bridge":           func(p *ExecutionListenerPolicy) { p.BridgeInterface = p.GuestInterface },
		"wildcard listener":     func(p *ExecutionListenerPolicy) { p.ListenerAddress = netip.MustParseAddrPort("0.0.0.0:31000") },
		"loopback listener":     func(p *ExecutionListenerPolicy) { p.ListenerAddress = netip.MustParseAddrPort("127.0.0.1:31000") },
		"missing port":          func(p *ExecutionListenerPolicy) { p.ListenerAddress = netip.AddrPortFrom(p.ListenerAddress.Addr(), 0) },
		"same addresses":        func(p *ExecutionListenerPolicy) { p.GuestAddress = p.ListenerAddress.Addr() },
		"multicast guest":       func(p *ExecutionListenerPolicy) { p.GuestAddress = netip.MustParseAddr("224.0.0.1") },
		"ipv6 guest":            func(p *ExecutionListenerPolicy) { p.GuestAddress = netip.MustParseAddr("2001:db8::1") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			change(&changed)
			if _, err := RenderExecutionListenerPolicy(changed); err == nil {
				t.Fatal("invalid listener policy accepted")
			}
		})
	}
}

func TestExecutionListenerTablesArePerExecAndSweptPerInterface(t *testing.T) {
	first := executionListenerTable("tap0", "0123456789abcdef")
	second := executionListenerTable("tap0", "fedcba9876543210")
	other := executionListenerTable("tap1", "0123456789abcdef")
	prefix := executionListenerTablePrefix("tap0")
	if first == second || !strings.HasPrefix(first, prefix) || !strings.HasPrefix(second, prefix) || strings.HasPrefix(other, prefix) {
		t.Fatalf("listener tables first=%s second=%s other=%s prefix=%s", first, second, other, prefix)
	}
	rules, err := RenderExecutionListenerPolicy(ExecutionListenerPolicy{
		InstanceID: "instance", GuestInterface: "tap0", BridgeInterface: "br0", ListenerID: "0123456789abcdef",
		GuestAddress: netip.MustParseAddr("10.0.0.2"), ListenerAddress: netip.MustParseAddrPort("10.0.0.1:41000"),
	})
	if err != nil || !strings.Contains(rules, "add table inet "+first+"\n") || !strings.Contains(rules, "add table bridge "+first+"\n") {
		t.Fatalf("listener rules = %q, error = %v", rules, err)
	}
}
