package firecracker

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

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

const (
	instancePrefix      = "fc"
	workspaceName       = "workspace.ext4"
	rootfsName          = "rootfs.ext4"
	kernelName          = "vmlinux"
	sharedImageName     = "shared.img"
	firecrackerSockName = "firecracker.sock"
	vsockUDSName        = "guest.vsock"
	configName          = "firecracker.json"
)

// maxUnixSocketPathLen is the kernel's sockaddr_un.sun_path capacity (including the
// trailing NUL). Firecracker's API socket and the vsock UDS must fit within it.
// Jailed mode binds a short path relative to the jail chroot; unjailed mode has no
// chroot and binds the full host path, so a deep SECONDBOX_GUEST_DATA_DIR otherwise yields the
// cryptic "path must be shorter than SUN_LEN" at boot.
const maxUnixSocketPathLen = 108

// reservedRunDirBudget reserves room under maxUnixSocketPathLen for the longest
// current per-instance suffix: "/fc-<32-char-sandbox>-<16-char-compartment>-<8-char-random>/firecracker.sock".
const reservedRunDirBudget = 80

//go:embed firecracker.lock
var firecrackerVersionLock string

// Manager owns Firecracker-backed sandbox runtime instances.
type Manager struct {
	cfg                  *config.Config
	mu                   sync.Mutex
	instances            map[string]*instance
	instancesByKey       map[runtimeInstanceKey]string
	provisioning         map[runtimeInstanceKey]chan struct{}
	pendingSpawns        map[runtimeInstanceKey]int
	pendingMemoryMiB     map[runtimeInstanceKey]int
	shuttingDown         bool
	sweepCancel          context.CancelFunc
	sweepDone            chan struct{}
	guestIPs             map[string]string // instanceID -> reserved guest IP
	network              HostNetworkConfigurer
	networkPolicy        HostNetworkPolicyEnforcer
	defaultNetworkPolicy *networkpolicy.CompiledPolicy
	trustedArtifacts     *trustedMicroVMArtifacts
	startCompartment     func(context.Context, string, string, runtimemanager.StartOpts) (string, error)
	executeTool          func(context.Context, string, ToolExecRequest) (ToolExecResponse, error)
	freezeWorkspace      func(context.Context, string) (BackupResponse, error)
	removeInstance       func(context.Context, string) error
	signalInstance       func(string, syscall.Signal) error
	startDurations       []time.Duration
	mountLocks           map[runtimeInstanceKey]*sync.Mutex
	cleanupFailure       error
	evidence             runnerevidence.Sink
	runnerID             string
	workspaceStore       workspacestore.WorkspaceStore
}

// SetWorkspaceStore binds the provider-neutral local workspace authority before
// the Runner starts accepting assignments.
func (m *Manager) SetWorkspaceStore(store workspacestore.WorkspaceStore) error {
	if m == nil || store == nil {
		return fmt.Errorf("SecondBox Firecracker WorkspaceStore is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.instances) != 0 || len(m.provisioning) != 0 || m.sweepCancel != nil {
		return fmt.Errorf("SecondBox Firecracker WorkspaceStore must be bound before startup")
	}
	m.workspaceStore = store
	return nil
}

type runtimeInstanceKey struct {
	sandboxID     string
	compartmentID string
}

type instance struct {
	id                     string
	sandboxID              string
	sandboxGeneration      uint64
	compartmentID          string
	dir                    string
	logPath                string
	socket                 string
	vsockUDS               string
	guestControlPort       uint32
	guestProtocolPort      uint32
	guestProtocolSession   *GuestProtocolSession
	guestProtocolCloseOnce sync.Once
	guestProtocolCloseErr  error
	tapName                string
	jailRoot               string
	rootfsPath             string
	rootfsImagePath        string
	workspacePath          string
	workspaceAttachment    workspacestore.ComputeAttachment
	sharedImagePath        string
	guestIP                string
	startupFingerprint     string
	cmd                    *exec.Cmd
	startedAt              time.Time
	lastUsedAt             time.Time
	inflight               int
	warmToolVM             bool
	draining               bool
	reaping                bool
	done                   chan struct{} // closed after the VM exits and cleanup runs
	doneOnce               sync.Once
	cleanupErr             error
	requestID              string
	operationID            string
	leaseID                string
	assignmentID           string
	memoryMiB              int
	ready                  bool
	explicitStop           bool
	baselineOOMKills       *uint64
	terminationEvidenceErr error
	terminalObserver       func(context.Context, InstanceTerminalObservation) error
	// jailedProcess is true when cmd is the runner-owned jailer supervisor.
	// The supervisor adopts and reaps the jailer's orphaned Firecracker child.
	jailedProcess bool
}

type firecrackerLaunch struct {
	executable  string
	args        []string
	environment []string
	config      firecrackerConfig
	configPath  string
	socketPath  string
	vsockUDS    string
	jailRoot    string
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
	if err := requireExecutable("firecracker", cfg.FirecrackerPath); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.NetworkPolicyNFTPath) != "" {
		if err := requireExecutable("nft", cfg.NetworkPolicyNFTPath); err != nil {
			return nil, err
		}
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
	for _, dir := range []string{cfg.MicroVMRunDir, cfg.MicroVMLogDir} {
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
		evidence:         runnerevidence.SlogSink{},
		signalInstance:   signalFirecrackerByID,
	}
	if strings.TrimSpace(cfg.NetworkPolicyNFTPath) != "" {
		m.networkPolicy = &NFTablesNetworkPolicyEnforcer{
			nftPath:     cfg.NetworkPolicyNFTPath,
			dnsListen:   bridgeAddress(cfg.MicroVMBridgeCIDR),
			dnsUpstream: cfg.NetworkPolicyDNSUpstream,
		}
		m.defaultNetworkPolicy, err = networkpolicy.Compile(
			networkpolicy.Policy{Mode: networkpolicy.ModeDenyAll},
			m.networkPolicyCompileOptions(),
		)
		if err != nil {
			return nil, fmt.Errorf("compile default-deny host network policy: %w", err)
		}
	}
	return m, nil
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	var startupErr error
	if err := m.sweepStartupOrphans(ctx); err != nil {
		startupErr = errors.Join(startupErr, fmt.Errorf("startup orphan microVM sweep: %w", err))
	}
	if deleted, err := m.pruneLogs(time.Now().UTC(), 7*24*time.Hour); err != nil {
		startupErr = errors.Join(startupErr, fmt.Errorf("prune stale microVM logs: %w", err))
	} else if deleted > 0 {
		slog.Info("pruned stale microVM logs", "count", deleted)
	}
	if startupErr != nil {
		return startupErr
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
	var shutdownErr error
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
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("wait for provisioning tool VMs: %w", waitCtx.Err()))
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
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("wait for %d in-flight tool VM operations: %w", inflight, inflightCtx.Err()))
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
	teardownErrors := make(chan error, len(victims))
	for _, inst := range victims {
		wg.Add(1)
		go func(inst *instance) {
			defer wg.Done()
			if err := m.teardownManagedVMContext(ctx, inst); err != nil {
				teardownErrors <- err
			}
		}(inst)
	}
	go func() {
		wg.Wait()
		close(teardownErrors)
		close(doneTeardown)
	}()
	select {
	case <-doneTeardown:
	case <-ctx.Done():
		return errors.Join(shutdownErr, ctx.Err())
	}
	for err := range teardownErrors {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if m.networkPolicy != nil {
		shutdownErr = errors.Join(shutdownErr, m.networkPolicy.Close())
	}
	return shutdownErr
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
				id:            instanceID,
				dir:           runDir,
				jailRoot:      jailRoot,
				tapName:       tapNameForInstance(m.cfg.MicroVMTapPrefix, instanceID),
				jailedProcess: true,
				done:          make(chan struct{}),
			}
			orphanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := m.stopInstance(orphanCtx, inst, true)
			cancel()
			if err != nil {
				joined = errors.Join(joined, fmt.Errorf("stop startup orphan %s: %w", instanceID, err))
				continue
			}
		}
		if !running {
			if err := m.cleanupNetworkChecked(
				ctx,
				instanceID,
				tapNameForInstance(m.cfg.MicroVMTapPrefix, instanceID),
			); err != nil {
				joined = errors.Join(joined, fmt.Errorf("remove startup orphan network %s: %w", instanceID, err))
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
		return fmt.Errorf("firecracker %d.%d.%d does not match pinned version %d.%d.%d; rebuild snapshots/artifacts with the pinned VMM or update internal/firecracker/firecracker.lock deliberately", maj, minr, pat, wantMaj, wantMin, wantPatch)
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
func (m *Manager) createAndStart(ctx context.Context, sandboxID string, opts runtimemanager.StartOpts) (string, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	compartmentID := normalizeRuntimeCompartmentID(opts.CompartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	opts.CompartmentID = compartmentID
	key := runtimeInstanceKey{sandboxID: sandboxID, compartmentID: compartmentID}
	memoryMiB := m.requestedMemoryMiB(opts)
	m.mu.Lock()
	if err := m.reserveCompartmentSpawnLocked(key, memoryMiB); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	releasePendingLocked := sync.OnceFunc(func() {
		m.releaseCompartmentSpawnLocked(key, memoryMiB)
	})
	releasePending := func() {
		m.mu.Lock()
		releasePendingLocked()
		m.mu.Unlock()
	}
	defer releasePending()
	// Single-guest-IP mode admits only one compartment per sandbox, so a Firecracker
	// process for this sandbox that the manager is not tracking is a leaked orphan
	// that still holds the shared guest IP. Reclaim it before this cold boot
	// reserves the same IP, which would otherwise put two VMs on one address. With
	// a bridge CIDR each VM gets a distinct IP and legitimate sibling/in-flight
	// spawns must not be touched.
	if m.cfg != nil && strings.TrimSpace(m.cfg.MicroVMBridgeCIDR) == "" {
		if err := m.cleanupUntrackedSandboxOrphans(ctx, sandboxID); err != nil {
			return "", err
		}
	}
	return m.startCompartmentInstance(ctx, sandboxID, compartmentID, opts, releasePendingLocked)
}

// ExecuteEphemeralTool starts a fresh Firecracker tool VM for one dangerous
// operation, executes the request, then tears the VM down. Every sandbox hand
// gets its own ephemeral VM; there is no warm-VM reuse.
func (m *Manager) cleanupUntrackedSandboxOrphans(ctx context.Context, sandboxID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	orphanIDs, err := firecrackerInstanceIDsForSandboxFunc(sandboxID)
	if err != nil {
		return fmt.Errorf("enumerate Firecracker processes for sandbox %q orphan cleanup: %w", sandboxID, err)
	}
	if len(orphanIDs) == 0 {
		return nil
	}
	var cleanupErr error
	for _, id := range m.untrackedInstanceIDs(orphanIDs) {
		slog.Warn("reclaiming orphaned firecracker process not tracked by any runtime instance", "sandbox", sandboxID, "instance", id)
		inst := &instance{
			id:            id,
			sandboxID:     sandboxID,
			dir:           filepath.Join(m.cfg.MicroVMRunDir, id),
			jailRoot:      m.jailerRoot(id),
			tapName:       tapNameForInstance(m.cfg.MicroVMTapPrefix, id),
			jailedProcess: true,
			done:          make(chan struct{}),
		}
		if err := m.stopInstance(ctx, inst, true); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop orphaned Firecracker process %q: %w", id, err))
		}
	}
	return cleanupErr
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

func (m *Manager) startCompartmentInstance(ctx context.Context, sandboxID, compartmentID string, opts runtimemanager.StartOpts, onRegisteredLocked func()) (id string, err error) {
	started := time.Now()
	defer func() {
		if err == nil && strings.TrimSpace(id) != "" {
			m.recordStartDuration(time.Since(started))
		}
	}()
	if m.startCompartment != nil {
		return m.startCompartment(ctx, sandboxID, compartmentID, opts)
	}
	return m.createAndStartCold(ctx, sandboxID, compartmentID, opts, onRegisteredLocked)
}

func (m *Manager) createAndStartCold(ctx context.Context, sandboxID, compartmentID string, opts runtimemanager.StartOpts, onRegisteredLocked func()) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if opts.WorkspaceAttachment == nil {
		return "", fmt.Errorf("SecondBox Firecracker Workspace attachment is required")
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	opts.CompartmentID = compartmentID
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelSetup()

	id, err := newInstanceID(sandboxID, compartmentID)
	if err != nil {
		return "", err
	}
	timer := newColdStartStageTimer("sandbox", sandboxID, "compartment", compartmentID, "instance", id)
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
			SandboxID:  sandboxID,
			InstanceID: id,
			TapName:    tapName,
			GuestIP:    guestIP,
			BridgeName: m.cfg.MicroVMBridgeName,
			BridgeCIDR: m.cfg.MicroVMBridgeCIDR,
			OwnerUID:   m.tapOwnerUID(),
		}); err != nil {
			return "", fmt.Errorf("configure microVM tap: %w", err)
		}
		policy := opts.NetworkPolicy
		if policy == nil {
			policy = m.defaultNetworkPolicy
		}
		if m.networkPolicy == nil {
			return "", m.joinInstanceNetworkCleanup(
				setupCtx,
				id,
				tapName,
				fmt.Errorf("microVM host network policy enforcement is required but unavailable"),
			)
		}
		if policy == nil {
			return "", m.joinInstanceNetworkCleanup(
				setupCtx,
				id,
				tapName,
				fmt.Errorf("microVM default-deny network policy is unavailable"),
			)
		}
		if err := m.networkPolicy.Install(setupCtx, PolicyNetworkConfig{
			InstanceID: id,
			TapName:    tapName,
			GuestIP:    guestIP,
			DNSAddress: bridgeAddress(m.cfg.MicroVMBridgeCIDR),
			Policy:     policy,
			OnFailure: func(policyErr error) {
				m.handleNetworkPolicyFailure(id, policyErr)
			},
		}); err != nil {
			return "", m.joinInstanceNetworkCleanup(
				setupCtx,
				id,
				tapName,
				fmt.Errorf("install microVM host network policy: %w", err),
			)
		}
	}
	timer.mark("network_ready", "tap", tapName, "networkRequired", m.networkRequired(opts))
	if opts.StartupProgress != nil {
		if progressErr := opts.StartupProgress(runtimemanager.StartupStageNetworkReady); progressErr != nil {
			return "", m.joinInstanceNetworkCleanup(
				setupCtx,
				id,
				tapName,
				fmt.Errorf("report network-ready startup stage: %w", progressErr),
			)
		}
	}
	dir := filepath.Join(m.cfg.MicroVMRunDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", m.joinInstanceNetworkCleanup(ctx, id, tapName, fmt.Errorf("create instance dir: %w", err))
	}
	// The per-instance dir holds disposable launch state (the rootfs copy, the
	// firecracker config, the identity file). Reclaim it on any early failure;
	// ownership transfers to the running instance once it is registered below.
	cleanupDir := true
	defer func() {
		if cleanupDir {
			if err := os.RemoveAll(dir); err != nil {
				m.recordCleanupFailure(fmt.Errorf("remove failed launch directory %q: %w", dir, err))
			}
		}
	}()
	logPath := filepath.Join(m.cfg.MicroVMLogDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", m.joinInstanceNetworkCleanup(ctx, id, tapName, fmt.Errorf("open microVM log: %w", err))
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			m.recordCleanupFailure(fmt.Errorf("close microVM log %q: %w", logPath, err))
		}
	}()
	timer.mark("instance_files_ready", "dir", dir, "log", logPath)

	image, err := m.microVMImageForStart(opts)
	if err != nil {
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, err)
	}
	launchImage, err := m.prepareLaunchImage(dir, image)
	if err != nil {
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, err)
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
	workspaceImage := opts.WorkspaceAttachment.Image()
	if workspaceImage == nil || opts.WorkspaceAttachment.Generation() != opts.SandboxGeneration {
		return "", m.joinInstanceNetworkCleanup(
			setupCtx,
			id,
			tapName,
			fmt.Errorf("resolved Workspace attachment generation is stale"),
		)
	}
	info, statErr := workspaceImage.Stat()
	if statErr != nil {
		return "", m.joinInstanceNetworkCleanup(
			setupCtx,
			id,
			tapName,
			fmt.Errorf("inspect resolved Workspace attachment: %w", statErr),
		)
	}
	if !info.Mode().IsRegular() || info.Size() != int64(workspaceSizeMiB)*1024*1024 {
		return "", m.joinInstanceNetworkCleanup(
			setupCtx,
			id,
			tapName,
			fmt.Errorf("resolved Workspace attachment capacity is invalid"),
		)
	}
	workspacePath := workspaceImage.Name()
	timer.mark("workspace_ready", "workspace", workspacePath)
	// WorkspaceStore owns the image and its lifetime. Launch teardown removes
	// only the jail hard link and closes the opaque attachment after Firecracker
	// and every host-side user have stopped.

	if err := m.writeIdentityFile(dir, id, sandboxID, opts); err != nil {
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, err)
	}
	startupFingerprint, err := m.startupFingerprint(sandboxID, compartmentID, opts)
	if err != nil {
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, fmt.Errorf("build startup fingerprint: %w", err))
	}
	timer.mark("launch_config_ready")

	launch, launchErr := m.prepareLaunchWithPolicy(setupCtx, id, dir, launchImage.KernelPath, launchImage.RootfsPath, workspacePath, sharedImagePath, tapName, guestIP, opts.SandboxPolicy)
	if launchErr != nil {
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, launchErr)
	}
	guestBootArgs, guestBootArgsErr := m.guestProtocolBootArgs(compartmentID, sandboxID, opts)
	if guestBootArgsErr != nil {
		m.cleanupLaunch(launch)
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, guestBootArgsErr)
	}
	launch.config.BootSource.BootArgs = strings.TrimSpace(launch.config.BootSource.BootArgs + " " + guestBootArgs)
	timer.mark("launch_prepared", "jailRoot", launch.jailRoot)
	data, marshalErr := json.MarshalIndent(launch.config, "", "  ")
	if marshalErr != nil {
		m.cleanupLaunch(launch)
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, fmt.Errorf("marshal firecracker config: %w", marshalErr))
	}
	if writeErr := os.WriteFile(launch.configPath, data, 0o600); writeErr != nil {
		m.cleanupLaunch(launch)
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, fmt.Errorf("write firecracker config: %w", writeErr))
	}
	if launch.jailRoot != "" {
		if chownErr := chownIfDifferent(launch.configPath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); chownErr != nil {
			m.cleanupLaunch(launch)
			return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, fmt.Errorf("chown jailed firecracker config: %w", chownErr))
		}
	}
	cmd := exec.Command(launch.executable, launch.args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if len(launch.environment) != 0 {
		cmd.Env = append(os.Environ(), launch.environment...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if startErr := cmd.Start(); startErr != nil {
		m.cleanupLaunch(launch)
		return "", m.joinInstanceNetworkCleanup(setupCtx, id, tapName, fmt.Errorf("start firecracker: %w", startErr))
	}
	socketPath, vsockPath, jailRoot := launch.socketPath, launch.vsockUDS, launch.jailRoot
	jailedProcess := launch.jailRoot != ""
	timer.mark("firecracker_process_started", "pid", cmd.Process.Pid)

	inst := &instance{
		id:                  id,
		sandboxID:           sandboxID,
		sandboxGeneration:   opts.SandboxGeneration,
		compartmentID:       compartmentID,
		dir:                 dir,
		logPath:             logPath,
		socket:              socketPath,
		vsockUDS:            vsockPath,
		guestControlPort:    m.cfg.MicroVMGuestControlVsockPort,
		guestProtocolPort:   m.cfg.MicroVMGuestProtocolVsockPort,
		tapName:             tapName,
		jailRoot:            jailRoot,
		rootfsPath:          launchImage.RootfsPath,
		rootfsImagePath:     image.RootfsPath,
		workspacePath:       workspacePath,
		workspaceAttachment: opts.WorkspaceAttachment,
		sharedImagePath:     launchImage.SharedImagePath,
		guestIP:             guestIP,
		startupFingerprint:  startupFingerprint,
		cmd:                 cmd,
		startedAt:           time.Now().UTC(),
		done:                make(chan struct{}),
		jailedProcess:       jailedProcess,
		requestID:           opts.RequestID,
		operationID:         opts.OperationID,
		leaseID:             opts.LeaseID,
		assignmentID:        opts.AssignmentID,
		memoryMiB:           m.requestedMemoryMiB(opts),
	}
	m.registerStartingInstance(inst, onRegisteredLocked)
	// Start the reaper before any stopInstance call so the process is always
	// waited on (no zombie) and cleanup runs exactly once.
	go m.reap(inst)
	timer.mark("instance_registered")
	if opts.StartupProgress != nil {
		if progressErr := opts.StartupProgress(runtimemanager.StartupStageComputeStarted); progressErr != nil {
			cleanupErr := m.stopInstance(setupCtx, inst, true)
			return "", errors.Join(
				fmt.Errorf("report compute-started startup stage: %w", progressErr),
				cleanupErr,
			)
		}
	}
	negotiationCtx, cancelNegotiation := context.WithTimeout(
		setupCtx,
		controlPlaneReadyTimeout,
	)
	err = m.negotiateInstanceGuest(negotiationCtx, inst, opts)
	cancelNegotiation()
	if err != nil {
		diagnostics := inst.logTailDiagnostics(120)
		cleanupErr := m.stopInstance(setupCtx, inst, true)
		return "", errors.Join(
			fmt.Errorf("negotiate guest protocol: %w%s", err, diagnostics),
			cleanupErr,
		)
	}
	timer.mark("guest_protocol_negotiated")
	if opts.StartupProgress != nil {
		if progressErr := opts.StartupProgress(runtimemanager.StartupStageGuestNegotiated); progressErr != nil {
			cleanupErr := m.stopInstance(setupCtx, inst, true)
			return "", errors.Join(
				fmt.Errorf("report guest-negotiated startup stage: %w", progressErr),
				cleanupErr,
			)
		}
	}
	if err := m.deliverStartupSecrets(setupCtx, inst, sandboxID, opts, timer); err != nil {
		cleanupErr := m.stopInstance(setupCtx, inst, true)
		return "", errors.Join(fmt.Errorf("deliver runtime startup secrets: %w", err), cleanupErr)
	}
	timer.mark("microvm_ready")
	releaseIP = false // ownership transfers to the running instance
	cleanupDir = false
	slog.Info("started firecracker microVM", "sandbox", sandboxID, "compartment", compartmentID, "instance", id, "elapsedMs", timer.elapsedMs(), "log", logPath)
	return id, nil
}

func (m *Manager) handleNetworkPolicyFailure(instanceID string, policyErr error) {
	if inst := m.lookup(instanceID); inst != nil {
		if err := m.evidenceSink().Emit(
			context.Background(),
			m.instanceEvidenceRecord(
				inst,
				runnerevidence.EventNetworkFailure,
				"failed",
				"enforcement_unavailable",
			),
		); err != nil {
			m.recordCleanupFailure(errors.Join(policyErr, err))
		}
	}
	m.recordCleanupFailure(fmt.Errorf("host network policy failed for %s: %w", instanceID, policyErr))
	inst := m.lookup(instanceID)
	if inst == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.stopInstance(cleanupCtx, inst, true); err != nil {
		m.recordCleanupFailure(errors.Join(policyErr, err))
	}
}

func (m *Manager) reap(inst *instance) {
	var err error
	if inst.cmd != nil {
		err = inst.cmd.Wait()
	}
	if err != nil {
		if inst.jailedProcess {
			slog.Warn("firecracker jailer supervisor exited with error", "sandbox", inst.sandboxID, "instance", inst.id, "error", err)
		} else {
			slog.Warn("firecracker microVM exited", "sandbox", inst.sandboxID, "instance", inst.id, "error", err)
		}
	}
	m.finishInstance(inst)
}

func (m *Manager) finishInstance(inst *instance) {
	if inst == nil {
		return
	}
	inst.doneOnce.Do(func() {
		if err := m.observeNaturalTermination(inst); err != nil {
			inst.cleanupErr = errors.Join(inst.cleanupErr, fmt.Errorf("observe natural instance termination: %w", err))
			m.recordCleanupFailure(inst.cleanupErr)
		}
		if err := inst.closeGuestProtocol(); err != nil {
			inst.cleanupErr = errors.Join(inst.cleanupErr, fmt.Errorf("close guest protocol: %w", err))
			m.recordCleanupFailure(inst.cleanupErr)
		}
		if inst.workspaceAttachment != nil {
			if err := inst.workspaceAttachment.Close(); err != nil {
				inst.cleanupErr = errors.Join(
					inst.cleanupErr,
					fmt.Errorf("close Workspace attachment: %w", err),
				)
				m.recordCleanupFailure(inst.cleanupErr)
			}
			inst.workspaceAttachment = nil
		}
		m.mu.Lock()
		m.removeInstanceLocked(inst)
		m.mu.Unlock()
		// Release the guest identity only after the jailed TAP and state are gone.
		// If cleanup fails the jailed process may still claim this IP/MAC, so retain the
		// reservation fail-closed rather than let a concurrent start recycle it into
		// an ownership conflict.
		if err := m.cleanupNetworkChecked(context.Background(), inst.id, inst.tapName); err != nil {
			inst.cleanupErr = errors.Join(inst.cleanupErr, err)
			m.recordCleanupFailure(inst.cleanupErr)
		} else {
			m.releaseGuestIP(inst.id)
		}
		outcome := "completed"
		terminalKind := "removed"
		if inst.cleanupErr != nil {
			outcome = "failed"
			terminalKind = "cleanup_failed"
		}
		if err := m.evidenceSink().Emit(
			context.Background(),
			m.instanceEvidenceRecord(
				inst,
				runnerevidence.EventTeardownTerminal,
				outcome,
				terminalKind,
			),
		); err != nil {
			inst.cleanupErr = errors.Join(inst.cleanupErr, err)
			m.recordCleanupFailure(inst.cleanupErr)
		}
		close(inst.done)
	})
}

func (m *Manager) SetRunnerEvidenceSink(sink runnerevidence.Sink, runnerID string) {
	if sink == nil {
		return
	}
	m.evidence = sink
	m.runnerID = runnerID
}

func (m *Manager) evidenceSink() runnerevidence.Sink {
	if m.evidence == nil {
		return runnerevidence.SlogSink{}
	}
	return m.evidence
}

func (m *Manager) instanceEvidenceRecord(
	inst *instance,
	event runnerevidence.Event,
	outcome string,
	terminalKind string,
) runnerevidence.Record {
	record := runnerevidence.NewRecord(event, outcome, terminalKind, time.Now().UTC())
	record.RunnerID = m.runnerID
	if inst == nil {
		return record
	}
	record.RequestID = inst.requestID
	record.OperationID = inst.operationID
	record.LeaseID = inst.leaseID
	record.AssignmentID = inst.assignmentID
	record.SandboxID = inst.sandboxID
	record.InstanceID = inst.compartmentID
	record.SandboxGeneration = inst.sandboxGeneration
	return record
}

var firecrackerProcessRunningFunc = firecrackerProcessRunning

// firecrackerInstanceIDsForSandboxFunc is overridable in tests; production uses the
// real /proc-based enumeration.
var firecrackerInstanceIDsForSandboxFunc = firecrackerInstanceIDsForSandbox

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

func firecrackerInstanceIDsForSandbox(sandboxID string) ([]string, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, nil
	}
	prefix := instancePrefix + "-" + instanceSandboxIDSegment(sandboxID) + "-"
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

func (m *Manager) StopAndRemoveCompartment(ctx context.Context, sandboxID, compartmentID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	m.mu.Lock()
	var instances []*instance
	for _, inst := range m.instances {
		if inst == nil {
			continue
		}
		if inst.sandboxID == sandboxID && inst.compartmentID == compartmentID {
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

// quiesceUnavailableGrace bounds termination when the Workspace could not be
// frozen. The guest may still hold dirty pages, so the VMM is asked to exit
// first and only escalated afterwards.
const quiesceUnavailableGrace = 5 * time.Second

// quiescedGrace is a backstop only. A frozen Workspace is already consistent on
// disk and the VMM receives SIGKILL directly, so this timer should never fire.
const quiescedGrace = 500 * time.Millisecond

// quiesceWorkspace freezes the guest Workspace filesystem so the VMM can be
// terminated without waiting for a graceful guest shutdown. It reports whether
// the filesystem is known to be consistent on disk. A guest that cannot be
// reached is not an error here: the caller falls back to the slower signal
// escalation that does not assume a quiesced filesystem.
func (m *Manager) quiesceWorkspace(ctx context.Context, inst *instance) bool {
	if m == nil || inst == nil {
		return false
	}
	freeze := m.FreezeWorkspace
	if m.freezeWorkspace != nil {
		freeze = m.freezeWorkspace
	}
	freezeCtx, cancel := context.WithTimeout(ctx, quiescedGrace)
	defer cancel()
	if _, err := freeze(freezeCtx, inst.id); err != nil {
		slog.Warn(
			"microVM workspace freeze before termination failed",
			"instance", inst.id, "error", err,
		)
		return false
	}
	return true
}

func (m *Manager) terminationGrace(quiesced bool) time.Duration {
	if quiesced {
		return quiescedGrace
	}
	return quiesceUnavailableGrace
}

func (m *Manager) stopInstance(ctx context.Context, inst *instance, removeFiles bool) error {
	if inst == nil {
		return nil
	}
	m.mu.Lock()
	inst.explicitStop = true
	m.mu.Unlock()
	var stopErr error
	// Quiesce the Workspace filesystem before terminating the VMM. A frozen
	// filesystem has flushed its dirty pages and is consistent on disk, so the
	// VMM can be terminated immediately. Without this the runner sends SIGTERM
	// and waits out the escalation grace period on every stop, because the VMM
	// does not exit on SIGTERM; that grace period dominated stop latency.
	quiesced := m.quiesceWorkspace(ctx, inst)
	if err := inst.closeGuestProtocol(); err != nil {
		stopErr = errors.Join(stopErr, fmt.Errorf("close guest protocol: %w", err))
	}
	if inst.jailedProcess {
		signalInstance := signalFirecrackerByID
		if m.signalInstance != nil {
			signalInstance = m.signalInstance
		}
		terminate := syscall.SIGTERM
		if quiesced {
			terminate = syscall.SIGKILL
		}
		if err := signalInstance(inst.id, terminate); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("signal jailed Firecracker %q: %w", inst.id, err))
		}
		kill := time.AfterFunc(m.terminationGrace(quiesced), func() {
			_ = signalInstance(inst.id, syscall.SIGKILL)
		})
		select {
		case <-inst.done:
			kill.Stop()
		case <-ctx.Done():
			// Leave the kill timer to escalate. The jailer supervisor remains
			// responsible for reaping Firecracker when it exits.
			return ctx.Err()
		}
	} else {
		if inst.cmd == nil || inst.cmd.Process == nil {
			m.finishInstance(inst)
		} else {
			terminate := syscall.SIGTERM
			if quiesced {
				terminate = syscall.SIGKILL
			}
			if err := syscall.Kill(-inst.cmd.Process.Pid, terminate); err != nil && !errors.Is(err, syscall.ESRCH) {
				stopErr = errors.Join(stopErr, fmt.Errorf("signal Firecracker process group %q: %w", inst.id, err))
			}
			// Escalate to SIGKILL after a grace period. The reaper (started at create)
			// observes exit, runs cleanup exactly once, and signals via inst.done.
			kill := time.AfterFunc(m.terminationGrace(quiesced), func() {
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
	stopErr = errors.Join(stopErr, inst.cleanupErr)
	if removeFiles {
		if err := os.RemoveAll(inst.dir); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("remove instance run directory %q: %w", inst.dir, err))
		}
		if strings.TrimSpace(inst.logPath) != "" {
			if err := os.Remove(inst.logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				stopErr = errors.Join(stopErr, fmt.Errorf("remove instance log %q: %w", inst.logPath, err))
			}
		}
		if inst.jailRoot != "" {
			if err := os.RemoveAll(filepath.Dir(inst.jailRoot)); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("remove instance jail directory %q: %w", filepath.Dir(inst.jailRoot), err))
			}
		}
	}
	return stopErr
}

func (inst *instance) closeGuestProtocol() error {
	if inst == nil {
		return nil
	}
	inst.guestProtocolCloseOnce.Do(func() {
		if inst.guestProtocolSession != nil {
			inst.guestProtocolCloseErr = inst.guestProtocolSession.Close()
		}
	})
	return inst.guestProtocolCloseErr
}

func (m *Manager) recordCleanupFailure(err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	m.cleanupFailure = errors.Join(m.cleanupFailure, err)
	m.mu.Unlock()
}

func (m *Manager) CleanupHealth() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupFailure
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
	if inst.jailedProcess {
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

func (m *Manager) requestGuestShutdown(ctx context.Context, instanceID string) error {
	inst := m.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	return inst.apiClient(5 * time.Second).SendCtrlAltDel(ctx)
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
	inst.sandboxID = strings.TrimSpace(inst.sandboxID)
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
		key := runtimeInstanceKey{sandboxID: strings.TrimSpace(inst.sandboxID), compartmentID: normalizeRuntimeCompartmentID(inst.compartmentID)}
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
	if compartmentID == "default" {
		return fmt.Errorf("default is not a valid runtime compartment")
	}
	return nil
}

func (i *instance) controlClient(timeout time.Duration) ControlClient {
	return ControlClient{UDSPath: i.vsockUDS, Port: i.guestControlPort, Timeout: timeout}
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
