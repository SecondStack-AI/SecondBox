package microvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentcy/internal/config"
	"agentcy/internal/egressproxy"
	"agentcy/internal/flow"
	"agentcy/internal/registry"
	"agentcy/internal/runtimecontext"
	"agentcy/internal/runtimemanager"
)

const (
	instancePrefix      = "fc"
	workspaceName       = "workspace.ext4"
	rootfsName          = "rootfs.ext4"
	kernelName          = "vmlinux"
	sharedImageName     = "shared.img"
	firecrackerSockName = "firecracker.sock"
	vsockUDSName        = "agentcy.vsock"
	configName          = "firecracker.json"
)

// maxUnixSocketPathLen is the kernel's sockaddr_un.sun_path capacity (including the
// trailing NUL). Firecracker's API socket and the vsock UDS must fit within it.
// Jailed mode binds a short path relative to the jail chroot; unjailed mode has no
// chroot and binds the full host path, so a deep AG_DATA_DIR otherwise yields the
// cryptic "path must be shorter than SUN_LEN" at boot.
const maxUnixSocketPathLen = 108

// reservedRunDirBudget reserves room under maxUnixSocketPathLen for the per-instance
// suffix "/<instanceID>/firecracker.sock" the manager appends to the run dir.
const reservedRunDirBudget = 64

//go:embed firecracker.lock
var firecrackerVersionLock string

// Manager owns Firecracker-backed agent runtime instances.
type Manager struct {
	cfg              *config.Config
	mu               sync.Mutex
	instances        map[string]*instance
	instancesByKey   map[runtimeInstanceKey]string
	provisioning     map[runtimeInstanceKey]chan struct{}
	shuttingDown     bool
	sweepCancel      context.CancelFunc
	sweepDone        chan struct{}
	guestIPs         map[string]string // instanceID -> reserved guest IP
	sourceBindings   SourceBindingRegistrar
	network          HostNetworkConfigurer
	egressRouter     HostEgressRouter
	startCompartment func(context.Context, string, string, runtimemanager.StartOpts) (string, error)
	executeTool      func(context.Context, string, ToolExecRequest) (ToolExecResponse, error)
	freezeWorkspace  func(context.Context, string) (BackupResponse, error)
	removeInstance   func(context.Context, string) error
	flowRecorder     flow.Recorder
	startDurations   []time.Duration
	mountLocks       map[runtimeInstanceKey]*sync.Mutex
}

func (m *Manager) SetFlowRecorder(recorder flow.Recorder) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flowRecorder = recorder
}

type RuntimeMetricsSnapshot struct {
	ConcurrentVMsByAgent map[string]int
	GuestIPsInUse        int
	GuestIPCapacity      int
	ColdStartCount       int
	ColdStartP95         time.Duration
}

type runtimeInstanceKey struct {
	agentID       string
	compartmentID string
}

type instance struct {
	id                 string
	agentID            string
	compartmentID      string
	dir                string
	logPath            string
	socket             string
	vsockUDS           string
	tapName            string
	jailRoot           string
	rootfsPath         string
	rootfsImagePath    string
	workspacePath      string
	sharedImagePath    string
	guestIP            string
	startupFingerprint string
	cmd                *exec.Cmd
	startedAt          time.Time
	lastUsedAt         time.Time
	inflight           int
	warmToolVM         bool
	draining           bool
	reaping            bool
	done               chan struct{} // closed after the VM exits and cleanup runs
	doneOnce           sync.Once
	// launcherOnly is true for jailed launches where cmd is the short-lived
	// jailer parent. The real Firecracker process continues as an orphaned
	// child, identified by --id.
	launcherOnly bool
}

type firecrackerLaunch struct {
	executable string
	args       []string
	config     firecrackerConfig
	configPath string
	socketPath string
	vsockUDS   string
	jailRoot   string
}

type microVMImageSelection struct {
	RuntimeClass    runtimemanager.RuntimeClass
	RootfsPath      string
	SharedImagePath string
}

type warmToolLease struct {
	instanceID string
	reused     bool
	startVMMs  int64
}

var (
	toolExecutorFingerprintContractVersion = ToolExecutorContractVersion
	toolExecutorFingerprintCapabilities    = []string{"workspace-session-env"}
	hardLinkFile                           = os.Link
)

type coldStartStageTimer struct {
	start time.Time
	last  time.Time
	base  []any
}

func newColdStartStageTimer(base ...any) *coldStartStageTimer {
	now := time.Now()
	return &coldStartStageTimer{start: now, last: now, base: base}
}

func (t *coldStartStageTimer) mark(stage string, attrs ...any) {
	if t == nil {
		return
	}
	now := time.Now()
	args := make([]any, 0, len(t.base)+6+len(attrs))
	args = append(args, t.base...)
	args = append(args,
		"stage", stage,
		"stageMs", now.Sub(t.last).Milliseconds(),
		"elapsedMs", now.Sub(t.start).Milliseconds(),
	)
	args = append(args, attrs...)
	slog.Info("microVM cold start stage", args...)
	t.last = now
}

func (t *coldStartStageTimer) elapsedMs() int64 {
	if t == nil {
		return 0
	}
	return time.Since(t.start).Milliseconds()
}

// New validates host prerequisites and constructs a microVM manager.
func New(cfg *config.Config) (*Manager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if err := requireExecutable("firecracker", cfg.FirecrackerPath); err != nil {
		return nil, err
	}
	if err := ensureFirecrackerVersion(cfg.FirecrackerPath); err != nil {
		return nil, err
	}
	if !cfg.MicroVMAllowUnjailed {
		if err := requireExecutable("jailer", cfg.JailerPath); err != nil {
			return nil, err
		}
	}
	for label, path := range map[string]string{
		"kernel": cfg.MicroVMKernelPath,
		"rootfs": cfg.MicroVMRootfsPath,
	} {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("microVM %s path is required", label)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("microVM %s path %q: %w", label, path, err)
		}
	}
	if cfg.MicroVMSharedImagePath != "" {
		if _, err := os.Stat(cfg.MicroVMSharedImagePath); err != nil {
			return nil, fmt.Errorf("microVM shared image path %q: %w", cfg.MicroVMSharedImagePath, err)
		}
	}
	if cfg.MicroVMToolRootfsPath != "" {
		if _, err := os.Stat(cfg.MicroVMToolRootfsPath); err != nil {
			return nil, fmt.Errorf("microVM tool rootfs path %q: %w", cfg.MicroVMToolRootfsPath, err)
		}
	}
	if cfg.MicroVMToolSharedImagePath != "" {
		if _, err := os.Stat(cfg.MicroVMToolSharedImagePath); err != nil {
			return nil, fmt.Errorf("microVM tool shared image path %q: %w", cfg.MicroVMToolSharedImagePath, err)
		}
	}
	if cfg.MicroVMAllowUnjailed {
		if relocated, ok := relocateRunDirForUnixSockets(cfg.MicroVMRunDir); ok {
			slog.Warn("microVM run dir is too long for unjailed unix sockets; relocating to keep API/vsock paths under the kernel limit",
				"from", cfg.MicroVMRunDir, "to", relocated, "limit", maxUnixSocketPathLen)
			cfg.MicroVMRunDir = relocated
		}
	}
	for _, dir := range []string{cfg.MicroVMWorkspaceDir, cfg.MicroVMRunDir, cfg.MicroVMLogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create microVM dir %q: %w", dir, err)
		}
	}
	return &Manager{
		cfg:            cfg,
		instances:      map[string]*instance{},
		instancesByKey: map[runtimeInstanceKey]string{},
		provisioning:   map[runtimeInstanceKey]chan struct{}{},
		guestIPs:       map[string]string{},
		network:        IPTapConfigurer{},
	}, nil
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := m.sweepStartupOrphans(ctx); err != nil {
		slog.Warn("startup orphan microVM sweep completed with unreclaimed entries", "error", err)
	}
	if m.cfg == nil || !m.cfg.ToolVMReuseEffective() {
		return nil
	}
	m.mu.Lock()
	if m.sweepCancel != nil {
		m.mu.Unlock()
		return nil
	}
	ttl := m.cfg.MicroVMToolVMIdleTTL
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	interval := ttl
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.sweepCancel = cancel
	m.sweepDone = done
	m.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case now := <-ticker.C:
				m.sweepIdleToolVMs(now)
			}
		}
	}()
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.shuttingDown = true
	cancel := m.sweepCancel
	done := m.sweepDone
	m.sweepCancel = nil
	m.sweepDone = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 3*time.Second)
	defer cancelWait()
	provisioningTimedOut := false
	for {
		m.mu.Lock()
		provisioning := len(m.provisioning)
		m.mu.Unlock()
		if provisioning == 0 {
			break
		}
		select {
		case <-waitCtx.Done():
			slog.Warn("microVM manager shutdown timed out waiting for provisioning warm tool VMs", "error", waitCtx.Err())
			provisioningTimedOut = true
			break
		case <-time.After(25 * time.Millisecond):
		}
		if provisioningTimedOut {
			break
		}
	}
	if !provisioningTimedOut {
		inflightCtx, cancelInflight := context.WithTimeout(ctx, 30*time.Second)
		defer cancelInflight()
		for {
			m.mu.Lock()
			inflight := m.warmToolInflightLocked()
			m.mu.Unlock()
			if inflight == 0 {
				break
			}
			select {
			case <-inflightCtx.Done():
				slog.Warn("microVM manager shutdown timed out waiting for in-flight warm tool VM work", "inflight", inflight, "error", inflightCtx.Err())
				inflight = 0
			case <-time.After(25 * time.Millisecond):
			}
			if inflight == 0 {
				break
			}
		}
	}

	var victims []*instance
	m.mu.Lock()
	for _, inst := range m.instances {
		if inst == nil || !inst.warmToolVM || inst.reaping {
			continue
		}
		inst.reaping = true
		victims = append(victims, inst)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	doneTeardown := make(chan struct{})
	for _, inst := range victims {
		wg.Add(1)
		go func(inst *instance) {
			defer wg.Done()
			m.teardownWarmToolVMContext(ctx, inst)
		}(inst)
	}
	go func() {
		wg.Wait()
		close(doneTeardown)
	}()
	select {
	case <-doneTeardown:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
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

func (m *Manager) SetSourceBindingRegistrar(registrar SourceBindingRegistrar) {
	m.sourceBindings = registrar
}

func (m *Manager) sweepStartupOrphans(ctx context.Context) error {
	if m == nil || m.cfg == nil || strings.TrimSpace(m.cfg.MicroVMRunDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(m.cfg.MicroVMRunDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read microVM run dir: %w", err)
	}
	var joined error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		instanceID := strings.TrimSpace(entry.Name())
		if !strings.HasPrefix(instanceID, instancePrefix+"-") || m.lookup(instanceID) != nil {
			continue
		}
		runDir := filepath.Join(m.cfg.MicroVMRunDir, instanceID)
		jailRoot := m.jailerRoot(instanceID)
		running, err := firecrackerProcessRunningFunc(instanceID)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("inspect startup orphan %s: %w", instanceID, err))
			continue
		}
		slog.Warn("reclaiming startup orphaned microVM", "instance", instanceID, "running", running, "runDir", runDir)
		if running {
			inst := &instance{
				id:           instanceID,
				dir:          runDir,
				jailRoot:     jailRoot,
				tapName:      tapNameForInstance(m.cfg.MicroVMTapPrefix, instanceID),
				launcherOnly: true,
				done:         make(chan struct{}),
			}
			orphanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := m.stopInstance(orphanCtx, inst, true)
			cancel()
			if err != nil {
				joined = errors.Join(joined, fmt.Errorf("stop startup orphan %s: %w", instanceID, err))
				continue
			}
		}
		if err := os.RemoveAll(runDir); err != nil {
			joined = errors.Join(joined, fmt.Errorf("remove startup orphan run dir %q: %w", runDir, err))
		}
		if jailRoot != "" {
			if err := os.RemoveAll(filepath.Dir(jailRoot)); err != nil {
				joined = errors.Join(joined, fmt.Errorf("remove startup orphan jail root %q: %w", jailRoot, err))
			}
		}
	}
	return joined
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

func (m *Manager) SetHostNetworkConfigurer(configurer HostNetworkConfigurer) {
	m.network = configurer
}

func (m *Manager) SetHostEgressRouter(router HostEgressRouter) {
	m.egressRouter = router
}

// ensureFirecrackerVersion pins the VMM snapshot ABI and keeps the
// CVE-2026-5747 floor explicit. Golden snapshots are not portable across
// Firecracker major/minor changes unless rebuilt.
func ensureFirecrackerVersion(fcPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, fcPath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run firecracker --version: %w", err)
	}
	maj, minr, pat, ok := parseFirecrackerVersion(string(out))
	if !ok {
		return fmt.Errorf("parse firecracker --version output %q", strings.TrimSpace(string(out)))
	}
	if !firecrackerVersionSupported(maj, minr, pat) {
		return fmt.Errorf("firecracker %d.%d.%d is below the required minimum (>=1.14.4 / >=1.15.1, CVE-2026-5747); upgrade the firecracker binary", maj, minr, pat)
	}
	wantMaj, wantMin, wantPatch, ok := expectedFirecrackerVersion()
	if !ok {
		return fmt.Errorf("parse embedded firecracker.lock")
	}
	if maj != wantMaj || minr != wantMin || pat != wantPatch {
		return fmt.Errorf("firecracker %d.%d.%d does not match pinned version %d.%d.%d; rebuild snapshots/artifacts with the pinned VMM or update internal/microvm/firecracker.lock deliberately", maj, minr, pat, wantMaj, wantMin, wantPatch)
	}
	return nil
}

// firecrackerVersionSupported applies the CVE-2026-5747 floor: >=1.14.4 on the
// 1.14 branch, >=1.15.1 on the 1.15 branch, and anything newer.
func firecrackerVersionSupported(major, minor, patch int) bool {
	if major != 1 {
		return major > 1
	}
	switch {
	case minor > 15:
		return true
	case minor == 15:
		return patch >= 1
	case minor == 14:
		return patch >= 4
	default:
		return false
	}
}

func parseFirecrackerVersion(out string) (int, int, int, bool) {
	for _, tok := range strings.Fields(out) {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "v")
		parts := strings.SplitN(tok, ".", 3)
		if len(parts) < 3 {
			continue
		}
		maj, err1 := strconv.Atoi(leadingDigits(parts[0]))
		minr, err2 := strconv.Atoi(leadingDigits(parts[1]))
		pat, err3 := strconv.Atoi(leadingDigits(parts[2]))
		if err1 == nil && err2 == nil && err3 == nil {
			return maj, minr, pat, true
		}
	}
	return 0, 0, 0, false
}

func expectedFirecrackerVersion() (int, int, int, bool) {
	for _, line := range strings.Split(firecrackerVersionLock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "FIRECRACKER_VERSION" {
			continue
		}
		return parseFirecrackerVersion(strings.TrimSpace(value))
	}
	return 0, 0, 0, false
}

func expectedFirecrackerVersionString() string {
	maj, minr, pat, ok := expectedFirecrackerVersion()
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", maj, minr, pat)
}

func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}

func requireExecutable(label, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s path is required", label)
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return fmt.Errorf("%s binary %q not found: %w", label, path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("%s binary %q: %w", label, resolved, err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s binary %q is not executable", label, resolved)
	}
	return nil
}

// createAndStart starts a Firecracker process for a microVM package workflow.
func (m *Manager) createAndStart(ctx context.Context, agentID string, opts runtimemanager.StartOpts) (string, error) {
	agentID = strings.TrimSpace(agentID)
	compartmentID := normalizeRuntimeCompartmentID(opts.CompartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	opts.CompartmentID = compartmentID
	key := runtimeInstanceKey{agentID: agentID, compartmentID: compartmentID}
	// Single-guest-IP mode admits only one compartment per agent, so a Firecracker
	// process for this agent that the manager is not tracking is a leaked orphan
	// that still holds the shared guest IP. Reclaim it before this cold boot
	// reserves the same IP, which would otherwise put two VMs on one address. With
	// a bridge CIDR each VM gets a distinct IP and legitimate sibling/in-flight
	// spawns must not be touched.
	if m.cfg != nil && strings.TrimSpace(m.cfg.MicroVMBridgeCIDR) == "" {
		m.cleanupUntrackedAgentOrphans(ctx, agentID)
	}
	m.mu.Lock()
	if err := m.admitCompartmentSpawnLocked(key); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	return m.startCompartmentInstance(ctx, agentID, compartmentID, opts)
}

// ExecuteEphemeralTool starts a fresh Firecracker tool VM for one dangerous
// operation, executes the request, then tears the VM down. Every sandbox hand
// gets its own ephemeral VM; there is no warm-VM reuse.
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
		if err := m.admitCompartmentSpawnLocked(key); err != nil {
			m.mu.Unlock()
			return warmToolLease{}, err
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
	if inst == nil {
		return
	}
	freezeCtx, cancelFreeze := context.WithTimeout(ctx, 10*time.Second)
	freezeWorkspace := m.FreezeWorkspace
	if m.freezeWorkspace != nil {
		freezeWorkspace = m.freezeWorkspace
	}
	if _, err := freezeWorkspace(freezeCtx, inst.id); err != nil {
		slog.Warn("failed to freeze warm tool microVM before teardown", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "error", err)
	}
	cancelFreeze()
	removeCtx, cancelRemove := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRemove()
	removeInstance := m.Remove
	if m.removeInstance != nil {
		removeInstance = m.removeInstance
	}
	if err := removeInstance(removeCtx, inst.id); err != nil {
		slog.Warn("failed to tear down warm tool microVM", "agent", inst.agentID, "compartment", inst.compartmentID, "instance", inst.id, "error", err)
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
func (m *Manager) cleanupUntrackedAgentOrphans(ctx context.Context, agentID string) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return
	}
	orphanIDs, err := firecrackerInstanceIDsForAgentFunc(agentID)
	if err != nil {
		slog.Warn("failed to enumerate firecracker processes for orphan cleanup", "agent", agentID, "error", err)
		return
	}
	if len(orphanIDs) == 0 {
		return
	}
	for _, id := range m.untrackedInstanceIDs(orphanIDs) {
		slog.Warn("reclaiming orphaned firecracker process not tracked by any runtime instance", "agent", agentID, "instance", id)
		inst := &instance{
			id:           id,
			agentID:      agentID,
			dir:          filepath.Join(m.cfg.MicroVMRunDir, id),
			jailRoot:     m.jailerRoot(id),
			tapName:      tapNameForInstance(m.cfg.MicroVMTapPrefix, id),
			launcherOnly: true,
			done:         make(chan struct{}),
		}
		if err := m.stopInstance(ctx, inst, true); err != nil {
			slog.Warn("failed to stop orphaned firecracker process", "agent", agentID, "instance", id, "error", err)
		}
	}
}

// untrackedInstanceIDs returns the subset of candidateIDs the manager is not
// currently tracking in m.instances. Tracked instances — including adopted
// siblings and in-flight spawns — are excluded so only true orphans are returned.
func (m *Manager) untrackedInstanceIDs(candidateIDs []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range candidateIDs {
		if _, ok := m.instances[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (m *Manager) startCompartmentInstance(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (id string, err error) {
	started := time.Now()
	defer func() {
		if err == nil && strings.TrimSpace(id) != "" {
			m.recordStartDuration(time.Since(started))
		}
	}()
	if m.startCompartment != nil {
		return m.startCompartment(ctx, agentID, compartmentID, opts)
	}
	return m.createAndStartCold(ctx, agentID, compartmentID, opts)
}

func (m *Manager) createAndStartCold(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	opts.CompartmentID = compartmentID
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelSetup()

	id, err := newInstanceID(agentID, compartmentID)
	if err != nil {
		return "", err
	}
	timer := newColdStartStageTimer("agent", agentID, "compartment", compartmentID, "instance", id)
	timer.mark("instance_id_allocated")
	guestIP, err := m.reserveGuestIP(id)
	if err != nil {
		return "", fmt.Errorf("reserve guest IP: %w", err)
	}
	timer.mark("guest_ip_reserved", "guestIP", guestIP)
	releaseIP := true
	defer func() {
		if releaseIP {
			m.releaseGuestIP(id)
		}
	}()
	tapName := ""
	if m.networkRequired(opts) {
		tapName = tapNameForInstance(m.cfg.MicroVMTapPrefix, id)
		if m.network == nil {
			return "", fmt.Errorf("microVM tap networking is required but no host network configurer is available")
		}
		if err := m.network.ConfigureTap(setupCtx, TapConfig{
			AgentID:    agentID,
			InstanceID: id,
			TapName:    tapName,
			BridgeName: m.cfg.MicroVMBridgeName,
			BridgeCIDR: m.cfg.MicroVMBridgeCIDR,
			OwnerUID:   m.tapOwnerUID(),
		}); err != nil {
			return "", fmt.Errorf("configure microVM tap: %w", err)
		}
	}
	timer.mark("network_ready", "tap", tapName, "networkRequired", m.networkRequired(opts))
	dir := filepath.Join(m.cfg.MicroVMRunDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.cleanupTap(ctx, tapName)
		return "", fmt.Errorf("create instance dir: %w", err)
	}
	// The per-instance dir holds disposable launch state (the rootfs copy, the
	// firecracker config, the identity file). Reclaim it on any early failure;
	// ownership transfers to the running instance once it is registered below.
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.RemoveAll(dir)
		}
	}()
	logPath := filepath.Join(m.cfg.MicroVMLogDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.cleanupTap(ctx, tapName)
		return "", fmt.Errorf("open microVM log: %w", err)
	}
	defer logFile.Close()
	timer.mark("instance_files_ready", "dir", dir, "log", logPath)

	image, err := m.microVMImageForStart(opts)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	rootfsPath := filepath.Join(dir, rootfsName)
	if err := reflinkOnlyFile(rootfsPath, image.RootfsPath, 0o600); err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("prepare rootfs: %w", err)
	}
	timer.mark("rootfs_reflinked")
	workspacePath, err := m.prepareWorkspace(setupCtx, agentID, compartmentID)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("prepare workspace: %w", err)
	}
	timer.mark("workspace_ready", "workspace", workspacePath)
	// workspacePath is the compartment's persistent VM-local disk, seeded once
	// from the agent workspace. It is deliberately never removed on the error
	// paths below so a restart does not destroy saved compartment-local work.

	launch, err := m.prepareLaunch(setupCtx, id, dir, rootfsPath, workspacePath, image.SharedImagePath, tapName, guestIP)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	timer.mark("launch_prepared", "jailRoot", launch.jailRoot)
	data, err := json.MarshalIndent(launch.config, "", "  ")
	if err != nil {
		m.cleanupLaunch(launch)
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("marshal firecracker config: %w", err)
	}
	if err := os.WriteFile(launch.configPath, data, 0o600); err != nil {
		m.cleanupLaunch(launch)
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("write firecracker config: %w", err)
	}
	if launch.jailRoot != "" {
		if err := chownIfDifferent(launch.configPath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
			m.cleanupLaunch(launch)
			m.cleanupTap(setupCtx, tapName)
			return "", fmt.Errorf("chown jailed firecracker config: %w", err)
		}
	}
	if err := m.writeIdentityFile(dir, id, agentID, opts); err != nil {
		m.cleanupLaunch(launch)
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	startupFingerprint, err := m.startupFingerprint(agentID, compartmentID, opts)
	if err != nil {
		m.cleanupLaunch(launch)
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("build startup fingerprint: %w", err)
	}
	timer.mark("launch_config_ready")

	cmd := exec.Command(launch.executable, launch.args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.cleanupLaunch(launch)
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("start firecracker: %w", err)
	}
	timer.mark("firecracker_process_started", "pid", cmd.Process.Pid)

	inst := &instance{
		id:                 id,
		agentID:            agentID,
		compartmentID:      compartmentID,
		dir:                dir,
		logPath:            logPath,
		socket:             launch.socketPath,
		vsockUDS:           launch.vsockUDS,
		tapName:            tapName,
		jailRoot:           launch.jailRoot,
		rootfsPath:         rootfsPath,
		rootfsImagePath:    image.RootfsPath,
		workspacePath:      workspacePath,
		sharedImagePath:    image.SharedImagePath,
		guestIP:            guestIP,
		startupFingerprint: startupFingerprint,
		cmd:                cmd,
		startedAt:          time.Now().UTC(),
		done:               make(chan struct{}),
		launcherOnly:       launch.jailRoot != "",
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.mu.Unlock()
	// Start the reaper before any stopInstance call so the process is always
	// waited on (no zombie) and cleanup runs exactly once.
	go m.reap(inst)
	timer.mark("instance_registered")
	contextToken, err := m.registerSourceBinding(setupCtx, agentID, id, opts)
	if err != nil {
		_ = m.stopInstance(setupCtx, inst, true)
		return "", err
	}
	if opts.ProxyEgress != nil {
		opts.ProxyEgress.ContextToken = contextToken
	}
	timer.mark("source_binding_registered")
	if err := m.deliverStartupSecrets(setupCtx, inst, agentID, opts, timer); err != nil {
		_ = m.stopInstance(setupCtx, inst, true)
		return "", fmt.Errorf("deliver runtime startup secrets: %w", err)
	}
	timer.mark("microvm_ready")
	releaseIP = false // ownership transfers to the running instance
	cleanupDir = false
	slog.Info("started firecracker microVM", "agent", agentID, "compartment", compartmentID, "instance", id, "elapsedMs", timer.elapsedMs(), "log", logPath)
	return id, nil
}

// relocateRunDirForUnixSockets returns a shorter run dir (and true) when runDir is
// deep enough that appending the per-instance socket suffix would exceed the unix
// socket path limit. The run dir also holds large rootfs copies, so avoid
// XDG_RUNTIME_DIR: it is commonly a small tmpfs and cannot hold an 8G VM image.
// Returns ("", false) when runDir already fits.
func relocateRunDirForUnixSockets(runDir string) (string, bool) {
	abs := runDir
	if a, err := filepath.Abs(runDir); err == nil {
		abs = a
	}
	if len(abs)+reservedRunDirBudget < maxUnixSocketPathLen {
		return "", false
	}
	for _, candidate := range shortMicroVMRunDirCandidates() {
		if len(candidate)+reservedRunDirBudget < maxUnixSocketPathLen {
			return candidate, true
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agentcy-%d", os.Getuid()), "run"), true
}

func shortMicroVMRunDirCandidates() []string {
	var candidates []string
	if cacheDir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cacheDir) != "" {
		candidates = append(candidates, filepath.Join(cacheDir, "agentcy", "microvm", "run"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), fmt.Sprintf("agentcy-%d", os.Getuid()), "run"))
	return candidates
}

func (m *Manager) microVMImageForStart(opts runtimemanager.StartOpts) (microVMImageSelection, error) {
	runtimeClass := opts.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	switch runtimeClass {
	case runtimemanager.RuntimeClassToolExecutor:
		rootfsPath := firstNonEmpty(m.cfg.MicroVMToolRootfsPath, m.cfg.MicroVMRootfsPath)
		sharedImagePath := firstNonEmpty(m.cfg.MicroVMToolSharedImagePath, m.cfg.MicroVMSharedImagePath)
		return microVMImageSelection{
			RuntimeClass:    runtimeClass,
			RootfsPath:      rootfsPath,
			SharedImagePath: sharedImagePath,
		}, nil
	default:
		return microVMImageSelection{}, fmt.Errorf("unsupported microVM runtime class %q", runtimeClass)
	}
}

// checkUnixSocketPath fails fast with an actionable message when a socket path would
// exceed the kernel limit, instead of letting Firecracker exit with a cryptic
// "path must be shorter than SUN_LEN".
func checkUnixSocketPath(label, path string) error {
	if len(path) >= maxUnixSocketPathLen {
		return fmt.Errorf("%s socket path %q is %d bytes, exceeding the unix socket limit of %d; set AG_MICROVM_RUN_DIR to a shorter path", label, path, len(path), maxUnixSocketPathLen)
	}
	return nil
}

func (m *Manager) prepareLaunch(ctx context.Context, instanceID, dir, rootfsPath, workspacePath, sharedImagePath, tapName, guestIP string) (firecrackerLaunch, error) {
	if m.cfg.MicroVMAllowUnjailed {
		socket := filepath.Join(dir, firecrackerSockName)
		vsockUDS := filepath.Join(dir, vsockUDSName)
		if err := checkUnixSocketPath("firecracker api", socket); err != nil {
			return firecrackerLaunch{}, err
		}
		if err := checkUnixSocketPath("vsock", vsockUDS); err != nil {
			return firecrackerLaunch{}, err
		}
		configPath := filepath.Join(dir, configName)
		return firecrackerLaunch{
			executable: m.cfg.FirecrackerPath,
			args:       []string{"--id", instanceID, "--api-sock", socket, "--config-file", configPath},
			config:     buildFirecrackerConfig(m.cfg, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP),
			configPath: configPath,
			socketPath: socket,
			vsockUDS:   vsockUDS,
		}, nil
	}

	jailRoot := m.jailerRoot(instanceID)
	if err := os.MkdirAll(jailRoot, 0o700); err != nil {
		return firecrackerLaunch{}, fmt.Errorf("create jail root: %w", err)
	}
	stagedRootfs := filepath.Join(jailRoot, rootfsName)
	stagedWorkspace := filepath.Join(jailRoot, workspaceName)
	if err := stageLinkedJailFile(stagedRootfs, rootfsPath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage rootfs in jail: %w", err)
	}
	if err := stageWorkspaceJailFile(stagedWorkspace, workspacePath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage workspace in jail: %w", err)
	}
	if err := stageCopiedJailFile(filepath.Join(jailRoot, kernelName), m.cfg.MicroVMKernelPath, 0o600, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage kernel in jail: %w", err)
	}
	drivesSharedPath := ""
	if strings.TrimSpace(sharedImagePath) != "" {
		drivesSharedPath = sharedImageName
		if err := stageCopiedJailFile(filepath.Join(jailRoot, sharedImageName), sharedImagePath, 0o600, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
			_ = os.RemoveAll(jailRoot)
			return firecrackerLaunch{}, fmt.Errorf("stage shared image in jail: %w", err)
		}
	}

	socket := filepath.Join(jailRoot, firecrackerSockName)
	vsockUDS := filepath.Join(jailRoot, vsockUDSName)
	configPath := filepath.Join(jailRoot, configName)
	fcConfig := buildFirecrackerConfig(m.cfg, rootfsName, workspaceName, drivesSharedPath, vsockUDSName, tapName, guestIP)
	fcConfig.BootSource.KernelImagePath = kernelName

	args := m.jailerArgs(instanceID)
	args = append(args, "--", "--api-sock", firecrackerSockName, "--config-file", configName)
	return firecrackerLaunch{
		executable: m.cfg.JailerPath,
		args:       args,
		config:     fcConfig,
		configPath: configPath,
		socketPath: socket,
		vsockUDS:   vsockUDS,
		jailRoot:   jailRoot,
	}, nil
}

func (m *Manager) jailerArgs(instanceID string) []string {
	args := []string{
		"--id", instanceID,
		"--exec-file", m.cfg.FirecrackerPath,
		"--uid", strconv.Itoa(m.cfg.MicroVMJailerUID),
		"--gid", strconv.Itoa(m.cfg.MicroVMJailerGID),
		"--chroot-base-dir", m.cfg.MicroVMJailerChrootBaseDir,
		"--new-pid-ns",
		"--resource-limit", "no-file=4096",
	}
	if m.cfg.MicroVMJailerCgroupVersion > 0 {
		args = append(args, "--cgroup-version", strconv.Itoa(m.cfg.MicroVMJailerCgroupVersion))
		if parent := strings.TrimSpace(m.cfg.MicroVMJailerParentCgroup); parent != "" {
			args = append(args, "--parent-cgroup", parent)
		}
		args = append(args, "--cgroup", jailerMemoryCgroup(m.cfg.MicroVMJailerCgroupVersion, m.cfg.MicroVMMemoryMiB))
	}
	return args
}

func (m *Manager) jailerRoot(instanceID string) string {
	execName := filepath.Base(m.cfg.FirecrackerPath)
	if execName == "." || execName == string(filepath.Separator) || execName == "" {
		execName = "firecracker"
	}
	return filepath.Join(m.cfg.MicroVMJailerChrootBaseDir, execName, instanceID, "root")
}

func (m *Manager) cleanupLaunch(launch firecrackerLaunch) {
	if launch.jailRoot != "" {
		_ = os.RemoveAll(launch.jailRoot)
	}
}

func jailerMemoryCgroup(version, memMiB int) string {
	overheadMiB := memMiB / 10
	if overheadMiB < 256 {
		overheadMiB = 256
	}
	bytes := int64(memMiB+overheadMiB) * 1024 * 1024
	if version == 1 {
		return fmt.Sprintf("memory.limit_in_bytes=%d", bytes)
	}
	return fmt.Sprintf("memory.max=%d", bytes)
}

func (m *Manager) networkRequired(opts runtimemanager.StartOpts) bool {
	if strings.TrimSpace(m.cfg.MicroVMBridgeName) != "" {
		return true
	}
	if strings.TrimSpace(m.cfg.MicroVMGuestIP) != "" {
		return true
	}
	return opts.ProxyEgress != nil && opts.ProxyEgress.Enabled
}

func (m *Manager) tapOwnerUID() int {
	if m.cfg == nil || m.cfg.MicroVMAllowUnjailed {
		return os.Getuid()
	}
	return m.cfg.MicroVMJailerUID
}

func tapNameForInstance(prefix, instanceID string) string {
	prefix = sanitizeTapPrefix(prefix)
	if prefix == "" {
		prefix = "agfc"
	}
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("%s%x", prefix, sum[:])[:min(15, len(prefix)+10)]
}

func sanitizeTapPrefix(prefix string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(prefix) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 5 {
		return out[:5]
	}
	return out
}

func guestMACForInstance(instanceID string) string {
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// reserveGuestIP allocates a per-VM guest IP. When a bridge CIDR is configured it
// picks the next free host address in that subnet (skipping the gateway), giving
// each microVM a distinct source identity for the egress proxy and a distinct
// target for the service proxies. Without a bridge CIDR it falls back to
// the single configured guest IP (only correct with one concurrent VM).
func (m *Manager) reserveGuestIP(instanceID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guestIPs == nil {
		m.guestIPs = map[string]string{}
	}
	cidr := strings.TrimSpace(m.cfg.MicroVMBridgeCIDR)
	if cidr == "" {
		ip := strings.TrimSpace(m.cfg.MicroVMGuestIP)
		m.guestIPs[instanceID] = ip
		return ip, nil
	}
	gwIP, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse bridge CIDR %q: %w", cidr, err)
	}
	gw4, base, mask := gwIP.To4(), ipnet.IP.To4(), ipnet.Mask
	if gw4 == nil || base == nil || len(mask) != 4 {
		return "", fmt.Errorf("microVM bridge CIDR %q must be IPv4", cidr)
	}
	used := make(map[string]bool, len(m.guestIPs))
	for _, ip := range m.guestIPs {
		used[ip] = true
	}
	maskU := binary.BigEndian.Uint32(mask)
	network := binary.BigEndian.Uint32(base) & maskU
	broadcast := network | ^maskU
	gwU := binary.BigEndian.Uint32(gw4)
	for host := network + 1; host < broadcast; host++ {
		if host == gwU {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], host)
		s := net.IP(b[:]).String()
		if used[s] {
			continue
		}
		m.guestIPs[instanceID] = s
		return s, nil
	}
	return "", fmt.Errorf("no free guest IP available in %s", cidr)
}

func (m *Manager) releaseGuestIP(instanceID string) {
	m.mu.Lock()
	delete(m.guestIPs, instanceID)
	m.mu.Unlock()
}

func (m *Manager) guestIP(instanceID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.guestIPs[instanceID]
}

// guestIPBootArg renders the kernel ip= autoconfiguration argument for a per-VM
// guest IP. It is only emitted in bridge mode, where the gateway and netmask are
// known; single-IP mode relies on operator-provided AG_MICROVM_KERNEL_ARGS.
func guestIPBootArg(cfg *config.Config, guestIP string) string {
	guestIP = strings.TrimSpace(guestIP)
	cidr := strings.TrimSpace(cfg.MicroVMBridgeCIDR)
	if guestIP == "" || cidr == "" {
		return ""
	}
	gwIP, ipnet, err := net.ParseCIDR(cidr)
	if err != nil || ipnet.IP.To4() == nil {
		return ""
	}
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, gwIP.String(), net.IP(ipnet.Mask).String())
}

func newInstanceID(agentID, compartmentID string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	return instancePrefix + "-" + agentID + "-" + compartmentIDSegment(compartmentID) + "-" + hex.EncodeToString(b[:]), nil
}

func compartmentIDSegment(compartmentID string) string {
	compartmentID = strings.TrimSpace(compartmentID)
	var b strings.Builder
	for _, r := range compartmentID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
		if b.Len() >= 16 {
			break
		}
	}
	segment := strings.Trim(b.String(), "-")
	if segment == "" {
		segment = "compartment"
	}
	if len(segment) <= 16 {
		return segment
	}
	return segment[:16]
}

func copyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := reflinkFile(out, in); err == nil {
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chmod(dst, mode)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// reflinkOnlyFile clones src into dst as a copy-on-write reflink and never falls
// back to a full byte copy. The tool rootfs image is multiple gigabytes, so a
// silent io.Copy per ephemeral VM adds tens of seconds of latency to every
// workspace tool call. When the clone cannot be made — dst on a different
// filesystem than src, or a filesystem without reflink support — it returns an
// actionable error instead of copying, so the misconfiguration surfaces loudly
// rather than as a mysterious stall.
func reflinkOnlyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := reflinkFile(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		hint := "the run dir must be on the same copy-on-write filesystem (btrfs/xfs) as the rootfs image"
		if errors.Is(err, syscall.EXDEV) {
			hint = "dst and src are on different filesystems; set AG_MICROVM_RUN_DIR to a path on the same filesystem as AG_MICROVM_ROOTFS_PATH"
		}
		return fmt.Errorf("reflink rootfs %s -> %s: %w (%s)", src, dst, err, hint)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func reflinkFile(dst, src *os.File) error {
	const ficlone = 0x40049409
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}

func stageCopiedJailFile(dst, src string, mode os.FileMode, uid, gid int) error {
	if err := copyFile(dst, src, mode); err != nil {
		return err
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		return err
	}
	return nil
}

func stageLinkedJailFile(dst, src string, uid, gid int) error {
	_ = os.Remove(dst)
	if err := hardLinkFile(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(dst, src, 0o600); err != nil {
			return err
		}
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func stageWorkspaceJailFile(dst, src string, uid, gid int) error {
	_ = os.Remove(dst)
	if err := hardLinkFile(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("link workspace image into jail: %w (jailer chroot base dir must be on the same filesystem as AG_MICROVM_WORKSPACE_DIR)", err)
		}
		return err
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func chownIfDifferent(path string, uid, gid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}

func ensureWorkspaceImage(ctx context.Context, path string, sizeMiB int, seedDir string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return createWorkspaceImage(ctx, path, sizeMiB, seedDir)
}

func createWorkspaceImage(ctx context.Context, path string, sizeMiB int, seedDir string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sizeBytes := int64(sizeMiB) * 1024 * 1024
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	args := []string{
		"-F",
		"-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard",
	}
	if seedDir != "" {
		if info, err := os.Stat(seedDir); err == nil && info.IsDir() {
			args = append(args, "-d", seedDir)
		} else if err != nil && !os.IsNotExist(err) {
			_ = os.Remove(path)
			return fmt.Errorf("stat workspace seed dir: %w", err)
		}
	}
	args = append(args, path)
	cmd := exec.CommandContext(ctx, "mkfs.ext4", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func debugfsReadFile(ctx context.Context, imagePath, guestPath string) ([]byte, bool, error) {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, imagePath).CombinedOutput()
	text := string(out)
	if strings.Contains(text, "File not found") || strings.Contains(text, "File not found by ext2_lookup") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("debugfs cat %s: %w: %s", guestPath, err, strings.TrimSpace(text))
	}
	lines := strings.SplitN(text, "\n", 2)
	if len(lines) == 2 && strings.HasPrefix(lines[0], "debugfs ") {
		text = lines[1]
	}
	return []byte(text), true, nil
}

func (m *Manager) prepareWorkspace(ctx context.Context, agentID, compartmentID string) (string, error) {
	if strings.EqualFold(m.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return m.ensureThinWorkspace(ctx, agentID, compartmentID)
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	workspacePath := filepath.Join(m.cfg.MicroVMWorkspaceDir, agentID, compartmentID+"."+workspaceName)
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0o700); err != nil {
		return "", fmt.Errorf("create compartment workspace dir: %w", err)
	}
	seedDir := m.workspaceSeedDir(agentID, compartmentID)
	if err := ensureWorkspaceImage(ctx, workspacePath, m.cfg.MicroVMWorkspaceSizeMiB, seedDir); err != nil {
		return "", err
	}
	return workspacePath, nil
}

type firecrackerConfig struct {
	BootSource    bootSource     `json:"boot-source"`
	Drives        []drive        `json:"drives"`
	Machine       machineConfig  `json:"machine-config"`
	Vsock         vsockConfig    `json:"vsock"`
	Logger        loggerConfig   `json:"logger,omitempty"`
	NetworkIfaces []networkIface `json:"network-interfaces,omitempty"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machineConfig struct {
	VCPUCount   int    `json:"vcpu_count"`
	MemSizeMiB  int    `json:"mem_size_mib"`
	SMT         bool   `json:"smt"`
	CPUTemplate string `json:"cpu_template,omitempty"`
}

type vsockConfig struct {
	VsockID  string `json:"vsock_id"`
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type loggerConfig struct {
	LogPath string `json:"log_path,omitempty"`
	Level   string `json:"level,omitempty"`
}

type networkIface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name,omitempty"`
}

func buildFirecrackerConfig(cfg *config.Config, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP string) firecrackerConfig {
	drives := []drive{
		{DriveID: "rootfs", PathOnHost: rootfsPath, IsRootDevice: true, IsReadOnly: false},
		{DriveID: "workspace", PathOnHost: workspacePath, IsRootDevice: false, IsReadOnly: false},
	}
	if strings.TrimSpace(sharedImagePath) != "" {
		drives = append(drives, drive{DriveID: "shared", PathOnHost: sharedImagePath, IsRootDevice: false, IsReadOnly: true})
	}
	fc := firecrackerConfig{
		BootSource: bootSource{KernelImagePath: cfg.MicroVMKernelPath, BootArgs: effectiveKernelArgs(cfg, guestIP)},
		Drives:     drives,
		Machine:    machineConfig{VCPUCount: cfg.MicroVMVCPUs, MemSizeMiB: cfg.MicroVMMemoryMiB, SMT: false, CPUTemplate: cfg.MicroVMCPUTemplate},
		Vsock:      vsockConfig{VsockID: "agentcy-vsock", GuestCID: 3, UDSPath: vsockUDS},
	}
	if strings.TrimSpace(tapName) != "" {
		fc.NetworkIfaces = []networkIface{{
			IfaceID:     "eth0",
			GuestMAC:    guestMACForInstance(tapName),
			HostDevName: strings.TrimSpace(tapName),
		}}
	}
	return fc
}

func effectiveKernelArgs(cfg *config.Config, guestIP string) string {
	args := strings.TrimSpace(cfg.MicroVMKernelArgs)
	if args == "" {
		args = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"
	}
	// Firecracker snapshot restore on this kernel/CPU combination faults in the
	// guest FPU restore path when XSAVE state is enabled. Disable guest XSAVE
	// until the image/kernel snapshot path can safely support it.
	if !hasKernelArg(args, "noxsave") {
		args += " noxsave"
	}
	if ipArg := guestIPBootArg(cfg, guestIP); ipArg != "" && !strings.Contains(args, "ip=") {
		args += " " + ipArg
	}
	return strings.TrimSpace(args)
}

func hasKernelArg(args, name string) bool {
	for _, field := range strings.Fields(args) {
		if field == name || strings.HasPrefix(field, name+"=") {
			return true
		}
	}
	return false
}

type identityFile struct {
	InstanceID    string `json:"instanceId"`
	AgentID       string `json:"agentId"`
	CompartmentID string `json:"compartmentId"`
	Timezone      string `json:"timezone"`
	PlatformURL   string `json:"platformApiUrl"`
	FlueStoreURL  string `json:"flueStoreUrl"`
	CreatedAt     string `json:"createdAt"`
}

func (m *Manager) flueStoreURL(agentID string) string {
	return strings.TrimRight(m.cfg.PlatformAPIURL, "/") + "/api/agents/" + agentID + "/flue-store"
}

func (m *Manager) writeIdentityFile(dir, instanceID, agentID string, opts runtimemanager.StartOpts) error {
	identity := identityFile{
		InstanceID:    instanceID,
		AgentID:       agentID,
		CompartmentID: normalizeRuntimeCompartmentID(opts.CompartmentID),
		Timezone:      strings.TrimSpace(opts.Timezone),
		PlatformURL:   m.cfg.PlatformAPIURL,
		FlueStoreURL:  m.flueStoreURL(agentID),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), data, 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}

// controlPlaneReadyTimeout bounds how long CreateAndStart waits for the in-guest
// agent to come up so startup secrets can be delivered before the runtime starts.
const controlPlaneReadyTimeout = 60 * time.Second

// buildStartupSecretBundle assembles the static-tier runtime environment (and any
// proxy CA material) delivered to the guest over vsock after boot. The agent
// runtime reads PLATFORM_API_URL / AG_FLUE_STORE_URL and proxy settings from
// this environment, so it must be applied before the runtime command starts —
// the in-guest agent blocks on it via --wait-secrets. Persistent guests must
// not receive AGENT_PLATFORM_TOKEN; host harness cells use scoped
// AGENTCY_HARNESS_TOKEN instead.
func (m *Manager) buildStartupSecretBundle(agentID, instanceID string, opts runtimemanager.StartOpts) (SecretBundle, error) {
	env := map[string]string{
		"PLATFORM_API_URL": m.cfg.PlatformAPIURL,
		// sudo is a preserved capability and safe under the microVM
		// hypervisor boundary.
		"AGENT_ENABLE_SUDO":      "true",
		"AGENT_ID":               agentID,
		"AGENT_HOST_GID":         strconv.Itoa(os.Getgid()),
		"AGENTCY_COMPARTMENT_ID": normalizeRuntimeCompartmentID(opts.CompartmentID),
		"AGENTCY_RUNTIME_TOKEN":  m.cfg.AgentRuntimeToken(agentID),
		"TZ":                     registry.NormalizeTimezone(opts.Timezone),
		"AG_FLUE_STORE_URL":      m.flueStoreURL(agentID),
		"AG_FLUE_STORE_TOKEN":    m.cfg.AgentRuntimeToken(agentID),
	}
	if strings.TrimSpace(instanceID) != "" {
		env["AGENTCY_RUNTIME_CREDENTIAL_ID"] = agentID + ":" + normalizeRuntimeCompartmentID(opts.CompartmentID) + ":" + strings.TrimSpace(instanceID)
	}
	files := map[string]string{}
	if opts.ProxyEgress != nil && opts.ProxyEgress.Enabled {
		proxyURL := m.proxyURLForGuest(opts.ProxyEgress)
		const gitHubAskpassPath = "/runtime-private/github-askpass"
		const gitConfigPath = "/runtime-private/gitconfig"
		env["AGENTCY_PROXY_EGRESS_ENABLED"] = "true"
		env["GH_TOKEN"] = "agentcy-proxy:github"
		env["GITHUB_TOKEN"] = "agentcy-proxy:github"
		env["GIT_ASKPASS"] = gitHubAskpassPath
		env["GIT_CONFIG_GLOBAL"] = gitConfigPath
		env["GIT_TERMINAL_PROMPT"] = "0"
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["NO_PROXY"] = opts.ProxyEgress.NoProxy
		env["no_proxy"] = opts.ProxyEgress.NoProxy
		env["npm_config_proxy"] = proxyURL
		env["npm_config_https_proxy"] = proxyURL
		if strings.TrimSpace(opts.ProxyEgress.NoProxy) != "" {
			env["npm_config_noproxy"] = opts.ProxyEgress.NoProxy
		}
		files["github-askpass"] = gitHubAskpassScript()
		files["gitconfig"] = gitHubProxyGitConfig()
		if token := strings.TrimSpace(opts.ProxyEgress.ContextToken); token != "" {
			env["AGENTCY_EGRESS_CONTEXT_TOKEN"] = token
		}
		if strings.TrimSpace(opts.ProxyEgress.CACertPath) != "" {
			data, err := os.ReadFile(opts.ProxyEgress.CACertPath)
			if err != nil {
				return SecretBundle{}, fmt.Errorf("read egress proxy CA cert: %w", err)
			}
			const guestCAPath = "/runtime-private/proxy-ca.crt"
			files["proxy-ca.crt"] = string(data)
			env["NODE_EXTRA_CA_CERTS"] = guestCAPath
			env["REQUESTS_CA_BUNDLE"] = guestCAPath
			env["SSL_CERT_FILE"] = guestCAPath
			env["GIT_SSL_CAINFO"] = guestCAPath
			env["CURL_CA_BUNDLE"] = guestCAPath
		}
	}
	bundle := SecretBundle{Env: env}
	if len(files) > 0 {
		bundle.Files = files
	}
	if !opts.RuntimeContextProjection.IsZero() {
		projection, err := runtimecontext.Canonicalize(opts.RuntimeContextProjection)
		if err != nil {
			return SecretBundle{}, fmt.Errorf("canonicalize runtime context projection: %w", err)
		}
		bundle.RuntimeContextProjection = projection
	}
	return bundle, nil
}

func gitHubAskpassScript() string {
	return `#!/bin/sh
case "$1" in
  *Username*|*username*)
    printf '%s\n' 'x-access-token'
    ;;
  *Password*|*password*)
    printf '%s\n' "${GITHUB_TOKEN:-${GH_TOKEN:-}}"
    ;;
  *)
    printf '\n'
    ;;
esac
`
}

func gitHubProxyGitConfig() string {
	return `[credential "https://github.com"]
	username = x-access-token
	helper =
	useHttpPath = true
[url "https://github.com/"]
	insteadOf = git@github.com:
	insteadOf = ssh://git@github.com/
`
}

func runtimeEnvDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func (m *Manager) proxyURLForGuest(proxy *runtimemanager.ProxyEgressConfig) string {
	if proxy == nil {
		return ""
	}
	rawURL := strings.TrimSpace(proxy.ProxyURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return rawURL
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	if host == "agentcy-egress-proxy" {
		gateway, _, err := net.ParseCIDR(strings.TrimSpace(m.cfg.MicroVMBridgeCIDR))
		if err == nil && gateway != nil {
			port := parsed.Port()
			if port != "" {
				parsed.Host = net.JoinHostPort(gateway.String(), port)
			} else {
				parsed.Host = gateway.String()
			}
		}
	}
	if token := strings.TrimSpace(proxy.ContextToken); token != "" {
		parsed.User = url.UserPassword("AgentcyContext", token)
	}
	return parsed.String()
}

func (m *Manager) startupFingerprint(agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
	return m.startupFingerprintWithEffectiveProfileHash(agentID, compartmentID, opts, strings.TrimSpace(opts.ShapeFingerprint))
}

func (m *Manager) startupFingerprintWithEffectiveProfileHash(agentID, compartmentID string, opts runtimemanager.StartOpts, effectiveProfileHash string) (string, error) {
	bundle, err := m.buildStartupSecretBundle(agentID, "", opts)
	if err != nil {
		return "", err
	}
	image, err := m.microVMImageForStart(opts)
	if err != nil {
		return "", err
	}
	rootfsIdentity, err := fileArtifactIdentity(image.RootfsPath)
	if err != nil {
		return "", fmt.Errorf("stat startup rootfs image: %w", err)
	}
	sharedIdentity, err := fileArtifactIdentity(image.SharedImagePath)
	if err != nil {
		return "", fmt.Errorf("stat startup shared image: %w", err)
	}
	fingerprintInput := struct {
		SecretBundle            SecretBundle                        `json:"secretBundle"`
		RuntimeClass            runtimemanager.RuntimeClass         `json:"runtimeClass"`
		RootfsPath              string                              `json:"rootfsPath"`
		RootfsIdentity          *ArtifactIdentity                   `json:"rootfsIdentity,omitempty"`
		SharedPath              string                              `json:"sharedPath,omitempty"`
		SharedIdentity          *ArtifactIdentity                   `json:"sharedIdentity,omitempty"`
		EffectiveProfileHash    string                              `json:"effectiveProfileHash,omitempty"`
		ActorPrincipal          string                              `json:"actorPrincipal,omitempty"`
		RuntimeActorContext     runtimecontext.VerifiedActorContext `json:"runtimeActorContext,omitempty"`
		ExecutorContractVersion int                                 `json:"executorContractVersion,omitempty"`
		ExecutorCapabilities    []string                            `json:"executorCapabilities,omitempty"`
	}{
		SecretBundle:            bundle,
		RuntimeClass:            image.RuntimeClass,
		RootfsPath:              image.RootfsPath,
		RootfsIdentity:          rootfsIdentity,
		SharedPath:              image.SharedImagePath,
		SharedIdentity:          sharedIdentity,
		EffectiveProfileHash:    effectiveProfileHash,
		ActorPrincipal:          strings.TrimSpace(opts.ActorPrincipal),
		RuntimeActorContext:     canonicalRuntimeActorContext(opts.RuntimeActorContext),
		ExecutorContractVersion: executorContractVersionForFingerprint(image.RuntimeClass),
		ExecutorCapabilities:    executorCapabilitiesForFingerprint(image.RuntimeClass),
	}
	data, err := json.Marshal(fingerprintInput)
	if err != nil {
		return "", fmt.Errorf("marshal startup fingerprint bundle: %w", err)
	}
	sum := sha256.New()
	sum.Write(data)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func canonicalRuntimeActorContext(actor runtimecontext.VerifiedActorContext) runtimecontext.VerifiedActorContext {
	actor.Principal = strings.TrimSpace(actor.Principal)
	actor.PlatformUserID = strings.TrimSpace(actor.PlatformUserID)
	actor.SessionID = strings.TrimSpace(actor.SessionID)
	actor.Source = strings.TrimSpace(actor.Source)
	actor.TurnContextID = strings.TrimSpace(actor.TurnContextID)
	actor.RequestID = strings.TrimSpace(actor.RequestID)
	return actor
}

func executorContractVersionForFingerprint(runtimeClass runtimemanager.RuntimeClass) int {
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	if runtimeClass != runtimemanager.RuntimeClassToolExecutor {
		return 0
	}
	return toolExecutorFingerprintContractVersion
}

func executorCapabilitiesForFingerprint(runtimeClass runtimemanager.RuntimeClass) []string {
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	if runtimeClass != runtimemanager.RuntimeClassToolExecutor {
		return nil
	}
	caps := append([]string(nil), toolExecutorFingerprintCapabilities...)
	sort.Strings(caps)
	return caps
}

func (m *Manager) workspaceSeedDir(agentID, compartmentID string) string {
	defaultSeed := filepath.Join(m.cfg.AgentsDir(), agentID, "workspace")
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if compartmentID == "" {
		return defaultSeed
	}
	compartmentSeed := filepath.Join(m.cfg.AgentsDir(), agentID, "compartments", compartmentID, "workspace")
	if info, err := os.Stat(compartmentSeed); err == nil && info.IsDir() {
		return compartmentSeed
	}
	return defaultSeed
}

// waitForControlPlane blocks until the in-guest agent answers a heartbeat over
// vsock, the VM exits, or the timeout/ctx elapses.
func (m *Manager) waitForControlPlane(ctx context.Context, inst *instance, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := inst.controlClient(time.Second).Heartbeat(ctx); err == nil {
			return nil
		}
		if inst.launcherOnly {
			if time.Since(inst.startedAt) > 2*time.Second {
				running, err := firecrackerProcessRunning(inst.id)
				if err == nil && !running {
					return fmt.Errorf("jailed firecracker process exited before its control plane became ready%s%s", inst.jailDiagnostics(), inst.logTailDiagnostics(120))
				}
			}
		} else {
			select {
			case <-inst.done:
				return fmt.Errorf("microVM exited before its control plane became ready")
			default:
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for microVM control plane after %s%s%s", timeout, inst.jailDiagnostics(), inst.logTailDiagnostics(80))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// deliverStartupSecrets waits for the guest control plane, then applies the
// runtime environment bundle over vsock so the runtime can reach the platform
// API and the Flue durable store before its first turn.
func (m *Manager) deliverStartupSecrets(ctx context.Context, inst *instance, agentID string, opts runtimemanager.StartOpts, timer *coldStartStageTimer) error {
	bundle, err := m.buildStartupSecretBundle(agentID, inst.id, opts)
	if err != nil {
		return err
	}
	timer.mark("startup_secret_bundle_ready")
	if err := m.waitForControlPlane(ctx, inst, controlPlaneReadyTimeout); err != nil {
		return err
	}
	timer.mark("control_plane_ready")
	if err := inst.controlClient(15*time.Second).ApplySecrets(ctx, bundle); err != nil {
		return fmt.Errorf("apply runtime secrets: %w", err)
	}
	timer.mark("startup_secrets_applied")
	return nil
}

func (inst *instance) jailDiagnostics() string {
	if inst == nil || inst.jailRoot == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\njail diagnostics:")
	for _, rel := range []string{"dev", "dev/net", "dev/net/tun", "dev/kvm", firecrackerSockName, configName} {
		path := filepath.Join(inst.jailRoot, rel)
		info, err := os.Lstat(path)
		if err != nil {
			b.WriteString("\n  ")
			b.WriteString(rel)
			b.WriteString(": ")
			b.WriteString(err.Error())
			continue
		}
		b.WriteString("\n  ")
		b.WriteString(rel)
		b.WriteString(": mode=")
		b.WriteString(info.Mode().String())
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			b.WriteString(" uid=")
			b.WriteString(strconv.FormatUint(uint64(stat.Uid), 10))
			b.WriteString(" gid=")
			b.WriteString(strconv.FormatUint(uint64(stat.Gid), 10))
			if info.Mode()&os.ModeDevice != 0 {
				b.WriteString(" rdev=")
				b.WriteString(strconv.FormatUint(uint64(stat.Rdev), 10))
			}
		}
	}
	for _, path := range []string{filepath.Join(inst.jailRoot, "firecracker.pid"), filepath.Join(filepath.Dir(inst.jailRoot), "firecracker.pid")} {
		if pidData, err := os.ReadFile(path); err == nil {
			b.WriteString("\n  ")
			b.WriteString(path)
			b.WriteString("=")
			b.WriteString(strings.TrimSpace(string(pidData)))
		}
	}
	if inst.tapName != "" {
		for _, args := range [][]string{
			{"tuntap", "show", inst.tapName},
			{"-details", "link", "show", "dev", inst.tapName},
		} {
			out, err := exec.Command("ip", args...).CombinedOutput()
			b.WriteString("\n  ip ")
			b.WriteString(strings.Join(args, " "))
			b.WriteString(": ")
			if err != nil {
				b.WriteString(err.Error())
				if len(out) > 0 {
					b.WriteString(": ")
				}
			}
			b.WriteString(strings.TrimSpace(string(out)))
		}
	}
	return b.String()
}

func (inst *instance) logTailDiagnostics(lines int) string {
	if inst == nil || strings.TrimSpace(inst.logPath) == "" {
		return ""
	}
	data, err := os.ReadFile(inst.logPath)
	if err != nil {
		return "\nfirecracker log tail: " + err.Error()
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "\nfirecracker log tail: empty"
	}
	all := strings.Split(trimmed, "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return "\nfirecracker log tail:\n" + strings.Join(all, "\n")
}

func (m *Manager) reap(inst *instance) {
	err := inst.cmd.Wait()
	if err != nil {
		if inst.launcherOnly {
			slog.Warn("firecracker jailer launcher exited with error", "agent", inst.agentID, "instance", inst.id, "error", err)
		} else {
			slog.Warn("firecracker microVM exited", "agent", inst.agentID, "instance", inst.id, "error", err)
		}
	}
	if inst.launcherOnly {
		// cmd was only the short-lived jailer parent; the real Firecracker
		// process keeps running as an orphaned child (identified by --id). Watch
		// for its exit so the guest IP, source binding, and TAP are reclaimed
		// when the VM stops on its own, not only via an explicit stopInstance.
		m.watchJailedExit(inst)
		return
	}
	m.finishInstance(inst)
}

// jailedExitPollInterval controls how often watchJailedExit polls for the real
// (orphaned) Firecracker process to exit. It is a var so tests can shorten it.
var jailedExitPollInterval = 2 * time.Second

// watchJailedExit blocks until the jailed Firecracker process for inst exits, or
// an explicit stop closes inst.done, then runs finishInstance (made idempotent
// by doneOnce). Unlike the active stop path, a transient failure to scan /proc
// is retried rather than treated as an exit, so a live VM is never torn down by
// mistake.
func (m *Manager) watchJailedExit(inst *instance) {
	ticker := time.NewTicker(jailedExitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-inst.done:
			return
		case <-ticker.C:
			running, err := firecrackerProcessRunningFunc(inst.id)
			if err != nil {
				continue
			}
			if !running {
				m.finishInstance(inst)
				return
			}
		}
	}
}

func (m *Manager) finishInstance(inst *instance) {
	if inst == nil {
		return
	}
	inst.doneOnce.Do(func() {
		m.mu.Lock()
		m.removeInstanceLocked(inst)
		m.mu.Unlock()
		m.unregisterSourceBinding(inst.id)
		m.releaseGuestIP(inst.id)
		m.cleanupTap(context.Background(), inst.tapName)
		close(inst.done)
	})
}

var firecrackerProcessRunningFunc = firecrackerProcessRunning

// firecrackerInstanceIDsForAgentFunc is overridable in tests; production uses the
// real /proc-based enumeration.
var firecrackerInstanceIDsForAgentFunc = firecrackerInstanceIDsForAgent

func firecrackerProcessRunning(instanceID string) (bool, error) {
	pids, err := firecrackerPIDs(instanceID)
	if err != nil {
		return false, err
	}
	return len(pids) > 0, nil
}

func signalFirecrackerByID(instanceID string, sig syscall.Signal) error {
	pids, err := firecrackerPIDs(instanceID)
	if err != nil {
		return err
	}
	var joined error
	for _, pid := range pids {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func firecrackerPIDs(instanceID string) ([]int, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return nil, nil
	}
	return firecrackerPIDsMatching(func(id string) bool {
		return id == instanceID
	})
}

func firecrackerInstanceIDsForAgent(agentID string) ([]string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	prefix := instancePrefix + "-" + agentID + "-"
	seen := map[string]struct{}{}
	if _, err := firecrackerPIDsMatching(func(id string) bool {
		if strings.HasPrefix(id, prefix) {
			seen[id] = struct{}{}
			return true
		}
		return false
	}); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

func firecrackerPIDsMatching(match func(string) bool) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--id" && match(args[i+1]) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

func (m *Manager) Stop(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return nil
	}
	return m.stopInstance(ctx, inst, false)
}

func (m *Manager) StopAndRemove(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return nil
	}
	return m.stopInstance(ctx, inst, true)
}

func (m *Manager) StopAndRemoveCompartment(ctx context.Context, agentID, compartmentID string) error {
	agentID = strings.TrimSpace(agentID)
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	m.mu.Lock()
	var instances []*instance
	for _, inst := range m.instances {
		if inst == nil {
			continue
		}
		if inst.agentID == agentID && inst.compartmentID == compartmentID {
			instances = append(instances, inst)
		}
	}
	m.mu.Unlock()

	var joined error
	for _, inst := range instances {
		if err := m.stopInstance(ctx, inst, true); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (m *Manager) Remove(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return nil
	}
	return m.stopInstance(ctx, inst, true)
}

func (m *Manager) stopInstance(ctx context.Context, inst *instance, removeFiles bool) error {
	if inst == nil {
		return nil
	}
	if inst.launcherOnly {
		_ = signalFirecrackerByID(inst.id, syscall.SIGTERM)
		kill := time.AfterFunc(5*time.Second, func() {
			_ = signalFirecrackerByID(inst.id, syscall.SIGKILL)
		})
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			running, err := firecrackerProcessRunningFunc(inst.id)
			if err == nil && !running {
				kill.Stop()
				m.finishInstance(inst)
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	} else {
		if inst.cmd == nil || inst.cmd.Process == nil {
			m.finishInstance(inst)
		} else {
			_ = syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGTERM)
			// Escalate to SIGKILL after a grace period. The reaper (started at create)
			// observes exit, runs cleanup exactly once, and signals via inst.done.
			kill := time.AfterFunc(5*time.Second, func() {
				_ = syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL)
			})
			select {
			case <-inst.done:
				kill.Stop()
			case <-ctx.Done():
				// Leave the kill timer to escalate so the reaper still cleans up.
				return ctx.Err()
			}
		}
	}
	if removeFiles {
		_ = os.RemoveAll(inst.dir)
		if inst.jailRoot != "" {
			_ = os.RemoveAll(filepath.Dir(inst.jailRoot))
		}
	}
	return nil
}

func (m *Manager) IsRunning(ctx context.Context, instanceID string) (bool, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return false, nil
	}
	if hb, err := inst.controlClient(500 * time.Millisecond).Heartbeat(ctx); err == nil {
		return hb.Healthy, nil
	}
	if inst.launcherOnly {
		return firecrackerProcessRunning(inst.id)
	}
	if inst.cmd == nil || inst.cmd.Process == nil {
		return false, nil
	}
	if err := inst.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) Logs(ctx context.Context, instanceID string, tail string) (io.ReadCloser, error) {
	if inst := m.lookup(instanceID); inst != nil {
		if data, err := inst.controlClient(2*time.Second).Logs(ctx, tail); err == nil {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	}
	path := m.logPath(instanceID)
	if path == "" {
		return nil, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(tail))
	if n <= 0 {
		return f, nil
	}
	return newTailReadCloser(f, n), nil
}

func (m *Manager) LogsStream(ctx context.Context, instanceID string) (io.ReadCloser, error) {
	path := m.logPath(instanceID)
	if path == "" {
		return nil, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	cmd := exec.CommandContext(ctx, "tail", "-n", "50", "-F", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processReadCloser{ReadCloser: stdout, cmd: cmd}, nil
}

func (m *Manager) ContainerIP(_ context.Context, instanceID string) (string, error) {
	if ip := strings.TrimSpace(m.guestIP(instanceID)); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("microVM instance %s has no reserved guest IP", instanceID)
}

func (m *Manager) Heartbeat(ctx context.Context, instanceID string) (HeartbeatResponse, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return HeartbeatResponse{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(2 * time.Second).Heartbeat(ctx)
}

func (m *Manager) Pause(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.apiClient(5 * time.Second).Pause(ctx)
}

func (m *Manager) Resume(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.apiClient(5 * time.Second).Resume(ctx)
}

func (m *Manager) CreateFullSnapshot(ctx context.Context, instanceID, snapshotPath, memFilePath string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.apiClient(30*time.Second).CreateFullSnapshot(ctx, snapshotPath, memFilePath)
}

func (m *Manager) PutMMDS(ctx context.Context, instanceID string, data any) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	client := inst.apiClient(5 * time.Second)
	if inst.tapName != "" {
		if err := client.ConfigureMMDSV2(ctx, []string{"eth0"}); err != nil {
			return err
		}
	}
	return client.PutMMDS(ctx, data)
}

func (m *Manager) ListWorkspace(ctx context.Context, instanceID, relPath string) (WorkspaceListResponse, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return WorkspaceListResponse{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(5*time.Second).ListWorkspace(ctx, relPath)
}

func (m *Manager) ListWorkspaceEntries(ctx context.Context, instanceID, relPath string) ([]runtimemanager.WorkspaceEntry, error) {
	resp, err := m.ListWorkspace(ctx, instanceID, relPath)
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

func (m *Manager) ReadWorkspaceFile(ctx context.Context, instanceID, relPath string, maxBytes int64) ([]byte, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return nil, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(10*time.Second).ReadWorkspaceFile(ctx, relPath, maxBytes)
}

func (m *Manager) OpenWorkspaceFileStream(ctx context.Context, instanceID, relPath string) (io.ReadCloser, int64, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return nil, 0, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(0).OpenWorkspaceFileStream(ctx, relPath)
}

func (m *Manager) PutWorkspaceFileStream(ctx context.Context, instanceID, relPath string, r io.Reader) (int64, string, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return 0, "", fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(0).PutWorkspaceFileStream(ctx, relPath, r)
}

func (m *Manager) ExecuteTool(ctx context.Context, instanceID string, req ToolExecRequest) (ToolExecResponse, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return ToolExecResponse{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(maxSandboxToolTimeout(req)).ExecuteTool(ctx, req)
}

func maxSandboxToolTimeout(req ToolExecRequest) time.Duration {
	if req.TimeoutMillis <= 0 {
		return 10 * time.Minute
	}
	timeout := time.Duration(req.TimeoutMillis)*time.Millisecond + 5*time.Second
	if timeout < 10*time.Second {
		return 10 * time.Second
	}
	return timeout
}

func (m *Manager) FreezeWorkspace(ctx context.Context, instanceID string) (BackupResponse, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return BackupResponse{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(10 * time.Second).FreezeWorkspace(ctx)
}

func (m *Manager) ThawWorkspace(ctx context.Context, instanceID string) (BackupResponse, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return BackupResponse{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(10 * time.Second).ThawWorkspace(ctx)
}

func (m *Manager) ApplySecrets(ctx context.Context, instanceID string, bundle SecretBundle) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(10*time.Second).ApplySecrets(ctx, bundle)
}

func (m *Manager) HardenPostRestore(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.controlClient(10*time.Second).HardenPostRestore(ctx, time.Now().UTC())
}

func (m *Manager) registerSourceBinding(ctx context.Context, agentID, instanceID string, opts runtimemanager.StartOpts) (string, error) {
	if m.sourceBindings == nil {
		return "", nil
	}
	if opts.ProxyEgress == nil || !opts.ProxyEgress.Enabled {
		return "", nil
	}
	sourceIP := strings.TrimSpace(m.guestIP(instanceID))
	if sourceIP == "" {
		return "", fmt.Errorf("register proxy source binding: no guest IP reserved for %s (set AG_MICROVM_BRIDGE_CIDR or AG_MICROVM_GUEST_IP)", instanceID)
	}
	contextToken := ""
	if strings.TrimSpace(m.cfg.AgentRuntimeAuthSecret) != "" {
		var err error
		contextToken, err = egressproxy.MintContextToken(m.cfg.AgentRuntimeAuthSecret, egressproxy.ContextTokenClaims{
			AgentID:           agentID,
			CompartmentID:     normalizeRuntimeCompartmentID(opts.CompartmentID),
			PlaceID:           strings.TrimSpace(opts.ProxyEgress.PlaceID),
			CredentialSubject: strings.TrimSpace(opts.ActorPrincipal),
			PlatformUserID:    strings.TrimSpace(opts.RuntimeActorContext.PlatformUserID),
			AgentSessionID:    strings.TrimSpace(opts.RuntimeActorContext.SessionID),
			RequestID:         strings.TrimSpace(opts.RuntimeActorContext.RequestID),
			EgressID:          instanceID,
		}, time.Now().UTC(), egressproxy.ContextTokenTTL)
		if err != nil {
			return "", fmt.Errorf("mint proxy egress context token: %w", err)
		}
	}
	binding := egressproxy.SourceBinding{
		AgentID:             agentID,
		CompartmentID:       normalizeRuntimeCompartmentID(opts.CompartmentID),
		ContainerID:         instanceID,
		SourceIP:            sourceIP,
		Generation:          instanceID,
		ContextToken:        contextToken,
		AllowlistEnabled:    opts.ProxyEgress.AllowlistEnabled,
		AllowedHosts:        append([]string(nil), opts.ProxyEgress.AllowedHosts...),
		ToolCredentialHosts: append([]string(nil), opts.ProxyEgress.ToolCredentialHosts...),
	}
	if err := m.sourceBindings.Register(binding); err != nil {
		return "", fmt.Errorf("register proxy source binding: %w", err)
	}
	if opts.ProxyEgress.TransparentHTTPPort > 0 && m.egressRouter != nil {
		tapName := ""
		if inst := m.lookup(instanceID); inst != nil {
			tapName = inst.tapName
		}
		if err := m.egressRouter.RegisterTransparentRoute(ctx, TransparentRoute{
			AgentID:     agentID,
			InstanceID:  instanceID,
			SourceIP:    sourceIP,
			HTTPPort:    opts.ProxyEgress.TransparentHTTPPort,
			InterfaceID: tapName,
		}); err != nil {
			m.unregisterSourceBinding(instanceID)
			return "", fmt.Errorf("register transparent egress route: %w", err)
		}
	}
	return contextToken, nil
}

func (m *Manager) unregisterSourceBinding(instanceID string) {
	if m.sourceBindings == nil || strings.TrimSpace(instanceID) == "" {
		return
	}
	m.sourceBindings.UnregisterContainer(instanceID)
	if m.egressRouter != nil {
		m.egressRouter.UnregisterContainer(instanceID)
	}
}

func (m *Manager) cleanupTap(ctx context.Context, tapName string) {
	if m.network == nil || strings.TrimSpace(tapName) == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.network.RemoveTap(cleanupCtx, tapName); err != nil {
		slog.Warn("failed to remove microVM tap", "tap", tapName, "error", err)
	}
	_ = ctx
}

func (m *Manager) lookup(instanceID string) *instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[instanceID]
}

func (m *Manager) admitCompartmentSpawnLocked(key runtimeInstanceKey) error {
	liveForAgent := 0
	for _, inst := range m.instances {
		if inst == nil || strings.TrimSpace(inst.agentID) != key.agentID {
			continue
		}
		if !inst.reaping {
			liveForAgent++
		}
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
		if inst == nil || strings.TrimSpace(inst.agentID) == "" {
			continue
		}
		out.ConcurrentVMsByAgent[inst.agentID]++
	}
	out.GuestIPsInUse = len(m.guestIPs)
	if m.cfg != nil {
		out.GuestIPCapacity = microVMGuestIPCapacity(m.cfg)
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

func (m *Manager) addInstanceLocked(inst *instance) {
	if m.instances == nil {
		m.instances = map[string]*instance{}
	}
	if inst.done == nil {
		inst.done = make(chan struct{})
	}
	inst.agentID = strings.TrimSpace(inst.agentID)
	inst.compartmentID = normalizeRuntimeCompartmentID(inst.compartmentID)
	m.instances[inst.id] = inst
}

func (m *Manager) removeInstanceLocked(inst *instance) {
	if inst == nil {
		return
	}
	if m.instancesByKey != nil && inst.warmToolVM {
		key := runtimeInstanceKey{agentID: strings.TrimSpace(inst.agentID), compartmentID: normalizeRuntimeCompartmentID(inst.compartmentID)}
		if m.instancesByKey[key] == inst.id {
			delete(m.instancesByKey, key)
		}
	}
	delete(m.instances, inst.id)
}

func normalizeRuntimeCompartmentID(compartmentID string) string {
	return strings.TrimSpace(compartmentID)
}

func validateRuntimeCompartmentID(compartmentID string) error {
	compartmentID = strings.TrimSpace(compartmentID)
	if compartmentID == "" {
		return fmt.Errorf("compartment id is required")
	}
	if compartmentID == registry.CompartmentKindDefault {
		return fmt.Errorf("default is not a valid runtime compartment")
	}
	return nil
}

func (i *instance) controlClient(timeout time.Duration) ControlClient {
	return ControlClient{UDSPath: i.vsockUDS, Port: defaultControlPort, Timeout: timeout}
}

func (i *instance) apiClient(timeout time.Duration) FirecrackerAPIClient {
	return FirecrackerAPIClient{SocketPath: i.socket, Timeout: timeout}
}

func (m *Manager) logPath(instanceID string) string {
	if inst := m.lookup(instanceID); inst != nil {
		return inst.logPath
	}
	path := filepath.Join(m.cfg.MicroVMLogDir, instanceID+".log")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

type processReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (p *processReadCloser) Close() error {
	err := p.ReadCloser.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
	return err
}

type tailReadCloser struct {
	*strings.Reader
	close func() error
}

func newTailReadCloser(f *os.File, lines int) io.ReadCloser {
	defer f.Close()
	var all []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	if lines < len(all) {
		all = all[len(all)-lines:]
	}
	return &tailReadCloser{Reader: strings.NewReader(strings.Join(all, "\n") + "\n"), close: func() error { return nil }}
}

func (t *tailReadCloser) Close() error {
	if t.close != nil {
		return t.close()
	}
	return nil
}
