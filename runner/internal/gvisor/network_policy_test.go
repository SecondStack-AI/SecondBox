//go:build linux

package gvisor

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestTranslateNetworkPolicyUsesCompleteRunnerConfiguration(t *testing.T) {
	options := networkpolicy.CompileOptions{
		MaximumPins:        1,
		MaximumTTL:         9 * time.Second,
		RunnerAddresses:    []netip.Addr{netip.MustParseAddr("10.210.2.1")},
		ManagementPrefixes: []netip.Prefix{netip.MustParsePrefix("10.211.0.0/16")},
		RunnerGateways: map[string]netip.Addr{
			"agent-gateway.secondbox.internal": netip.MustParseAddr("10.210.2.2"),
		},
	}
	policy, err := translateNetworkPolicy(&runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{
			{Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443, Target: &runnerprotocol.NetworkDestination_Domain{Domain: "agent-gateway.secondbox.internal"}},
			{Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443, Target: &runnerprotocol.NetworkDestination_Domain{Domain: "public-one.test"}},
			{Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443, Target: &runnerprotocol.NetworkDestination_Domain{Domain: "public-two.test"}},
		},
	}, options)
	if err != nil {
		t.Fatal(err)
	}

	gateway := policy.AuthorizePinned(networkpolicy.ProtocolHTTPS, "agent-gateway.secondbox.internal", netip.MustParseAddr("10.210.2.2"), 443, time.Now())
	if !gateway.Allowed || gateway.Reason != networkpolicy.ReasonAllowedRunnerGateway {
		t.Fatalf("logical gateway decision = %+v", gateway)
	}
	for _, address := range []string{"10.210.2.1", "10.210.2.3", "10.211.9.8", "169.254.169.254"} {
		decision := policy.AuthorizePinned(networkpolicy.ProtocolHTTPS, "agent-gateway.secondbox.internal", netip.MustParseAddr(address), 443, time.Now())
		if decision.Allowed || decision.Reason != networkpolicy.ReasonProtectedDestination {
			t.Fatalf("protected %s decision = %+v", address, decision)
		}
	}

	now := time.Unix(100, 0)
	pin, decision := policy.PinDNS(networkpolicy.ProtocolHTTPS, "public-one.test", 443, []netip.Addr{netip.MustParseAddr("8.8.8.8")}, time.Minute, now)
	if !decision.Allowed || !pin.ExpiresAt.Equal(now.Add(9*time.Second)) {
		t.Fatalf("bounded pin = %+v, decision = %+v", pin, decision)
	}
	_, decision = policy.PinDNS(networkpolicy.ProtocolHTTPS, "public-two.test", 443, []netip.Addr{netip.MustParseAddr("8.8.4.4")}, time.Second, now)
	if decision.Allowed || decision.Reason != networkpolicy.ReasonPinCapacityExhausted {
		t.Fatalf("pin capacity decision = %+v", decision)
	}
}

func TestRenderInetPolicyAllowsOnlyConfiguredProtectedGatewayTuple(t *testing.T) {
	options := networkpolicy.CompileOptions{
		MaximumPins: 4,
		MaximumTTL:  time.Minute,
		RunnerGateways: map[string]netip.Addr{
			"agent-gateway.secondbox.internal": netip.MustParseAddr("10.210.2.2"),
		},
	}
	compiled, err := translateNetworkPolicy(&runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{{
			Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
			Port:     443,
			Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "agent-gateway.secondbox.internal"},
		}},
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	script := renderInetPolicy("sbx_gateway", "gvh0", "169.254.104.2", netip.MustParseAddr("169.254.99.53"), true,
		compiled.ProtectedPrefixes(), compiled.Destinations(), compiled.RunnerGatewayDestinations(), nil)
	inputAllow := `add rule inet sbx_gateway input iifname "gvh0" ip daddr 10.210.2.2 tcp dport 443 ct mark set 0x53425801 accept`
	forwardAllow := `add rule inet sbx_gateway forward iifname "gvh0" ip daddr 10.210.2.2 tcp dport 443 ct mark set 0x53425801 accept`
	for _, want := range []string{inputAllow, forwardAllow} {
		if !strings.Contains(script, want) {
			t.Fatalf("rendered policy lacks exact gateway allow %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "10.210.2.2 tcp dport 80") || strings.Contains(script, "10.210.2.3 tcp dport 443") {
		t.Fatalf("rendered policy broadened gateway allow:\n%s", script)
	}
	inputDrop := `add rule inet sbx_gateway input iifname "gvh0" drop`
	if strings.Index(script, inputAllow) > strings.Index(script, inputDrop) {
		t.Fatalf("input drop shadows runner-local gateway exception:\n%s", script)
	}
	if strings.Index(script, forwardAllow) > strings.Index(script, "ip daddr 10.0.0.0/8 drop") {
		t.Fatalf("protected-prefix drop shadows gateway exception:\n%s", script)
	}
}
