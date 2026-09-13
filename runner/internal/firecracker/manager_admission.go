package firecracker

import (
	"fmt"
	"slices"
	"strings"
	"time"

	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func (m *Manager) admitCompartmentSpawnLocked(
	key runtimeInstanceKey,
	requestedMemoryMiB int,
) error {
	liveForSandbox := 0
	liveTotal := 0
	reservedMemoryMiB := 0
	for _, inst := range m.instances {
		if inst == nil {
			continue
		}
		liveTotal++
		instanceMemoryMiB := inst.memoryMiB
		if instanceMemoryMiB <= 0 {
			instanceMemoryMiB = m.defaultMemoryMiB()
		}
		reservedMemoryMiB += instanceMemoryMiB
		if strings.TrimSpace(inst.sandboxID) != key.sandboxID {
			continue
		}
		liveForSandbox++
		bridgeCIDR := ""
		if m.cfg != nil {
			bridgeCIDR = strings.TrimSpace(m.cfg.MicroVMBridgeCIDR)
		}
		if normalizeRuntimeCompartmentID(inst.compartmentID) != key.compartmentID && bridgeCIDR == "" {
			return fmt.Errorf("cannot start compartment %q for sandbox %q while another compartment is live without SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR; single SECONDBOX_RUNNER_SANDBOX_GUEST_IP cannot safely isolate concurrent compartment VMs", key.compartmentID, key.sandboxID)
		}
	}
	for pendingKey, count := range m.pendingSpawns {
		if count <= 0 {
			continue
		}
		liveTotal += count
		pendingMemoryMiB := m.pendingMemoryMiB[pendingKey]
		if pendingMemoryMiB <= 0 {
			pendingMemoryMiB = count * m.defaultMemoryMiB()
		}
		reservedMemoryMiB += pendingMemoryMiB
		if pendingKey.sandboxID != key.sandboxID {
			continue
		}
		liveForSandbox += count
		bridgeCIDR := ""
		if m.cfg != nil {
			bridgeCIDR = strings.TrimSpace(m.cfg.MicroVMBridgeCIDR)
		}
		if pendingKey.compartmentID != key.compartmentID && bridgeCIDR == "" {
			return fmt.Errorf("cannot start compartment %q for sandbox %q while another compartment is pending without SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR; single SECONDBOX_RUNNER_SANDBOX_GUEST_IP cannot safely isolate concurrent compartment VMs", key.compartmentID, key.sandboxID)
		}
	}
	cap := 0
	if m.cfg != nil {
		cap = m.cfg.MicroVMMaxConcurrentPerSandbox
	}
	if cap > 0 && liveForSandbox >= cap {
		return fmt.Errorf("sandbox %q has reached SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX=%d", key.sandboxID, cap)
	}
	globalCap := 0
	memoryBudgetMiB := 0
	if m.cfg != nil {
		globalCap = m.cfg.MicroVMMaxConcurrentGlobal
		memoryBudgetMiB = m.cfg.MicroVMMemoryBudgetMiB
	}
	if globalCap > 0 && liveTotal >= globalCap {
		return fmt.Errorf("runner has reached SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL=%d", globalCap)
	}
	if memoryBudgetMiB > 0 &&
		requestedMemoryMiB > 0 &&
		reservedMemoryMiB+requestedMemoryMiB > memoryBudgetMiB {
		return fmt.Errorf("projected microVM memory exceeds SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB=%d", memoryBudgetMiB)
	}
	return nil
}

func (m *Manager) reserveCompartmentSpawnLocked(key runtimeInstanceKey, requestedMemoryMiB int) error {
	if err := m.admitCompartmentSpawnLocked(key, requestedMemoryMiB); err != nil {
		return err
	}
	if m.pendingSpawns == nil {
		m.pendingSpawns = map[runtimeInstanceKey]int{}
	}
	if m.pendingMemoryMiB == nil {
		m.pendingMemoryMiB = map[runtimeInstanceKey]int{}
	}
	m.pendingSpawns[key]++
	m.pendingMemoryMiB[key] += requestedMemoryMiB
	return nil
}

func (m *Manager) releaseCompartmentSpawnLocked(key runtimeInstanceKey, requestedMemoryMiB int) {
	if m.pendingSpawns[key] <= 1 {
		delete(m.pendingSpawns, key)
		delete(m.pendingMemoryMiB, key)
		return
	}
	m.pendingSpawns[key]--
	m.pendingMemoryMiB[key] -= requestedMemoryMiB
}

func (m *Manager) defaultMemoryMiB() int {
	if m == nil || m.cfg == nil {
		return 0
	}
	return m.cfg.MicroVMMemoryMiB
}

func (m *Manager) requestedMemoryMiB(opts runtimemanager.StartOpts) int {
	memoryMiB := m.defaultMemoryMiB()
	if opts.SandboxPolicy != nil && opts.SandboxPolicy.MemoryMiB > 0 {
		memoryMiB = opts.SandboxPolicy.MemoryMiB
	}
	return memoryMiB
}

// StartupTiming reports the sample count and nearest-rank p95 of recent starts.
func (m *Manager) StartupTiming() (uint64, time.Duration) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.startDurations) == 0 {
		return 0, 0
	}
	durations := slices.Clone(m.startDurations)
	slices.Sort(durations)
	index := (len(durations)*95 + 99) / 100
	return uint64(len(durations)), durations[index-1]
}

func (m *Manager) recordStartDuration(duration time.Duration) {
	if m == nil || duration < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	const maxStartDurationSamples = 256
	m.startDurations = append(m.startDurations, duration)
	if len(m.startDurations) > maxStartDurationSamples {
		copy(m.startDurations, m.startDurations[len(m.startDurations)-maxStartDurationSamples:])
		m.startDurations = m.startDurations[:maxStartDurationSamples]
	}
}
