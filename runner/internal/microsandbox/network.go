package microsandbox

import (
	"fmt"
	"net/netip"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// translateNetworkPolicy first passes through the shared provider-neutral
// compiler. This keeps protected address classes, exact-domain normalization,
// reserved DNS ports, destination bounds, and protocol validation identical to
// the Firecracker path before the private helper representation is constructed.
func translateNetworkPolicy(policy *runnerprotocol.NetworkPolicy, options networkpolicy.CompileOptions) (*microsandboxprotocol.HelperNetworkPolicy, error) {
	portable, err := networkpolicy.FromRunnerProtocol(policy)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox network policy: %w", err)
	}
	compiled, err := networkpolicy.Compile(portable, options)
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
	gatewayDestinations := compiled.RunnerGatewayDestinations()
	for _, gateway := range gatewayDestinations {
		bits := 128
		if gateway.Address.Is4() {
			bits = 32
		}
		translated, err := translatedMicrosandboxDestination(
			gateway.Destination,
			netip.PrefixFrom(gateway.Address, bits).String(),
			false,
		)
		if err != nil {
			return nil, err
		}
		result.RunnerGateways = append(result.RunnerGateways, translated)
	}
	for _, prefix := range compiled.ProtectedPrefixes() {
		result.ProtectedCidrs = append(result.ProtectedCidrs, prefix.String())
	}
	for _, destination := range compiled.Destinations() {
		if destination.Domain != "" {
			if _, selectedGateway := options.RunnerGateways[destination.Domain]; selectedGateway {
				continue
			}
		}
		translated, err := translatedMicrosandboxDestination(
			destination,
			destination.Prefix.String(),
			destination.Domain != "",
		)
		if err != nil {
			return nil, err
		}
		result.Destinations = append(result.Destinations, translated)
	}
	return result, nil
}

func translatedMicrosandboxDestination(destination networkpolicy.Destination, cidr string, domain bool) (*microsandboxprotocol.HelperNetworkDestination, error) {
	translated := &microsandboxprotocol.HelperNetworkDestination{Port: uint32(destination.Port)}
	if domain {
		translated.Target = &microsandboxprotocol.HelperNetworkDestination_Domain{Domain: destination.Domain}
	} else {
		translated.Target = &microsandboxprotocol.HelperNetworkDestination_Cidr{Cidr: cidr}
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
	return translated, nil
}
