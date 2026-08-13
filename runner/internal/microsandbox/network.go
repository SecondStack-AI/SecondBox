package microsandbox

import (
	"fmt"
	"net"
	"strings"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func translateNetworkPolicy(policy *runnerprotocol.NetworkPolicy) (*microsandboxprotocol.HelperNetworkPolicy, error) {
	if policy == nil {
		return nil, fmt.Errorf("SecondBox Microsandbox network policy is required")
	}
	result := &microsandboxprotocol.HelperNetworkPolicy{}
	switch policy.Mode {
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL:
		if len(policy.Destinations) != 0 {
			return nil, fmt.Errorf("SecondBox Microsandbox deny-all policy contains destinations")
		}
		result.Mode = microsandboxprotocol.HelperNetworkPolicyMode_HELPER_NETWORK_POLICY_MODE_DENY_ALL
		return result, nil
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST:
		if len(policy.Destinations) == 0 {
			return nil, fmt.Errorf("SecondBox Microsandbox allow-list policy is empty")
		}
		result.Mode = microsandboxprotocol.HelperNetworkPolicyMode_HELPER_NETWORK_POLICY_MODE_ALLOW_LIST
	default:
		return nil, fmt.Errorf("SecondBox Microsandbox network policy mode is unsupported")
	}
	for _, destination := range policy.Destinations {
		if destination == nil || destination.Port == 0 || destination.Port > 65535 {
			return nil, fmt.Errorf("SecondBox Microsandbox network destination is incomplete")
		}
		translated := &microsandboxprotocol.HelperNetworkDestination{Port: destination.Port}
		switch target := destination.Target.(type) {
		case *runnerprotocol.NetworkDestination_Domain:
			domain := strings.ToLower(strings.TrimSpace(target.Domain))
			if !validDomain(domain) {
				return nil, fmt.Errorf("SecondBox Microsandbox network domain is invalid")
			}
			translated.Target = &microsandboxprotocol.HelperNetworkDestination_Domain{Domain: domain}
		case *runnerprotocol.NetworkDestination_Cidr:
			_, parsed, err := net.ParseCIDR(strings.TrimSpace(target.Cidr))
			if err != nil || parsed.String() != strings.TrimSpace(target.Cidr) {
				return nil, fmt.Errorf("SecondBox Microsandbox network CIDR is not canonical")
			}
			translated.Target = &microsandboxprotocol.HelperNetworkDestination_Cidr{Cidr: parsed.String()}
		default:
			return nil, fmt.Errorf("SecondBox Microsandbox network destination target is unsupported")
		}
		switch destination.Protocol {
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_TCP
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTP:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTP
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTPS
		default:
			return nil, fmt.Errorf("SecondBox Microsandbox network destination protocol is unsupported")
		}
		result.Destinations = append(result.Destinations, translated)
	}
	return result, nil
}

func validDomain(value string) bool {
	if value == "" || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}
