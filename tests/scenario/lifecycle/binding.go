package main

import (
	"net"
	"strings"
)

// The configured ceiling of a deployment, and which setting imposes it.
//
// A refusal code names the class of refusal, not the resource: ErrQuotaExceeded
// is one sentinel covering Sandboxes, active instances, memory, CPU, Snapshots
// and concurrent Operations alike. Naming the resource takes arithmetic over the
// configuration, which is why this mirrors the stress driver's calculation
// rather than sharing it. The two drivers duplicate deliberately; only the API
// client is shared between them.
type configuredLimit struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// minimumConfiguredLimit returns the smallest limit. Limits that tie are named
// together, because a deployment bound equally by two settings is not bound by
// whichever happens to be listed first.
func minimumConfiguredLimit(limits []configuredLimit) configuredLimit {
	binding := limits[0]
	for _, candidate := range limits[1:] {
		if candidate.Capacity < binding.Capacity {
			binding = candidate
		} else if candidate.Capacity == binding.Capacity {
			binding.Name += " + " + candidate.Name
		}
	}
	return binding
}

// guestIPCapacity is the number of guests a CIDR can address. Three addresses
// are unavailable: the network, the broadcast, and the bridge itself. A /24 —
// what the scenario harness allocates per run — therefore tops out at 253
// concurrent Sandboxes regardless of how much memory the host has.
func guestIPCapacity(rawCIDR string) int {
	ip, network, err := net.ParseCIDR(strings.TrimSpace(rawCIDR))
	if err != nil || ip.To4() == nil {
		return 0
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return 0
	}
	total := 1 << (bits - ones)
	if total <= 3 {
		return 0
	}
	return total - 3
}

// configuredBinding is the first ceiling the deployment will reach, and its
// name. Every term is a count of concurrent Sandboxes at the configured Profile,
// so they are directly comparable.
func (config lifecycleConfig) configuredBinding(guestCIDR string) configuredLimit {
	subjectBinding := minimumConfiguredLimit([]configuredLimit{
		{Name: "active instances", Capacity: int(config.SubjectMaxActiveInstances)},
		{Name: "Sandboxes", Capacity: int(config.SubjectMaxSandboxes)},
		{
			Name:     "CPU",
			Capacity: int(config.SubjectMaxCPUMillis / config.Profile.CPUMillis),
		},
		{
			Name:     "memory",
			Capacity: int(config.SubjectMaxMemoryBytes / config.Profile.MemoryBytes),
		},
		{Name: "concurrent Operations", Capacity: int(config.SubjectMaxConcurrentOperations)},
		{Name: "Snapshots", Capacity: int(config.SubjectMaxSnapshots)},
	})
	subjectBinding.Name = "subject quota: " + subjectBinding.Name
	return minimumConfiguredLimit([]configuredLimit{
		{
			Name:     "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL",
			Capacity: config.Runner.MaxConcurrentGlobal,
		},
		{
			Name: "runner CPU capacity",
			Capacity: int(
				int64(config.Runner.SandboxMaxVcpus*config.Runner.MaxConcurrentGlobal*1000) /
					config.Profile.CPUMillis,
			),
		},
		{
			Name:     "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB",
			Capacity: config.Runner.MemoryBudgetMiB / config.Runner.SandboxMemoryMiB,
		},
		{
			Name: "runner Workspace capacity",
			Capacity: int(
				(int64(config.Runner.SandboxDiskMiB*config.Runner.MaxConcurrentGlobal) << 20) /
					config.Profile.WorkspaceBytes,
			),
		},
		{
			Name: "runner Operations capacity",
			Capacity: config.Runner.MaxConcurrentOperationsGlobal /
				int(config.Profile.ConcurrentOperations),
		},
		{Name: "guest IP capacity", Capacity: guestIPCapacity(guestCIDR)},
		subjectBinding,
	})
}
