package microsandbox

import (
	"strings"
	"testing"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestTranslateNetworkPolicyPreservesExactRules(t *testing.T) {
	policy, err := translateNetworkPolicy(&runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{
			{Target: &runnerprotocol.NetworkDestination_Domain{Domain: "API.Example.COM"}, Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443},
			{Target: &runnerprotocol.NetworkDestination_Cidr{Cidr: "93.184.216.0/24"}, Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTP, Port: 8080},
			{Target: &runnerprotocol.NetworkDestination_Cidr{Cidr: "2001:4860:4860::/48"}, Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP, Port: 8443},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != microsandboxprotocol.HelperNetworkPolicyMode_HELPER_NETWORK_POLICY_MODE_ALLOW_LIST || len(policy.Destinations) != 3 {
		t.Fatalf("translated policy = %#v", policy)
	}
	if policy.Destinations[0].GetDomain() != "api.example.com" || policy.Destinations[0].Protocol != microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTPS || policy.Destinations[0].Port != 443 {
		t.Fatalf("domain rule = %#v", policy.Destinations[0])
	}
	if policy.Destinations[1].GetCidr() != "93.184.216.0/24" || policy.Destinations[1].Protocol != microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTP || policy.Destinations[1].Port != 8080 {
		t.Fatalf("IPv4 rule = %#v", policy.Destinations[1])
	}
	if policy.Destinations[2].GetCidr() != "2001:4860:4860::/48" || policy.Destinations[2].Protocol != microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_TCP || policy.Destinations[2].Port != 8443 {
		t.Fatalf("IPv6 rule = %#v", policy.Destinations[2])
	}
}

func TestTranslateNetworkPolicyFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		policy *runnerprotocol.NetworkPolicy
		want   string
	}{
		{name: "missing", want: "required"},
		{name: "deny destinations", policy: &runnerprotocol.NetworkPolicy{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL, Destinations: []*runnerprotocol.NetworkDestination{{Target: &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"}, Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443}}}, want: "cannot contain destinations"},
		{name: "private CIDR", policy: allowCIDR("10.0.0.0/8", 443), want: "protected destination"},
		{name: "metadata CIDR", policy: allowCIDR("169.254.169.254/32", 443), want: "protected destination"},
		{name: "reserved DNS", policy: allowCIDR("93.184.216.0/24", 53), want: "reserved"},
		{name: "IP domain", policy: &runnerprotocol.NetworkPolicy{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST, Destinations: []*runnerprotocol.NetworkDestination{{Target: &runnerprotocol.NetworkDestination_Domain{Domain: "93.184.216.34"}, Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP, Port: 443}}}, want: "IP address"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := translateNetworkPolicy(test.policy)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTranslateNetworkPolicyAcceptsClosedModes(t *testing.T) {
	for _, policy := range []*runnerprotocol.NetworkPolicy{
		{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL},
		{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST},
	} {
		if _, err := translateNetworkPolicy(policy); err != nil {
			t.Fatal(err)
		}
	}
}

func allowCIDR(cidr string, port uint32) *runnerprotocol.NetworkPolicy {
	return &runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{{
			Target:   &runnerprotocol.NetworkDestination_Cidr{Cidr: cidr},
			Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP,
			Port:     port,
		}},
	}
}
