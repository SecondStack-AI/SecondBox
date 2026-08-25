//go:build linux

package gvisor

import (
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const (
	gvisorPolicyValidationPins = 64
	gvisorPolicyValidationTTL  = 5 * time.Minute
)

// validateNetworkPolicy first passes through the shared provider-neutral
// compiler, keeping protected address classes, exact-domain normalization,
// reserved DNS ports, destination bounds, and protocol validation identical
// to the other backends. Until the routed-veth enforcement lands, only
// deny_all has an exact runnable representation — the sandbox runs with
// runsc --network=none, which is total isolation — so every allow-list is
// rejected rather than started unenforced.
func validateNetworkPolicy(policy *runnerprotocol.NetworkPolicy) error {
	portable, err := networkpolicy.FromRunnerProtocol(policy)
	if err != nil {
		return err
	}
	compiled, err := networkpolicy.Compile(portable, networkpolicy.CompileOptions{
		MaximumPins: gvisorPolicyValidationPins,
		MaximumTTL:  gvisorPolicyValidationTTL,
	})
	if err != nil {
		return err
	}
	if compiled.Mode() != networkpolicy.ModeDenyAll {
		return fmt.Errorf("SecondBox gVisor network enforcement supports deny_all only; the allow-list has no exact representation yet")
	}
	return nil
}
