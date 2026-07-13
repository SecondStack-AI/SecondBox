package microvm

import (
	"agentcy/internal/config"
	"agentcy/internal/runtimemanager"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

type RuntimeMetricsSnapshot = runtimemanager.RuntimeMetricsSnapshot

func (m *Manager) admitCompartmentSpawnLocked(key runtimeInstanceKey) error {
	liveForAgent := 0
	liveTotal := 0
	for _, inst := range m.instances {
		if inst == nil || inst.reaping {
			continue
		}
		liveTotal++
		if strings.TrimSpace(inst.agentID) != key.agentID {
			continue
		}
		liveForAgent++
		bridgeCIDR := ""
		if m.cfg != nil {
			bridgeCIDR = strings.TrimSpace(m.cfg.MicroVMBridgeCIDR)
		}
		if normalizeRuntimeCompartmentID(inst.compartmentID) != key.compartmentID && bridgeCIDR == "" {
			return fmt.Errorf("cannot start compartment %q for agent %q while another compartment is live without AG_MICROVM_BRIDGE_CIDR; single AG_MICROVM_GUEST_IP fallback cannot safely isolate concurrent compartment VMs", key.compartmentID, key.agentID)
		}
	}
	cap := 0
	if m.cfg != nil {
		cap = m.cfg.MicroVMMaxConcurrentPerAgent
	}
	if cap > 0 && liveForAgent >= cap {
		return fmt.Errorf("agent %q has reached AG_MICROVM_MAX_CONCURRENT_PER_AGENT=%d", key.agentID, cap)
	}
	globalCap := 0
	memoryBudgetMiB := 0
	memoryMiB := 0
	if m.cfg != nil {
		globalCap = m.cfg.MicroVMMaxConcurrentGlobal
		memoryBudgetMiB = m.cfg.MicroVMMemoryBudgetMiB
		memoryMiB = m.cfg.MicroVMMemoryMiB
	}
	if globalCap > 0 && liveTotal >= globalCap {
		return fmt.Errorf("host has reached AG_MICROVM_MAX_CONCURRENT_GLOBAL=%d", globalCap)
	}
	if memoryBudgetMiB > 0 && memoryMiB > 0 && (liveTotal+1)*memoryMiB > memoryBudgetMiB {
		return fmt.Errorf("projected microVM memory exceeds AG_MICROVM_MEMORY_BUDGET_MIB=%d", memoryBudgetMiB)
	}
	return nil
}

func (m *Manager) RuntimeMetricsSnapshot() RuntimeMetricsSnapshot {
	out := RuntimeMetricsSnapshot{ConcurrentVMsByAgent: map[string]int{}}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst == nil || inst.reaping || strings.TrimSpace(inst.agentID) == "" {
			continue
		}
		out.ConcurrentVMsByAgent[inst.agentID]++
		out.ConcurrentVMsTotal++
		if inst.warmToolVM {
			out.WarmToolVMs++
		}
	}
	out.GuestIPsInUse = len(m.guestIPs)
	if m.cfg != nil {
		out.GuestIPCapacity = microVMGuestIPCapacity(m.cfg)
		out.MaxConcurrentPerAgent = m.cfg.MicroVMMaxConcurrentPerAgent
		out.MaxConcurrentGlobal = m.cfg.MicroVMMaxConcurrentGlobal
		out.MemoryBudgetMiB = m.cfg.MicroVMMemoryBudgetMiB
		if m.cfg.MicroVMMemoryMiB > 0 {
			out.MemoryReservedMiB = out.ConcurrentVMsTotal * m.cfg.MicroVMMemoryMiB
		}
	}
	out.ColdStartCount = len(m.startDurations)
	if len(m.startDurations) > 0 {
		durations := append([]time.Duration(nil), m.startDurations...)
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		idx := int(float64(len(durations))*0.95+0.999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		out.ColdStartP95 = durations[idx]
	}
	return out
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

func microVMGuestIPCapacity(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	if cidr := strings.TrimSpace(cfg.MicroVMBridgeCIDR); cidr != "" {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil || ip.To4() == nil {
			return 0
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 32 || ones > 30 {
			return 0
		}
		total := 1 << (bits - ones)
		// Network + broadcast are unusable, and the bridge gateway itself is
		// reserved for the host. reserveGuestIP starts allocating after it.
		if total <= 3 {
			return 0
		}
		return total - 3
	}
	if strings.TrimSpace(cfg.MicroVMGuestIP) != "" {
		return 1
	}
	return 0
}
