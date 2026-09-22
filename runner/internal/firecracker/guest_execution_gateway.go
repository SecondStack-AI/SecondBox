package firecracker

import (
	"fmt"
	"net/netip"
	"strings"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/networkpolicycontract"
	"google.golang.org/protobuf/proto"
)

const (
	executionGatewayEnvironment = "SECONDBOX_EXECUTION_GATEWAY"
	runnerGatewaysEnvironment   = "SECONDBOX_RUNNER_GATEWAYS"
)

// reservedGuestEnvironmentNames are Runner-owned; a caller that supplies one is
// refused before dispatch.
var reservedGuestEnvironmentNames = []string{executionGatewayEnvironment, runnerGatewaysEnvironment}

func validateExecutionGateway(attributed bool, endpoint netip.AddrPort) error {
	if !attributed {
		if endpoint != (netip.AddrPort{}) {
			return fmt.Errorf("ordinary execution cannot have an execution gateway")
		}
		return nil
	}
	address := endpoint.Addr()
	if !address.Is4() || !(address.IsGlobalUnicast() || address.IsLinkLocalUnicast()) || endpoint.Port() == 0 {
		return fmt.Errorf("attributed execution requires its host-owned IPv4 gateway endpoint")
	}
	return nil
}

func validateRunnerGateways(gateways []networkpolicy.LogicalGatewayEndpoint) error {
	for _, gateway := range gateways {
		name, err := networkpolicycontract.NormalizeLogicalGatewayName(gateway.LogicalName)
		if err != nil {
			return fmt.Errorf("Runner gateway logical name %q is invalid: %w", gateway.LogicalName, err)
		}
		if name != gateway.LogicalName {
			return fmt.Errorf("Runner gateway logical name %q is not canonical", gateway.LogicalName)
		}
		// Publication admits every address the egress-context loader and the
		// policy compiler admit; address-class policy belongs to the loader.
		// Only the attributed listener is restricted, to its IPv4 bridge address.
		address := gateway.Endpoint.Addr()
		if !address.IsValid() || address.Is4In6() || gateway.Endpoint.Port() == 0 {
			return fmt.Errorf("Runner gateway %q requires a valid unmapped IP endpoint with a nonzero port", gateway.LogicalName)
		}
	}
	return nil
}

// formatRunnerGateways renders the resolved logical gateway endpoints as
// space-separated logicalName=address:port entries, in the projected order.
func formatRunnerGateways(gateways []networkpolicy.LogicalGatewayEndpoint) string {
	entries := make([]string, 0, len(gateways))
	for _, gateway := range gateways {
		entries = append(entries, gateway.LogicalName+"="+gateway.Endpoint.String())
	}
	return strings.Join(entries, " ")
}

// The host supplies routing information; the listener supplies authority.
// Application wrappers choose proxy environment variables and HTTP semantics.
func (s *GuestProtocolSession) prepareReservedGuestEnvironment(request *guestv1.ExecRequest) (*guestv1.ExecRequest, error) {
	if err := validateExecutionGateway(s.attributedExecution != nil, s.executionGateway); err != nil {
		return nil, err
	}
	if err := validateRunnerGateways(s.runnerGateways); err != nil {
		return nil, err
	}
	for _, entry := range request.Environment {
		name := strings.TrimSpace(entry.GetName())
		for _, reserved := range reservedGuestEnvironmentNames {
			if name == reserved {
				return nil, fmt.Errorf("guest exec environment %s is reserved for the Runner", reserved)
			}
		}
	}
	injected := make([]*guestv1.EnvironmentEntry, 0, 2)
	if s.attributedExecution != nil {
		injected = append(injected, &guestv1.EnvironmentEntry{
			Name: executionGatewayEnvironment, Value: []byte(s.executionGateway.String()),
		})
	}
	if len(s.runnerGateways) > 0 {
		injected = append(injected, &guestv1.EnvironmentEntry{
			Name: runnerGatewaysEnvironment, Value: []byte(formatRunnerGateways(s.runnerGateways)),
		})
	}
	if len(injected) == 0 {
		return request, nil
	}
	prepared := proto.CloneOf(request)
	prepared.Environment = append(prepared.Environment, injected...)
	return prepared, nil
}
