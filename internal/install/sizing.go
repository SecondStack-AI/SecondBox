package install

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

const (
	DefaultSandboxProfile            = standardresources.DurableCoding
	DefaultControlReserveVCPUCount   = int64(2)
	DefaultControlReserveMemoryBytes = int64(4 << 30)
)

// CapacityOverrides selects the standard Profile that sizes a new install and
// the control reserve kept for the host OS, Docker, and SecondBox services.
// A zero field keeps its default.
type CapacityOverrides struct {
	SandboxProfile            string
	ControlReserveVCPUCount   int64
	ControlReserveMemoryBytes int64
}

type capacitySizing struct {
	profile            string
	resources          contracts.ResourcePolicy
	reserveVCPUCount   int64
	reserveMemoryBytes int64
}

// Validate rejects an unknown Profile or a non-positive reserve before any
// host inspection; host fit is checked by preflight and the planner.
func (overrides CapacityOverrides) Validate() error {
	_, err := overrides.resolve()
	return err
}

func (overrides CapacityOverrides) resolve() (capacitySizing, error) {
	sizing := capacitySizing{profile: overrides.SandboxProfile, reserveVCPUCount: overrides.ControlReserveVCPUCount, reserveMemoryBytes: overrides.ControlReserveMemoryBytes}
	if sizing.profile == "" {
		sizing.profile = DefaultSandboxProfile
	}
	if sizing.reserveVCPUCount == 0 {
		sizing.reserveVCPUCount = DefaultControlReserveVCPUCount
	}
	if sizing.reserveMemoryBytes == 0 {
		sizing.reserveMemoryBytes = DefaultControlReserveMemoryBytes
	}
	if sizing.reserveVCPUCount < 0 || sizing.reserveMemoryBytes < 0 {
		return capacitySizing{}, installerError("control reserve must be positive", nil)
	}
	resources, err := SandboxProfileResources(sizing.profile)
	if err != nil {
		return capacitySizing{}, err
	}
	sizing.resources = resources
	return sizing, nil
}

func (sizing capacitySizing) minimumHostCPUCount() int64 {
	return sizing.reserveVCPUCount + sizing.resources.VCPUCount
}

func (sizing capacitySizing) minimumHostMemoryBytes() int64 {
	return sizing.reserveMemoryBytes + sizing.resources.MemoryBytes
}

// SandboxProfileResources returns the current resource policy of a standard
// Profile that can size a single-host install.
func SandboxProfileResources(name string) (contracts.ResourcePolicy, error) {
	if !slices.Contains(standardresources.BundleNames(), name) {
		return contracts.ResourcePolicy{}, installerError(fmt.Sprintf("sandbox Profile %q is not a standard Profile; choose one of %v", name, standardresources.BundleNames()), nil)
	}
	profile, err := standardresources.ProfileLineage(name, standardresources.ArchitectureAMD64)
	if err != nil {
		return contracts.ResourcePolicy{}, installerError("sandbox Profile resources", err)
	}
	return profile.Revisions[len(profile.Revisions)-1].Spec.Resources, nil
}

// SandboxProfile names the standard Profile that sized the plan. Plans sized
// for durable-coding, including every plan accepted before the choice existed,
// do not record it.
func (plan InstallPlan) SandboxProfile() string {
	if plan.SizedSandboxProfile == "" {
		return DefaultSandboxProfile
	}
	return plan.SizedSandboxProfile
}

// SmokeProfiles returns the Profiles the post-install smoke boots: the
// isolated Profile, then the egress-context-requiring Profile closest to the
// sizing Profile.
func SmokeProfiles(sandboxProfile string) []string {
	egress := sandboxProfile
	if egress == standardresources.AgentCompartmentIsolated {
		egress = standardresources.AgentCompartment
	}
	return []string{standardresources.AgentCompartmentIsolated, egress}
}

func formatMemoryBytes(value int64) string {
	if value%(1<<30) == 0 {
		return strconv.FormatInt(value>>30, 10) + " GiB"
	}
	return strconv.FormatInt(value>>20, 10) + " MiB"
}
