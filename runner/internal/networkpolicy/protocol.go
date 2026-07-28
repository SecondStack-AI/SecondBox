package networkpolicy

import (
	"fmt"
	"net/netip"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// FromRunnerProtocol converts one profile-resolved assignment policy without
// supplying any runner-local bounds or protected-address defaults.
func FromRunnerProtocol(source *runnerprotocol.NetworkPolicy) (Policy, error) {
	if source == nil {
		return Policy{}, fmt.Errorf("SecondBox runner assignment network policy is required")
	}
	var mode Mode
	switch source.Mode {
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL:
		mode = ModeDenyAll
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST:
		mode = ModeAllowList
	default:
		return Policy{}, fmt.Errorf("SecondBox runner assignment network policy mode is unspecified")
	}
	destinations := make([]Destination, 0, len(source.Destinations))
	for index, sourceDestination := range source.Destinations {
		if sourceDestination == nil {
			return Policy{}, fmt.Errorf("SecondBox runner assignment network destination %d is missing", index)
		}
		var protocol Protocol
		switch sourceDestination.Protocol {
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP:
			protocol = ProtocolTCP
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTP:
			protocol = ProtocolHTTP
		case runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS:
			protocol = ProtocolHTTPS
		default:
			return Policy{}, fmt.Errorf(
				"SecondBox runner assignment network destination %d protocol is unspecified",
				index,
			)
		}
		destination := Destination{
			Protocol: protocol,
			Port:     uint16(sourceDestination.Port),
		}
		if sourceDestination.Port == 0 || sourceDestination.Port > 65535 {
			return Policy{}, fmt.Errorf(
				"SecondBox runner assignment network destination %d port is invalid",
				index,
			)
		}
		switch target := sourceDestination.Target.(type) {
		case *runnerprotocol.NetworkDestination_Domain:
			destination.Domain = target.Domain
		case *runnerprotocol.NetworkDestination_Cidr:
			prefix, err := netip.ParsePrefix(target.Cidr)
			if err != nil {
				return Policy{}, fmt.Errorf(
					"SecondBox runner assignment network destination %d CIDR: %w",
					index,
					err,
				)
			}
			destination.Prefix = prefix
		default:
			return Policy{}, fmt.Errorf(
				"SecondBox runner assignment network destination %d target is unspecified",
				index,
			)
		}
		destinations = append(destinations, destination)
	}
	return Policy{Mode: mode, Destinations: destinations}, nil
}
