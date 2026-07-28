package microvm

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-manager/internal/config"
	"agent-manager/internal/flow"
	"agent-manager/internal/registry"
	"agent-manager/internal/runtimemanager"
)

const (
	instancePrefix      = "fc"
	workspaceName       = "workspace.ext4"
	rootfsName          = "rootfs.ext4"
	kernelName          = "vmlinux"
	sharedImageName     = "shared.img"
	firecrackerSockName = "firecracker.sock"
	vsockUDSName        = "agent-manager.vsock"
	configName          = "firecracker.json"
)

// maxUnixSocketPathLen is the kernel's sockaddr_un.sun_path capacity (including the
// trailing NUL). Firecracker's API socket and the vsock UDS must fit within it.
// Jailed mode binds a short path relative to the jail chroot; unjailed mode has no
// chroot and binds the full host path, so a deep AGENT_MANAGER_DATA_DIR otherwise yields the
// cryptic "path must be shorter than SUN_LEN" at boot.
const maxUnixSocketPathLen = 108

// reservedRunDirBudget reserves room under maxUnixSocketPathLen for the longest
// current per-instance suffix: "/fc-<32-char-agent>-<16-char-compartment>-<8-char-random>/firecracker.sock".
const reservedRunDirBudget = 80

//go:embed firecracker.lock
var firecrackerVersionLock string

// Manager owns Firecracker-backed agent runtime instances.
type Manager struct {
	cfg              *config.Config
	mu               sync.Mutex
	instances        map[string]*instance
	instancesByKey   map[runtimeInstanceKey]string
	provisioning     map[runtimeInstanceKey]chan struct{}
	pendingSpawns    map[runtimeInstanceKey]int
	shuttingDown     bool
	sweepCancel      context.CancelFunc
	sweepDone        chan struct{}
	guestIPs         map[string]string // instanceID -> reserved guest IP
	launcher         *privilegedLauncherClient
	sourceBindings   SourceBindingRegistrar
	network          HostNetworkConfigurer
	egressRouter     HostEgressRouter
	trustedArtifacts *trustedMicroVMArtifacts
	startCompartment func(context.Context, string, string, runtimemanager.StartOpts) (string, error)
	executeTool      func(context.Context, string, ToolExecRequest) (ToolExecResponse, error)
	freezeWorkspace  func(context.Context, string) (BackupResponse, error)
	removeInstance   func(context.Context, string) error
	flowRecorder     flow.Recorder
	startDurations   []time.Duration
	mountLocks       map[runtimeInstanceKey]*sync.Mutex
	thinDeviceIDs    *thinDeviceIDAllocator
	thinDeviceIDOnce sync.Once
}

func (m *Manager) SetFlowRecorder(recorder flow.Recorder) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flowRecorder = recorder
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
	KernelPath      string
	RootfsPath      string
	SharedImagePath string
}

type trustedMicroVMArtifacts struct {
	files []trustedMicroVMArtifactFile
}

type trustedMicroVMArtifactFile struct {
	label    string
	path     string
	identity trustedMicroVMArtifactIdentity
}

type trustedMicroVMArtifactIdentity struct {
	dev             uint64
	ino             uint64
	size            int64
	modTimeUnixNano int64
	ctimeUnixNano   int64
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
	launcherMode := strings.TrimSpace(cfg.MicroVMLauncherSocket) != ""
	if !launcherMode {
		if err := requireExecutable("firecracker", cfg.FirecrackerPath); err != nil {
			return nil, err
		}
		if err := ensureFirecrackerVersion(cfg.FirecrackerPath); err != nil {
			return nil, err
		}
	}
	if !cfg.MicroVMAllowUnjailed && !launcherMode {
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
	trustedArtifacts, err := verifyAndCaptureTrustedMicroVMArtifacts(cfg)
	if err != nil {
		return nil, fmt.Errorf("verify microVM trust anchor: %w", err)
	}
	if cfg.MicroVMAllowUnjailed {
		if relocated, ok := relocateRunDirForUnixSockets(cfg.MicroVMRunDir); ok {
			originalRunDir := cfg.MicroVMRunDir
			if err := ensureShortRunDirAlias(relocated, originalRunDir); err != nil {
				return nil, fmt.Errorf("create short microVM run-dir alias: %w", err)
			}
			slog.Warn("microVM run dir is too long for unjailed unix sockets; relocating to keep API/vsock paths under the kernel limit",
				"from", originalRunDir, "to", relocated, "limit", maxUnixSocketPathLen)
			cfg.MicroVMRunDir = relocated
		}
	}
	for _, dir := range []string{cfg.MicroVMWorkspaceDir, cfg.MicroVMRunDir, cfg.MicroVMLogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create microVM dir %q: %w", dir, err)
		}
	}
	m := &Manager{
		cfg:              cfg,
		instances:        map[string]*instance{},
		instancesByKey:   map[runtimeInstanceKey]string{},
		provisioning:     map[runtimeInstanceKey]chan struct{}{},
		pendingSpawns:    map[runtimeInstanceKey]int{},
		guestIPs:         map[string]string{},
		network:          IPTapConfigurer{},
		trustedArtifacts: trustedArtifacts,
	}
	if launcherMode {
		client := newPrivilegedLauncherClient(cfg.MicroVMLauncherSocket)
		m.launcher = client
		m.network = client
		m.egressRouter = client
	}
	return m, nil
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := m.sweepStartupOrphans(ctx); err != nil {
		slog.Warn("startup orphan microVM sweep completed with unreclaimed entries", "error", err)
	}
	if deleted, err := m.pruneLogs(time.Now().UTC(), 7*24*time.Hour); err != nil {
		slog.Warn("failed to prune stale microVM logs", "error", err)
	} else if deleted > 0 {
		slog.Info("pruned stale microVM logs", "count", deleted)
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
		if inst == nil || inst.reaping {
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
			m.teardownManagedVMContext(ctx, inst)
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
		if m.launcher != nil {
			jailRoot = ""
			running, err = m.launcher.Running(ctx, instanceID)
		}
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
	m.mu.Lock()
	if err := m.reserveCompartmentSpawnLocked(key); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	releasePendingLocked := sync.OnceFunc(func() {
		m.releaseCompartmentSpawnLocked(key)
	})
	releasePending := func() {
		m.mu.Lock()
		releasePendingLocked()
		m.mu.Unlock()
	}
	defer releasePending()
	// Single-guest-IP mode admits only one compartment per agent, so a Firecracker
	// process for this agent that the manager is not tracking is a leaked orphan
	// that still holds the shared guest IP. Reclaim it before this cold boot
	// reserves the same IP, which would otherwise put two VMs on one address. With
	// a bridge CIDR each VM gets a distinct IP and legitimate sibling/in-flight
	// spawns must not be touched.
	if m.cfg != nil && strings.TrimSpace(m.cfg.MicroVMBridgeCIDR) == "" {
		m.cleanupUntrackedAgentOrphans(ctx, agentID)
	}
	return m.startCompartmentInstance(ctx, agentID, compartmentID, opts, releasePendingLocked)
}

// ExecuteEphemeralTool starts a fresh Firecracker tool VM for one dangerous
// operation, executes the request, then tears the VM down. Every sandbox hand
// gets its own ephemeral VM; there is no warm-VM reuse.
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

func (m *Manager) startCompartmentInstance(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, onRegisteredLocked func()) (id string, err error) {
	started := time.Now()
	defer func() {
		if err == nil && strings.TrimSpace(id) != "" {
			m.recordStartDuration(time.Since(started))
		}
	}()
	if m.startCompartment != nil {
		return m.startCompartment(ctx, agentID, compartmentID, opts)
	}
	return m.createAndStartCold(ctx, agentID, compartmentID, opts, onRegisteredLocked)
}

func (m *Manager) createAndStartCold(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts, onRegisteredLocked func()) (string, error) {
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
			GuestIP:    guestIP,
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
	var logFile *os.File
	if m.launcher == nil {
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			m.cleanupTap(ctx, tapName)
			return "", fmt.Errorf("open microVM log: %w", err)
		}
		defer logFile.Close()
	}
	timer.mark("instance_files_ready", "dir", dir, "log", logPath)

	image, err := m.microVMImageForStart(opts)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	launchImage, err := m.prepareLaunchImage(dir, image)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	timer.mark("trust_anchor_verified")
	timer.mark("rootfs_reflinked")
	workspaceSizeMiB := m.cfg.MicroVMWorkspaceSizeMiB
	sharedImagePath := launchImage.SharedImagePath
	if opts.SandboxPolicy != nil {
		workspaceSizeMiB = opts.SandboxPolicy.WorkspaceSizeMiB
		if !opts.SandboxPolicy.SharedReadOnly {
			sharedImagePath = ""
		}
	}
	workspacePath, err := m.prepareWorkspaceSized(setupCtx, agentID, compartmentID, workspaceSizeMiB)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("prepare workspace: %w", err)
	}
	timer.mark("workspace_ready", "workspace", workspacePath)
	// workspacePath is the compartment's persistent VM-local disk, seeded once
	// from the agent workspace. It is deliberately never removed on the error
	// paths below so a restart does not destroy saved compartment-local work.

	if err := m.writeIdentityFile(dir, id, agentID, opts); err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", err
	}
	startupFingerprint, err := m.startupFingerprint(agentID, compartmentID, opts)
	if err != nil {
		m.cleanupTap(setupCtx, tapName)
		return "", fmt.Errorf("build startup fingerprint: %w", err)
	}
	timer.mark("launch_config_ready")

	var cmd *exec.Cmd
	var socketPath, vsockPath, jailRoot string
	launcherOnly := m.launcher != nil
	if launcherOnly {
		request := buildPrivilegedLaunchRequest(id, agentID, compartmentID, launchImage, image, workspacePath, tapName, guestIP, opts.SandboxPolicy)
		resp, launchErr := m.launcher.Launch(setupCtx, request)
		if launchErr != nil {
			m.cleanupTap(setupCtx, tapName)
			return "", fmt.Errorf("launch firecracker through privileged launcher: %w", launchErr)
		}
		socketPath, vsockPath, jailRoot = resp.SocketPath, resp.VsockPath, resp.JailRoot
		if strings.TrimSpace(resp.LogPath) != "" {
			logPath = resp.LogPath
		}
		timer.mark("firecracker_process_started", "launcher", m.cfg.MicroVMLauncherSocket)
	} else {
		launch, launchErr := m.prepareLaunchWithPolicy(setupCtx, id, dir, launchImage.KernelPath, launchImage.RootfsPath, workspacePath, sharedImagePath, tapName, guestIP, opts.SandboxPolicy)
		if launchErr != nil {
			m.cleanupTap(setupCtx, tapName)
			return "", launchErr
		}
		timer.mark("launch_prepared", "jailRoot", launch.jailRoot)
		data, marshalErr := json.MarshalIndent(launch.config, "", "  ")
		if marshalErr != nil {
			m.cleanupLaunch(launch)
			m.cleanupTap(setupCtx, tapName)
			return "", fmt.Errorf("marshal firecracker config: %w", marshalErr)
		}
		if writeErr := os.WriteFile(launch.configPath, data, 0o600); writeErr != nil {
			m.cleanupLaunch(launch)
			m.cleanupTap(setupCtx, tapName)
			return "", fmt.Errorf("write firecracker config: %w", writeErr)
		}
		if launch.jailRoot != "" {
			if chownErr := chownIfDifferent(launch.configPath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); chownErr != nil {
				m.cleanupLaunch(launch)
				m.cleanupTap(setupCtx, tapName)
				return "", fmt.Errorf("chown jailed firecracker config: %w", chownErr)
			}
		}
		cmd = exec.Command(launch.executable, launch.args...)
		cmd.Dir = dir
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if startErr := cmd.Start(); startErr != nil {
			m.cleanupLaunch(launch)
			m.cleanupTap(setupCtx, tapName)
			return "", fmt.Errorf("start firecracker: %w", startErr)
		}
		socketPath, vsockPath, jailRoot = launch.socketPath, launch.vsockUDS, launch.jailRoot
		launcherOnly = launch.jailRoot != ""
		timer.mark("firecracker_process_started", "pid", cmd.Process.Pid)
	}

	inst := &instance{
		id:                 id,
		agentID:            agentID,
		compartmentID:      compartmentID,
		dir:                dir,
		logPath:            logPath,
		socket:             socketPath,
		vsockUDS:           vsockPath,
		tapName:            tapName,
		jailRoot:           jailRoot,
		rootfsPath:         launchImage.RootfsPath,
		rootfsImagePath:    image.RootfsPath,
		workspacePath:      workspacePath,
		sharedImagePath:    launchImage.SharedImagePath,
		guestIP:            guestIP,
		startupFingerprint: startupFingerprint,
		cmd:                cmd,
		startedAt:          time.Now().UTC(),
		done:               make(chan struct{}),
		launcherOnly:       launcherOnly,
	}
	m.registerStartingInstance(inst, onRegisteredLocked)
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

func (m *Manager) reap(inst *instance) {
	var err error
	if inst.cmd != nil {
		err = inst.cmd.Wait()
	}
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
			if m.launcher != nil {
				running, err = m.launcher.Running(context.Background(), inst.id)
			}
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
		// Release the guest identity only after the launcher tap and state are gone.
		// If cleanup fails the launcher may still claim this IP/MAC, so retain the
		// reservation fail-closed rather than let a concurrent start recycle it into
		// an ownership conflict.
		if err := m.cleanupTapChecked(context.Background(), inst.tapName); err != nil {
			slog.Warn("microVM tap cleanup failed; retaining guest identity reservation", "instance", inst.id, "tap", inst.tapName, "error", err)
		} else {
			m.releaseGuestIP(inst.id)
		}
		close(inst.done)
	})
}

var firecrackerProcessRunningFunc = firecrackerProcessRunning
var signalFirecrackerByIDFunc = signalFirecrackerByID

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
	prefix := instancePrefix + "-" + instanceAgentIDSegment(agentID) + "-"
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
		if m.launcher != nil {
			if err := m.launcher.Stop(ctx, inst.id); err != nil {
				return err
			}
			m.finishInstance(inst)
		} else {
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
		if strings.TrimSpace(inst.logPath) != "" {
			_ = os.Remove(inst.logPath)
		}
		if inst.jailRoot != "" && m.launcher == nil {
			_ = os.RemoveAll(filepath.Dir(inst.jailRoot))
		}
	}
	return nil
}

func (m *Manager) pruneLogs(now time.Time, maxAge time.Duration) (int, error) {
	if m == nil || m.cfg == nil || maxAge <= 0 || strings.TrimSpace(m.cfg.MicroVMLogDir) == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(m.cfg.MicroVMLogDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := now.Add(-maxAge)
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return deleted, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(m.cfg.MicroVMLogDir, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
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
		if m.launcher != nil {
			return m.launcher.Running(ctx, inst.id)
		}
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

func (m *Manager) lookup(instanceID string) *instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[instanceID]
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

func (m *Manager) registerStartingInstance(inst *instance, onRegisteredLocked func()) {
	m.mu.Lock()
	m.addInstanceLocked(inst)
	if onRegisteredLocked != nil {
		onRegisteredLocked()
	}
	m.mu.Unlock()
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
