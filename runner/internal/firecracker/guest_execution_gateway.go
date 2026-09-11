package firecracker

import (
	"fmt"
	"net/netip"
	"strings"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/protobuf/proto"
)

const executionGatewayEnvironment = "SECONDBOX_EXECUTION_GATEWAY"

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

// The host supplies routing information; the listener supplies authority.
// Application wrappers choose proxy environment variables and HTTP semantics.
func (s *GuestProtocolSession) prepareExecutionGateway(request *guestv1.ExecRequest) (*guestv1.ExecRequest, error) {
	if err := validateExecutionGateway(s.attributedExecution != nil, s.executionGateway); err != nil {
		return nil, err
	}
	for _, entry := range request.Environment {
		if strings.TrimSpace(entry.GetName()) == executionGatewayEnvironment {
			return nil, fmt.Errorf("guest exec environment SECONDBOX_EXECUTION_GATEWAY is reserved for the Runner")
		}
	}
	if s.attributedExecution == nil {
		return request, nil
	}
	prepared := proto.CloneOf(request)
	prepared.Environment = append(prepared.Environment, &guestv1.EnvironmentEntry{
		Name: executionGatewayEnvironment, Value: []byte(s.executionGateway.String()),
	})
	return prepared, nil
}
