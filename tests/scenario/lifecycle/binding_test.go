package main

import "testing"

// bindingConfig is deliberately generous everywhere, so each test can lower one
// setting and assert that it becomes the binding one.
func bindingConfig() lifecycleConfig {
	config := lifecycleConfig{
		Runner: runnerLimits{
			SandboxMaxVcpus:               2,
			SandboxMemoryMiB:              512,
			SandboxDiskMiB:                1024,
			MemoryBudgetMiB:               512 * 1000,
			MaxConcurrentGlobal:           1000,
			MaxConcurrentOperationsGlobal: 4000,
		},
		Profile: profileLimits{
			VCPUCount:            1,
			MemoryBytes:          512 << 20,
			WorkspaceBytes:       1 << 30,
			ConcurrentOperations: 4,
			DataPlaneTransport:   "proxied",
		},
		SubjectMaxActiveInstances:      1000,
		SubjectMaxSandboxes:            1000,
		SubjectMaxConcurrentOperations: 1000,
		SubjectMaxSnapshots:            1000,
		SubjectMaxVCPUCount:            1000,
		SubjectMaxMemoryBytes:          1000 * (512 << 20),
	}
	return config
}

func TestConfiguredBindingNamesTheSmallestLimit(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mutate   func(*lifecycleConfig)
		capacity int
		binding  string
	}{
		{
			name:     "runner memory budget",
			mutate:   func(c *lifecycleConfig) { c.Runner.MemoryBudgetMiB = 4096 },
			capacity: 8,
			binding:  "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB",
		},
		{
			name:     "subject active instances",
			mutate:   func(c *lifecycleConfig) { c.SubjectMaxActiveInstances = 12 },
			capacity: 12,
			binding:  "subject quota: active instances",
		},
		{
			// Workspace capacity is derived from MaxConcurrentGlobal, so the
			// per-Sandbox disk has to grow for the global cap to bind alone.
			name: "runner concurrent global",
			mutate: func(c *lifecycleConfig) {
				c.Runner.MaxConcurrentGlobal = 16
				c.Runner.SandboxDiskMiB = 2048
			},
			capacity: 16,
			binding:  "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL",
		},
		{
			name: "runner workspace capacity",
			mutate: func(c *lifecycleConfig) {
				c.Runner.MaxConcurrentGlobal = 40
				c.Runner.SandboxDiskMiB = 512
			},
			capacity: 20,
			binding:  "runner Workspace capacity",
		},
		{
			name:     "runner operations capacity",
			mutate:   func(c *lifecycleConfig) { c.Runner.MaxConcurrentOperationsGlobal = 64 },
			capacity: 16,
			binding:  "runner Operations capacity",
		},
		{
			name:     "subject memory quota",
			mutate:   func(c *lifecycleConfig) { c.SubjectMaxMemoryBytes = 20 * (512 << 20) },
			capacity: 20,
			binding:  "subject quota: memory",
		},
		{
			name:     "subject CPU quota",
			mutate:   func(c *lifecycleConfig) { c.SubjectMaxVCPUCount = 24 },
			capacity: 24,
			binding:  "subject quota: CPU",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := bindingConfig()
			testCase.mutate(&config)
			binding := config.configuredBinding("198.18.0.0/24")
			if binding.Capacity != testCase.capacity {
				t.Fatalf(
					"binding = %s at %d, want %d",
					binding.Name, binding.Capacity, testCase.capacity,
				)
			}
			if binding.Name != testCase.binding {
				t.Fatalf("binding name = %q, want %q", binding.Name, testCase.binding)
			}
		})
	}
}

// The scenario harness allocates one /24 per run, so guest addresses cap a run
// at 253 concurrent Sandboxes however much memory the host has.
func TestGuestIPCapacityBoundsTheRunAtTheAllocatedPrefix(t *testing.T) {
	for _, testCase := range []struct {
		cidr     string
		capacity int
	}{
		{cidr: "198.18.0.0/24", capacity: 253},
		{cidr: "198.18.0.0/23", capacity: 509},
		{cidr: "198.18.0.0/30", capacity: 1},
		{cidr: "198.18.0.0/31", capacity: 0},
		{cidr: "not-a-cidr", capacity: 0},
		{cidr: "2001:db8::/64", capacity: 0},
	} {
		t.Run(testCase.cidr, func(t *testing.T) {
			if capacity := guestIPCapacity(testCase.cidr); capacity != testCase.capacity {
				t.Fatalf("guest IP capacity = %d, want %d", capacity, testCase.capacity)
			}
		})
	}
}

func TestConfiguredBindingIsGuestAddressesWhenEverythingElseIsGenerous(t *testing.T) {
	binding := bindingConfig().configuredBinding("198.18.0.0/24")
	if binding.Capacity != 253 || binding.Name != "guest IP capacity" {
		t.Fatalf("binding = %s at %d, want guest IP capacity at 253", binding.Name, binding.Capacity)
	}
}

// A deployment bound equally by two settings is not bound by whichever happens
// to be listed first, so ties name both.
func TestConfiguredBindingNamesEveryTiedLimit(t *testing.T) {
	config := bindingConfig()
	config.SubjectMaxActiveInstances = 16
	config.SubjectMaxSandboxes = 16
	binding := config.configuredBinding("198.18.0.0/24")
	if binding.Capacity != 16 {
		t.Fatalf("binding capacity = %d, want 16", binding.Capacity)
	}
	if binding.Name != "subject quota: active instances + Sandboxes" {
		t.Fatalf("tied binding name = %q", binding.Name)
	}
}
