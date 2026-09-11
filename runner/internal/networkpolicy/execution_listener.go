package networkpolicy

import (
	"fmt"
	"net/netip"
)

// CompileExecutionListener permits only the host-owned forwarding endpoint.
// Ordinary gateway mappings and DNS authority are not inherited.
func CompileExecutionListener(endpoint netip.AddrPort, options CompileOptions) (*CompiledPolicy, error) {
	address := endpoint.Addr()
	if !address.Is4() || !(address.IsGlobalUnicast() || address.IsLinkLocalUnicast()) || endpoint.Port() == 0 {
		return nil, fmt.Errorf("SecondBox execution listener endpoint must be a unicast IPv4 address and nonzero port")
	}
	options.RunnerGateways = nil
	compiled, err := Compile(Policy{Mode: ModeAllowList}, options)
	if err != nil {
		return nil, err
	}
	compiled.protectedAddresses[address] = struct{}{}
	compiled.executionListener = endpoint
	return compiled, nil
}
