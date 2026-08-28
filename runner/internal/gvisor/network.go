//go:build linux

package gvisor

import (
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// translateNetworkPolicy passes the assignment policy through the shared
// provider-neutral compiler, keeping protected address classes, exact-domain
// normalization, reserved DNS ports, destination bounds, and protocol
// validation identical to the Firecracker path. The compiled policy is
// enforced exactly by the inet-family veth rendering; a rule the compiler
// rejects never reaches enforcement, and nothing is omitted or broadened.
func translateNetworkPolicy(policy *runnerprotocol.NetworkPolicy, options networkpolicy.CompileOptions) (*networkpolicy.CompiledPolicy, error) {
	portable, err := networkpolicy.FromRunnerProtocol(policy)
	if err != nil {
		return nil, err
	}
	return networkpolicy.Compile(portable, options)
}
