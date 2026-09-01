package networkpolicy

import (
	"net/netip"
	"testing"
	"time"
)

func TestContextSelectionUsesOneMappingAndProtectsEveryContextAddress(t *testing.T) {
	config := EgressContextConfig{
		contexts: map[string]map[string]netip.Addr{
			"installation-a": {"agent-gateway.secondbox.internal": netip.MustParseAddr("8.8.8.8")},
			"installation-b": {"agent-gateway.secondbox.internal": netip.MustParseAddr("9.9.9.9")},
		},
		protectedAddresses: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("9.9.9.9")},
	}
	base := CompileOptions{MaximumPins: 8, MaximumTTL: time.Minute}
	policy := Policy{Mode: ModeAllowList, Destinations: []Destination{{
		Protocol: ProtocolHTTPS,
		Domain:   "agent-gateway.secondbox.internal",
		Port:     443,
	}}}

	optionsA, err := config.CompileOptionsForContext("installation-a", base)
	if err != nil {
		t.Fatal(err)
	}
	compiledA, err := Compile(policy, optionsA)
	if err != nil {
		t.Fatal(err)
	}
	optionsB, err := config.CompileOptionsForContext("installation-b", base)
	if err != nil {
		t.Fatal(err)
	}
	compiledB, err := Compile(policy, optionsB)
	if err != nil {
		t.Fatal(err)
	}

	assertGateway := func(t *testing.T, compiled *CompiledPolicy, selected, other string) {
		t.Helper()
		selectedAddress := netip.MustParseAddr(selected)
		otherAddress := netip.MustParseAddr(other)
		if decision := compiled.AuthorizePinned(ProtocolHTTPS, "agent-gateway.secondbox.internal", selectedAddress, 443, time.Now()); !decision.Allowed || decision.Reason != ReasonAllowedRunnerGateway {
			t.Fatalf("selected gateway decision = %#v", decision)
		}
		if decision := compiled.AuthorizePinned(ProtocolHTTPS, "agent-gateway.secondbox.internal", otherAddress, 443, time.Now()); decision.Allowed || decision.Reason != ReasonProtectedDestination {
			t.Fatalf("other-context gateway decision = %#v", decision)
		}
		if pin, decision := compiled.PinDNS(ProtocolHTTPS, "agent-gateway.secondbox.internal", 443, []netip.Addr{otherAddress}, time.Minute, time.Now()); decision.Allowed || decision.Reason != ReasonProtectedDNSAnswer || pin.Domain != "" {
			t.Fatalf("protected DNS answer = %#v, %#v", pin, decision)
		}
	}
	assertGateway(t, compiledA, "8.8.8.8", "9.9.9.9")
	assertGateway(t, compiledB, "9.9.9.9", "8.8.8.8")
}

func TestCompileRejectsCIDRIntersectingConfiguredGatewayAddress(t *testing.T) {
	_, err := Compile(Policy{Mode: ModeAllowList, Destinations: []Destination{{
		Protocol: ProtocolHTTPS,
		Prefix:   netip.MustParsePrefix("8.8.8.0/24"),
		Port:     443,
	}}}, CompileOptions{
		MaximumPins:        8,
		MaximumTTL:         time.Minute,
		ProtectedAddresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")},
	})
	if err == nil {
		t.Fatal("Compile accepted a direct CIDR containing a configured gateway")
	}
}
