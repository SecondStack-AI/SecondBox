package networkpolicy

import (
	"net/netip"
	"testing"
	"time"
)

func TestExecutionListenerPolicyHasOnlyPrivateEndpoint(t *testing.T) {
	endpoint := netip.MustParseAddrPort("169.254.104.1:41000")
	options := CompileOptions{
		MaximumPins: 4, MaximumTTL: time.Minute,
		ProtectedAddresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")},
		RunnerGateways:     map[string]netip.Addr{"ordinary.internal": netip.MustParseAddr("10.0.0.2")},
	}
	policy, err := CompileExecutionListener(endpoint, options)
	if err != nil {
		t.Fatal(err)
	}
	if policy.AllowsDNS() || len(policy.Destinations()) != 0 {
		t.Fatal("execution policy grants ordinary destination or DNS access")
	}
	gateways := policy.RunnerGatewayDestinations()
	if len(gateways) != 1 || gateways[0].Address != endpoint.Addr() || gateways[0].Destination.Port != endpoint.Port() || gateways[0].Destination.Protocol != ProtocolTCP {
		t.Fatalf("execution gateway destinations = %+v", gateways)
	}
	if !policy.AuthorizeIP(ProtocolTCP, endpoint.Addr(), endpoint.Port()).Allowed {
		t.Fatal("private execution endpoint denied")
	}
	for _, target := range []string{"169.254.104.1:41001", "169.254.104.2:41000", "10.0.0.2:41000", "1.1.1.1:443", "8.8.8.8:53"} {
		address := netip.MustParseAddrPort(target)
		if policy.AuthorizeIP(ProtocolTCP, address.Addr(), address.Port()).Allowed {
			t.Fatalf("unrelated destination permitted: %s", target)
		}
	}
	if _, decision := policy.PinDNS(ProtocolHTTPS, "example.com", 443, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, time.Minute, time.Now()); decision.Allowed {
		t.Fatal("execution policy accepted a DNS pin")
	}
	if !policy.isProtected(netip.MustParseAddr("8.8.8.8")) || !policy.isProtected(endpoint.Addr()) {
		t.Fatal("protected addresses lost")
	}
	if len(options.RunnerGateways) != 1 {
		t.Fatal("ordinary compile options mutated")
	}
}

func TestExecutionListenerPolicyRejectsInvalidEndpoints(t *testing.T) {
	for _, endpoint := range []string{"0.0.0.0:1000", "127.0.0.1:1000", "224.0.0.1:1000", "255.255.255.255:1000", "[::1]:1000", "169.254.1.1:0"} {
		if _, err := CompileExecutionListener(netip.MustParseAddrPort(endpoint), CompileOptions{MaximumPins: 1, MaximumTTL: time.Second}); err == nil {
			t.Fatalf("invalid execution endpoint accepted: %s", endpoint)
		}
	}
}
