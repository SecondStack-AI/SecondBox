package microsandbox

import (
	"fmt"
	"time"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const microsandboxPolicyValidationPins = 64
const microsandboxPolicyValidationTTL = 5 * time.Minute

// translateNetworkPolicy first passes through the shared provider-neutral
// compiler. This keeps protected address classes, exact-domain normalization,
// reserved DNS ports, destination bounds, and protocol validation identical to
// the Firecracker path before the private helper representation is constructed.
func translateNetworkPolicy(policy *runnerprotocol.NetworkPolicy) (*microsandboxprotocol.HelperNetworkPolicy, error) {
	portable, err := networkpolicy.FromRunnerProtocol(policy)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox network policy: %w", err)
	}
	compiled, err := networkpolicy.Compile(portable, networkpolicy.CompileOptions{
		MaximumPins: microsandboxPolicyValidationPins,
		MaximumTTL:  microsandboxPolicyValidationTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox network policy: %w", err)
	}
	result := &microsandboxprotocol.HelperNetworkPolicy{}
	switch compiled.Mode() {
	case networkpolicy.ModeDenyAll:
		result.Mode = microsandboxprotocol.HelperNetworkPolicyMode_HELPER_NETWORK_POLICY_MODE_DENY_ALL
	case networkpolicy.ModeAllowList:
		result.Mode = microsandboxprotocol.HelperNetworkPolicyMode_HELPER_NETWORK_POLICY_MODE_ALLOW_LIST
	default:
		return nil, fmt.Errorf("SecondBox Microsandbox compiled network policy mode is unsupported")
	}
	for _, destination := range compiled.Destinations() {
		translated := &microsandboxprotocol.HelperNetworkDestination{Port: uint32(destination.Port)}
		if destination.Domain != "" {
			translated.Target = &microsandboxprotocol.HelperNetworkDestination_Domain{Domain: destination.Domain}
		} else {
			translated.Target = &microsandboxprotocol.HelperNetworkDestination_Cidr{Cidr: destination.Prefix.String()}
		}
		switch destination.Protocol {
		case networkpolicy.ProtocolTCP:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_TCP
		case networkpolicy.ProtocolHTTP:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTP
		case networkpolicy.ProtocolHTTPS:
			translated.Protocol = microsandboxprotocol.HelperNetworkProtocol_HELPER_NETWORK_PROTOCOL_HTTPS
		default:
			return nil, fmt.Errorf("SecondBox Microsandbox compiled network protocol is unsupported")
		}
		result.Destinations = append(result.Destinations, translated)
	}
	return result, nil
}
