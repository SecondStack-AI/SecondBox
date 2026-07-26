package microvm

import (
	"agent-manager/internal/flow"
	"agent-manager/internal/runtimemanager"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"time"
)

type warmToolLease struct {
	instanceID string
	reused     bool
	startVMMs  int64
}

func (m *Manager) warmToolInflightLocked() int {
	total := 0
	for _, inst := range m.instances {
		if inst != nil && inst.warmToolVM && !inst.reaping && inst.inflight > 0 {
			total += inst.inflight
		}
	}
	return total
}

func (m *Manager) sweepIdleToolVMs(now time.Time) int {
	if m == nil {
		return 0
	}
	ttl := 90 * time.Second
	if m.cfg != nil && m.cfg.MicroVMToolVMIdleTTL > 0 {
		ttl = m.cfg.MicroVMToolVMIdleTTL
	}
	var victims []*instance
	m.mu.Lock()
	for _, inst := range m.instances {
		if inst == nil || !inst.warmToolVM || inst.reaping || inst.draining || inst.inflight > 0 {
			continue
		}
		if inst.lastUsedAt.IsZero() || !now.After(inst.lastUsedAt.Add(ttl)) {
			continue
		}
		inst.reaping = true
		victims = append(victims, inst)
	}
	m.mu.Unlock()
	for _, inst := range victims {
		go m.teardownWarmToolVM(inst)
	}
	return len(victims)
}

func (m *Manager) ExecuteEphemeralTool(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, req ToolExecRequest) (string, ToolExecResponse, error) {
	started := time.Now()
	opts.CompartmentID = compartmentID
	opts.RuntimeClass = runtimemanager.RuntimeClassToolExecutor
	opts.Ephemeral = true
	unlock := m.lockSerializedCompartmentMount(agentID, compartmentID)
	defer unlock()
	startVMStarted := time.Now()
	instanceID, err := m.createAndStart(ctx, agentID, opts)
	startVMMs := time.Since(startVMStarted).Milliseconds()
	if err != nil {
		slog.Warn("microvm tool executor timing", "agent", agentID, "compartment", compartmentID, "operation", req.Operation, "reusable", false, "status", "failed", "stage", "start", "totalMs", time.Since(started).Milliseconds(), "startVMMs", startVMMs, "error", err)
		return "", ToolExecResponse{}, err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Remove(stopCtx, instanceID); err != nil {
			slog.Warn("failed to tear down ephemeral tool microVM", "agent", agentID, "compartment", compartmentID, "instance", instanceID, "error", err)
		}
	}()
	execStarted := time.Now()
	resp, execErr := m.ExecuteTool(ctx, instanceID, req)
	execMs := time.Since(execStarted).Milliseconds()
	var freezeMs int64
	if toolRequestWritesWorkspace(req) {
		freezeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		freezeStarted := time.Now()
		if _, err := m.FreezeWorkspace(freezeCtx, instanceID); err != nil {
			slog.Warn("failed to freeze ephemeral tool workspace before teardown", "agent", agentID, "compartment", compartmentID, "instance", instanceID, "error", err)
		}
		freezeMs = time.Since(freezeStarted).Milliseconds()
	}
	m.logMicroVMToolTiming(agentID, compartmentID, "", instanceID, req, false, false, started, startVMMs, execMs, freezeMs, execErr, resp)
	return instanceID, resp, execErr
}

func (m *Manager) acquireWarmToolVM(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (_ warmToolLease, err error) {
	agentID = strings.TrimSpace(agentID)
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return warmToolLease{}, err
	}
	opts.CompartmentID = compartmentID
	opts.RuntimeClass = runtimemanager.RuntimeClassToolExecutor
	key := runtimeInstanceKey{agentID: agentID, compartmentID: compartmentID}

	for {
		if err := ctx.Err(); err != nil {
			return warmToolLease{}, err
		}
		fingerprint, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return warmToolLease{}, err
		}

		m.mu.Lock()
		if m.shuttingDown {
			m.mu.Unlock()
			return warmToolLease{}, fmt.Errorf("microVM manager is shutting down")
		}
		if m.instancesByKey == nil {
			m.instancesByKey = map[runtimeInstanceKey]string{}
		}
		if m.provisioning == nil {
			m.provisioning = map[runtimeInstanceKey]chan struct{}{}
		}
		if id := m.instancesByKey[key]; id != "" {
			inst := m.instances[id]
			if inst == nil {
				delete(m.instancesByKey, key)
				m.mu.Unlock()
				continue
			}
			done := inst.done
			if inst.reaping || inst.draining {
				m.mu.Unlock()
				if err := waitWarmInstanceDone(ctx, done); err != nil {
					return warmToolLease{}, err
				}
				if err := waitFirecrackerProcessGone(ctx, id); err != nil {
					return warmToolLease{}, err
				}
				continue
			}
			if inst.startupFingerprint == fingerprint {
				inst.inflight++
				inst.lastUsedAt = time.Now().UTC()
				m.mu.Unlock()
				return warmToolLease{instanceID: id, reused: true}, nil
			}
			m.mu.Unlock()

			confirm, err := m.startupFingerprint(agentID, compartmentID, opts)
			if err != nil {
				return warmToolLease{}, err
			}
			m.mu.Lock()
			inst = m.instances[id]
			if inst == nil || m.instancesByKey[key] != id {
				m.mu.Unlock()
				continue
			}
			if inst.startupFingerprint == confirm && !inst.reaping && !inst.draining {
				inst.inflight++
				inst.lastUsedAt = time.Now().UTC()
				m.mu.Unlock()
				return warmToolLease{instanceID: id, reused: true}, nil
			}
			if !inst.draining {
				inst.draining = true
			}
			done = inst.done
			m.promoteWarmTeardownLocked(inst)
			m.mu.Unlock()
			if err := waitWarmInstanceDone(ctx, done); err != nil {
				return warmToolLease{}, err
			}
			continue
		}
		if ch := m.provisioning[key]; ch != nil {
			m.mu.Unlock()
			if err := waitWarmProvisioning(ctx, ch); err != nil {
				return warmToolLease{}, err
			}
			continue
		}
		ch := make(chan struct{})
		m.provisioning[key] = ch
		m.mu.Unlock()

		started := time.Now()
		var instanceID string
		bootErr := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("warm tool VM boot panic: %v", r)
				}
			}()
			instanceID, err = m.createAndStart(ctx, agentID, opts)
			return err
		}()
		startVMMs := time.Since(started).Milliseconds()

		m.mu.Lock()
		if m.provisioning[key] == ch {
			delete(m.provisioning, key)
			close(ch)
		}
		if bootErr != nil {
			m.mu.Unlock()
			return warmToolLease{}, bootErr
		}
		inst := m.instances[instanceID]
		if inst == nil {
			m.mu.Unlock()
			return warmToolLease{}, fmt.Errorf("warm tool VM %q exited before lease registration", instanceID)
		}
		inst.warmToolVM = true
		inst.inflight = 1
		inst.lastUsedAt = time.Now().UTC()
		inst.draining = false
		inst.reaping = false
		m.instancesByKey[key] = instanceID
		m.mu.Unlock()
		return warmToolLease{instanceID: instanceID, reused: false, startVMMs: startVMMs}, nil
	}
}

func waitWarmProvisioning(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitWarmInstanceDone(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitFirecrackerProcessGone(ctx context.Context, instanceID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err := firecrackerProcessRunningFunc(instanceID)
		if err == nil && !running {
			return nil
		}
		select {
		case <-waitCtx.Done():
			if err != nil {
				return fmt.Errorf("verify microVM process %s exited: %w", instanceID, err)
			}
			return fmt.Errorf("microVM process %s still running after instance done", instanceID)
		case <-ticker.C:
		}
	}
}

func (m *Manager) releaseWarmToolVM(instanceID string) {
	m.mu.Lock()
	inst := m.instances[instanceID]
	if inst != nil {
		if inst.inflight > 0 {
			inst.inflight--
		}
		inst.lastUsedAt = time.Now().UTC()
		m.promoteWarmTeardownLocked(inst)
	}
	m.mu.Unlock()
}

func (m *Manager) promoteWarmTeardownLocked(inst *instance) {
	if inst == nil || !inst.warmToolVM || !inst.draining || inst.reaping || inst.inflight > 0 {
		return
	}
	inst.reaping = true
	go m.teardownWarmToolVM(inst)
}

func (m *Manager) teardownWarmToolVM(inst *instance) {
	m.teardownWarmToolVMContext(context.Background(), inst)
}

func (m *Manager) teardownWarmToolVMContext(ctx context.Context, inst *instance) {
	m.teardownManagedVMContext(ctx, inst)
}

func (m *Manager) teardownManagedVMContext(ctx context.Context, inst *instance) {
	if inst == nil {
		return
	}
	freezeCtx, cancelFreeze := context.WithTimeout(ctx, 10*time.Second)
	freezeWorkspace := m.FreezeWorkspace
	if m.freezeWorkspace != nil {
		freezeWorkspace = m.freezeWorkspace
	}
	if _, err := freezeWorkspace(freezeCtx, inst.id); err != nil {
		slog.Warn("failed to freeze microVM workspace before teardown", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "warmToolVM", inst.warmToolVM, "error", err)
	}
	cancelFreeze()
	removeCtx, cancelRemove := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRemove()
	removeInstance := m.Remove
	if m.removeInstance != nil {
		removeInstance = m.removeInstance
	}
	if err := removeInstance(removeCtx, inst.id); err != nil {
		slog.Warn("failed to tear down microVM", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "warmToolVM", inst.warmToolVM, "error", err)
		var killErr error
		if m.launcher != nil {
			escalateCtx, cancelEscalate := context.WithTimeout(context.Background(), 10*time.Second)
			killErr = m.launcher.Stop(escalateCtx, inst.id)
			cancelEscalate()
		} else {
			killErr = signalFirecrackerByIDFunc(inst.id, syscall.SIGKILL)
		}
		if killErr != nil {
			slog.Error("failed to escalate microVM teardown", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "warmToolVM", inst.warmToolVM, "error", killErr)
		} else {
			slog.Warn("escalated microVM teardown", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "warmToolVM", inst.warmToolVM, "privilegedLauncher", m.launcher != nil)
		}
	}
}

func (m *Manager) ExecuteToolLeased(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, req ToolExecRequest) (string, ToolExecResponse, error) {
	started := time.Now()
	lease, err := m.acquireWarmToolVM(ctx, agentID, compartmentID, opts)
	if err != nil {
		m.logMicroVMToolTiming(agentID, compartmentID, "", "", req, true, false, started, 0, 0, 0, err, ToolExecResponse{})
		return "", ToolExecResponse{}, err
	}
	defer m.releaseWarmToolVM(lease.instanceID)
	execStarted := time.Now()
	exec := m.executeTool
	if exec == nil {
		exec = m.ExecuteTool
	}
	resp, execErr := exec(ctx, lease.instanceID, req)
	execMs := time.Since(execStarted).Milliseconds()
	m.logMicroVMToolTiming(agentID, compartmentID, lease.instanceID, lease.instanceID, req, true, lease.reused, started, lease.startVMMs, execMs, 0, execErr, resp)
	return lease.instanceID, resp, execErr
}

func (m *Manager) WithToolVMFile(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, fn func(instanceID string) error) error {
	if m.cfg == nil || !m.cfg.ToolVMReuseEffective() {
		return m.withEphemeralToolVMFile(ctx, agentID, compartmentID, opts, fn)
	}
	lease, err := m.acquireWarmToolVM(ctx, agentID, compartmentID, opts)
	if err != nil {
		return err
	}
	defer m.releaseWarmToolVM(lease.instanceID)
	return fn(lease.instanceID)
}

func (m *Manager) withEphemeralToolVMFile(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, fn func(instanceID string) error) error {
	agentID = strings.TrimSpace(agentID)
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return err
	}
	unlock := m.lockSerializedCompartmentMount(agentID, compartmentID)
	defer unlock()

	opts.CompartmentID = compartmentID
	opts.RuntimeClass = runtimemanager.RuntimeClassToolExecutor
	opts.Ephemeral = true
	instanceID, err := m.createAndStart(ctx, agentID, opts)
	if err != nil {
		return err
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Remove(removeCtx, instanceID); err != nil {
			slog.Warn("failed to tear down ephemeral file-transfer microVM", "agent", agentID, "compartment", compartmentID, "instance", instanceID, "error", err)
		}
	}()
	err = fn(instanceID)
	freezeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, freezeErr := m.FreezeWorkspace(freezeCtx, instanceID); freezeErr != nil {
		slog.Warn("failed to freeze ephemeral file-transfer workspace before teardown", "agent", agentID, "compartment", compartmentID, "instance", instanceID, "error", freezeErr)
		if err == nil {
			err = freezeErr
		}
	}
	return err
}

func (m *Manager) lockSerializedCompartmentMount(agentID, compartmentID string) func() {
	if !m.requiresSerializedCompartmentMount() {
		return func() {}
	}
	lock := m.compartmentMountLock(agentID, compartmentID)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) requiresSerializedCompartmentMount() bool {
	if m == nil || m.cfg == nil {
		return true
	}
	return !m.cfg.ToolVMReuseEffective()
}

func (m *Manager) compartmentMountLock(agentID, compartmentID string) *sync.Mutex {
	key := runtimeInstanceKey{agentID: strings.TrimSpace(agentID), compartmentID: normalizeRuntimeCompartmentID(compartmentID)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mountLocks == nil {
		m.mountLocks = map[runtimeInstanceKey]*sync.Mutex{}
	}
	lock := m.mountLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.mountLocks[key] = lock
	}
	return lock
}

func (m *Manager) logMicroVMToolTiming(agentID, compartmentID, leaseID, instanceID string, req ToolExecRequest, reusable, reused bool, started time.Time, startVMMs, execMs, freezeMs int64, err error, resp ToolExecResponse) {
	status := "completed"
	if err != nil {
		status = "failed"
	}
	attrs := []any{
		"agent", agentID,
		"compartment", compartmentID,
		"lease", leaseID,
		"instance", instanceID,
		"operation", req.Operation,
		"path", req.Path,
		"reusable", reusable,
		"reused", reused,
		"status", status,
		"totalMs", time.Since(started).Milliseconds(),
		"startVMMs", startVMMs,
		"execMs", execMs,
		"freezeMs", freezeMs,
		"exitCode", resp.ExitCode,
		"timedOut", resp.TimedOut,
	}
	if err != nil {
		attrs = append(attrs, "error", err)
		slog.Warn("microvm tool executor timing", attrs...)
	} else {
		slog.Info("microvm tool executor timing", attrs...)
	}
	if m == nil || m.flowRecorder == nil {
		return
	}
	flowStatus := flow.StatusCompleted
	if err != nil {
		flowStatus = flow.StatusFailed
	}
	event := flow.NewEvent(agentID, "tool-vm", string(req.Operation), flowStatus, started)
	event.CompartmentID = compartmentID
	event.DurationMs = time.Since(started).Milliseconds()
	if err != nil {
		event.Error = err.Error()
	}
	event.Attrs = flow.Attrs(
		"lease", leaseID,
		"instance", instanceID,
		"operation", req.Operation,
		"path", req.Path,
		"reusable", reusable,
		"reused", reused,
		"startVMMs", startVMMs,
		"execMs", execMs,
		"freezeMs", freezeMs,
		"exitCode", resp.ExitCode,
		"timedOut", resp.TimedOut,
	)
	m.flowRecorder.RecordFlowEvent(context.Background(), event)
}

func toolRequestWritesWorkspace(req ToolExecRequest) bool {
	switch req.Operation {
	case ToolOpExec, ToolOpWriteFile, ToolOpMkdir, ToolOpRm:
		return true
	default:
		return false
	}
}

// cleanupUntrackedAgentOrphans stops firecracker processes for the agent that the
// manager is not tracking in m.instances (leaked from a prior process whose
// runtime-instance row was GC'd before the process exited). Tracked instances —
// including adopted siblings — are left running.
