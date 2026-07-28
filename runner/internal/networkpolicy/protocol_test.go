package networkpolicy

import (
	"net/netip"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestFromRunnerProtocolConvertsImmutablePolicy(t *testing.T) {
	converted, err := FromRunnerProtocol(&runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{
			{
				Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
				Port:     443,
			},
			{
				Target:   &runnerprotocol.NetworkDestination_Cidr{Cidr: "8.8.8.0/24"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP,
				Port:     853,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if converted.Mode != ModeAllowList || len(converted.Destinations) != 2 {
		t.Fatalf("policy = %#v", converted)
	}
	if converted.Destinations[0].Domain != "example.com" ||
		converted.Destinations[0].Protocol != ProtocolHTTPS ||
		converted.Destinations[0].Port != 443 {
		t.Fatalf("domain destination = %#v", converted.Destinations[0])
	}
	if converted.Destinations[1].Prefix != netip.MustParsePrefix("8.8.8.0/24") ||
		converted.Destinations[1].Protocol != ProtocolTCP ||
		converted.Destinations[1].Port != 853 {
		t.Fatalf("CIDR destination = %#v", converted.Destinations[1])
	}
}

func TestFromRunnerProtocolRejectsMissingOrUnknownValues(t *testing.T) {
	tests := []struct {
		name   string
		policy *runnerprotocol.NetworkPolicy
	}{
		{name: "missing"},
		{name: "unspecified mode", policy: &runnerprotocol.NetworkPolicy{}},
		{
			name: "nil destination",
			policy: &runnerprotocol.NetworkPolicy{
				Mode:         runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
				Destinations: []*runnerprotocol.NetworkDestination{nil},
			},
		},
		{
			name: "unspecified protocol",
			policy: &runnerprotocol.NetworkPolicy{
				Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
				Destinations: []*runnerprotocol.NetworkDestination{{
					Target: &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
					Port:   443,
				}},
			},
		},
		{
			name: "unknown target",
			policy: &runnerprotocol.NetworkPolicy{
				Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
				Destinations: []*runnerprotocol.NetworkDestination{{
					Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
					Port:     443,
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := FromRunnerProtocol(test.policy); err == nil {
				t.Fatal("FromRunnerProtocol succeeded")
			}
		})
	}
}
