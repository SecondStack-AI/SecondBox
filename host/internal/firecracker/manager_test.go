package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"secondstack/sandbox-host/internal/config"
	"secondstack/sandbox-host/internal/runtime"
	"secondstack/sandbox-host/internal/runtimecontext"
)

type recordingHostNetworkConfigurer struct {
	tap TapConfig
}

func (r *recordingHostNetworkConfigurer) ConfigureTap(_ context.Context, cfg TapConfig) error {
	r.tap = cfg
	return nil
}

func (r *recordingHostNetworkConfigurer) RemoveTap(context.Context, string) error {
	return nil
}

type blockingHostNetworkConfigurer struct {
	removeStarted chan struct{}
	allowRemove   chan struct{}
}

func (b *blockingHostNetworkConfigurer) ConfigureTap(context.Context, TapConfig) error {
	return nil
}

func (b *blockingHostNetworkConfigurer) RemoveTap(context.Context, string) error {
	close(b.removeStarted)
	<-b.allowRemove
	return nil
}

type failingHostNetworkConfigurer struct {
	removeCalls int
}

func (f *failingHostNetworkConfigurer) ConfigureTap(context.Context, TapConfig) error {
	return nil
}

func (f *failingHostNetworkConfigurer) RemoveTap(context.Context, string) error {
	f.removeCalls++
	return fmt.Errorf("simulated launcher tap removal failure")
}

func TestNewInstanceIDIncludesCompartmentSegment(t *testing.T) {
	id, err := newInstanceID("agent-1", "cmp_abcdef1234567890")
	if err != nil {
		t.Fatalf("new instance id: %v", err)
	}
	if !strings.HasPrefix(id, "fc-agent-1-cmp-abcdef123456-") {
		t.Fatalf("instance id %q does not include sanitized compartment segment", id)
	}
	emptyID, err := newInstanceID("agent-1", "")
	if err != nil {
		t.Fatalf("new empty-compartment instance id: %v", err)
	}
	if !strings.HasPrefix(emptyID, "fc-agent-1-compartment-") {
		t.Fatalf("empty-compartment instance id %q does not include fallback segment", emptyID)
	}

	productionID, err := newInstanceID(
		"3228ade9370612d8e6b01dff6e3f2cef",
		"cmp_12c176d7ad7e8c50e589c75fbfce359f",
	)
	if err != nil {
		t.Fatalf("new production-length instance id: %v", err)
	}
	for _, socketName := range []string{firecrackerSockName, vsockUDSName} {
		socketPath := filepath.Join(
			"/agent-sandbox/state/jailer",
			"firecracker",
			productionID,
			"root",
			socketName,
		)
		if len(socketPath) >= maxUnixSocketPathLen {
			t.Fatalf("default jailed socket path is %d bytes: %q", len(socketPath), socketPath)
		}
	}
}

func TestBuildPrivilegedLaunchRequestMatchesLauncherValidation(t *testing.T) {
	dir := t.TempDir()
	runRoot := filepath.Join(dir, "run")
	workspaceRoot := filepath.Join(dir, "workspaces")
	artifactRoot := filepath.Join(dir, "artifacts")
	instanceID := "fc-agent-1-cmp-a-123"
	agentID := "agent-1"
	compartmentID := "cmp-a"
	rootfsPath := filepath.Join(runRoot, instanceID, rootfsName)
	workspacePath := filepath.Join(workspaceRoot, agentID, compartmentID+"."+workspaceName)
	rootfsImage := filepath.Join(artifactRoot, "rootfs.ext4")
	sharedImage := filepath.Join(artifactRoot, "shared.img")
	for _, path := range []string{rootfsPath, workspacePath, rootfsImage, sharedImage} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tapName := tapNameForInstance("agfc", instanceID)
	req := buildPrivilegedLaunchRequest(instanceID, agentID, compartmentID,
		microVMImageSelection{RootfsPath: rootfsPath, SharedImagePath: filepath.Join(runRoot, instanceID, sharedImageName)},
		microVMImageSelection{RootfsPath: rootfsImage, SharedImagePath: sharedImage},
		workspacePath, tapName, "172.30.0.2",
		&runtimemanager.SandboxRuntimePolicy{VCPUs: 1, MemoryMiB: 512, WorkspaceSizeMiB: 1024, ProcessLimit: 32, WorkspaceWritable: true, SharedReadOnly: true})
	server := &PrivilegedLauncherServer{cfg: PrivilegedLauncherConfig{
		RunRoot:          runRoot,
		WorkspaceRoot:    workspaceRoot,
		ArtifactRoot:     artifactRoot,
		TapPrefix:        "agfc",
		BridgeCIDR:       "172.30.0.0/24",
		VCPUs:            2,
		MemoryMiB:        1024,
		WorkspaceSizeMiB: 2048,
	}}
	if err := server.validateLaunchRequest(req); err != nil {
		t.Fatalf("validateLaunchRequest: %v", err)
	}
	if req.SharedImage != sharedImage {
		t.Fatalf("shared image = %q, want signed source artifact %q", req.SharedImage, sharedImage)
	}
}

func TestBuildPrivilegedLaunchRequestOmitsPolicyDisabledSharedImage(t *testing.T) {
	req := buildPrivilegedLaunchRequest("fc-agent-1-cmp-a-123", "agent-1", "cmp-a",
		microVMImageSelection{RootfsPath: "/run/rootfs.ext4", SharedImagePath: "/run/shared.img"},
		microVMImageSelection{RootfsPath: "/artifacts/rootfs.ext4", SharedImagePath: "/artifacts/shared.img"},
		"/workspaces/agent-1/cmp-a.workspace.ext4", "", "",
		&runtimemanager.SandboxRuntimePolicy{VCPUs: 1, MemoryMiB: 512, WorkspaceSizeMiB: 1024, ProcessLimit: 32, WorkspaceWritable: true})
	if req.SharedImage != "" {
		t.Fatalf("shared image = %q, want omitted by sandbox policy", req.SharedImage)
	}
}

func TestLauncherModeReapAllowsNilCommand(t *testing.T) {
	done := make(chan struct{})
	close(done)
	m := &Manager{}
	m.reap(&instance{id: "fc-agent-1-abc", launcherOnly: true, done: done})
}

func TestLauncherModeStopUsesLauncher(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan privilegedLauncherRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		if decodeErr := json.NewDecoder(conn).Decode(&req); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		requestSeen <- req
		serverErr <- json.NewEncoder(conn).Encode(privilegedLauncherResponse{OK: true})
	}()

	m := &Manager{
		launcher:       newPrivilegedLauncherClient(socketPath),
		instances:      map[string]*instance{},
		instancesByKey: map[runtimeInstanceKey]string{},
		guestIPs:       map[string]string{},
	}
	inst := &instance{id: "fc-agent-1-abc", launcherOnly: true, done: make(chan struct{})}
	if err := m.stopInstance(context.Background(), inst, false); err != nil {
		t.Fatalf("stopInstance: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("launcher server: %v", err)
	}
	req := <-requestSeen
	if req.Op != "stop" || req.ID != inst.id {
		t.Fatalf("launcher request = %#v", req)
	}
}

func TestLauncherModeLivenessUsesPrivilegedLauncherNamespace(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan privilegedLauncherRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		if decodeErr := json.NewDecoder(conn).Decode(&req); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		requestSeen <- req
		serverErr <- json.NewEncoder(conn).Encode(privilegedLauncherResponse{OK: true, Running: true})
	}()

	m := &Manager{launcher: newPrivilegedLauncherClient(socketPath)}
	inst := &instance{id: "fc-agent-1-abc", launcherOnly: true}
	running, err := m.firecrackerRunning(context.Background(), inst)
	if err != nil {
		t.Fatalf("firecrackerRunning: %v", err)
	}
	if !running {
		t.Fatal("launcher reported running process as stopped")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("launcher server: %v", err)
	}
	req := <-requestSeen
	if req.Op != "running" || req.ID != inst.id {
		t.Fatalf("launcher request = %#v", req)
	}
}

func TestTrustedMicroVMArtifactsDetectsSameSizeRestoredMtimeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	fixedTime := time.Unix(1700000000, 123)
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatalf("set artifact time: %v", err)
	}
	identity, err := trustedMicroVMArtifactIdentityFor(path)
	if err != nil {
		t.Fatalf("capture identity: %v", err)
	}
	artifacts := &trustedMicroVMArtifacts{files: []trustedMicroVMArtifactFile{{
		label:    "rootfs",
		path:     path,
		identity: identity,
	}}}
	if err := os.WriteFile(path, []byte("after!"), 0o600); err != nil {
		t.Fatalf("mutate artifact: %v", err)
	}
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatalf("restore artifact time: %v", err)
	}

	unchanged, err := trustedMicroVMArtifactsUnchanged(artifacts)
	if err != nil {
		t.Fatalf("check artifacts: %v", err)
	}
	if unchanged {
		t.Fatal("same-size mutation with restored mtime was treated as unchanged")
	}
}

func TestStageTrustedLaunchImageFilesUsesStagedPaths(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	shared := filepath.Join(dir, "shared.img")
	for path, data := range map[string]string{
		kernel: "kernel",
		rootfs: "rootfs",
		shared: "shared",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	artifacts := &trustedMicroVMArtifacts{files: []trustedMicroVMArtifactFile{
		{label: "kernel", path: kernel},
		{label: "rootfs", path: rootfs},
		{label: "shared image", path: shared},
	}}
	for i := range artifacts.files {
		identity, err := trustedMicroVMArtifactIdentityFor(artifacts.files[i].path)
		if err != nil {
			t.Fatalf("stat %s: %v", artifacts.files[i].path, err)
		}
		artifacts.files[i].identity = identity
	}

	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := stageTrustedLaunchImageFiles(runDir, microVMImageSelection{
		RuntimeClass:    runtimemanager.RuntimeClassToolExecutor,
		KernelPath:      kernel,
		RootfsPath:      rootfs,
		SharedImagePath: shared,
	}, artifacts)
	if err != nil {
		if strings.Contains(err.Error(), "reflink rootfs") {
			t.Skipf("filesystem does not support rootfs reflinks: %v", err)
		}
		t.Fatalf("stage trusted launch image: %v", err)
	}
	if staged.KernelPath == kernel || staged.RootfsPath == rootfs || staged.SharedImagePath == shared {
		t.Fatalf("staged image still points at source paths: %#v", staged)
	}
	for path, want := range map[string]string{
		staged.KernelPath:      "kernel",
		staged.RootfsPath:      "rootfs",
		staged.SharedImagePath: "shared",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read staged %s: %v", path, err)
		}
		if string(data) != want {
			t.Fatalf("staged %s = %q, want %q", path, data, want)
		}
	}
}

func TestManagerInstanceMapConcurrentAccess(t *testing.T) {
	m := &Manager{}
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			compartmentID := fmt.Sprintf("cmp_%02d", i)
			inst := &instance{
				id:            fmt.Sprintf("fc-agent-concurrent-cmp-%02d", i),
				agentID:       "agent-concurrent",
				compartmentID: compartmentID,
			}
			m.mu.Lock()
			m.addInstanceLocked(inst)
			m.mu.Unlock()
			if got := m.lookup(inst.id); got != inst {
				t.Errorf("lookup %s = %#v, want %#v", inst.id, got, inst)
			}
			m.mu.Lock()
			m.removeInstanceLocked(inst)
			m.mu.Unlock()
		}()
	}
	wg.Wait()
	if len(m.instances) != 0 {
		t.Fatalf("instances left after concurrent remove: %#v", m.instances)
	}
}

func TestManagerExecuteToolUsesInstanceControlClient(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	m := &Manager{instances: map[string]*instance{
		"fc-agent-1-cmp-a": {id: "fc-agent-1-cmp-a", vsockUDS: socketPath},
	}}
	resp, err := m.ExecuteTool(context.Background(), "fc-agent-1-cmp-a", ToolExecRequest{
		Operation:     ToolOpExec,
		Command:       "sh",
		Args:          []string{"-c", "printf ok"},
		Cwd:           ".",
		Env:           map[string]string{"A": "B"},
		TimeoutMillis: 1000,
	})
	if err != nil {
		t.Fatalf("execute tool: %v", err)
	}
	if resp.Stdout != "ok" || resp.ExitCode != 0 {
		t.Fatalf("response = %+v", resp)
	}
	if got := <-handshakes; got != "CONNECT 1024" {
		t.Fatalf("handshake = %q", got)
	}
}

func TestManagerExecuteToolRejectsUnknownInstance(t *testing.T) {
	m := &Manager{}
	if _, err := m.ExecuteTool(context.Background(), "missing", ToolExecRequest{Operation: ToolOpExec, Command: "true"}); err == nil || !strings.Contains(err.Error(), "unknown microVM instance") {
		t.Fatalf("error = %v", err)
	}
}

func newWarmToolTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	shared := filepath.Join(dir, "shared.img")
	for _, path := range []string{rootfs, shared} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		DataDir:                      dir,
		MicroVMRootfsPath:            rootfs,
		MicroVMSharedImagePath:       shared,
		MicroVMToolVMReuseEnabled:    true,
		MicroVMToolVMIdleTTL:         time.Minute,
		MicroVMBridgeCIDR:            "172.30.0.1/24",
		MicroVMWorkspaceDir:          filepath.Join(dir, "workspaces"),
		MicroVMRunDir:                filepath.Join(dir, "run"),
		MicroVMLogDir:                filepath.Join(dir, "logs"),
		MicroVMWorkspaceSizeMiB:      8,
		MicroVMMaxConcurrentPerAgent: 0,
		MicroVMMaxConcurrentGlobal:   0,
		MicroVMMemoryBudgetMiB:       0,
	}
	return &Manager{
		cfg:            cfg,
		instances:      map[string]*instance{},
		instancesByKey: map[runtimeInstanceKey]string{},
		provisioning:   map[runtimeInstanceKey]chan struct{}{},
		guestIPs:       map[string]string{},
	}
}

func TestExecuteToolLeasedReusesSequentialOps(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	var idsMu sync.Mutex
	var ids []string
	m.executeTool = func(_ context.Context, id string, _ ToolExecRequest) (ToolExecResponse, error) {
		idsMu.Lock()
		ids = append(ids, id)
		idsMu.Unlock()
		return ToolExecResponse{ExitCode: 0}, nil
	}
	for i := 0; i < 2; i++ {
		if _, _, err := m.ExecuteToolLeased(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, ToolExecRequest{Operation: ToolOpExec}); err != nil {
			t.Fatalf("leased exec %d: %v", i, err)
		}
	}
	if boots.Load() != 1 {
		t.Fatalf("boots = %d, want 1", boots.Load())
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("exec ids = %#v, want same non-empty id", ids)
	}
	m.mu.Lock()
	inst := m.instances[ids[0]]
	inflight := inst.inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight after releases = %d, want 0", inflight)
	}
}

func TestWithToolVMFileUsesWarmLeaseAndReleases(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	var leasedID string
	err := m.WithToolVMFile(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, func(instanceID string) error {
		leasedID = instanceID
		m.mu.Lock()
		defer m.mu.Unlock()
		if got := m.instances[instanceID].inflight; got != 1 {
			t.Fatalf("inflight during file lease = %d, want 1", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("file lease: %v", err)
	}
	if boots.Load() != 1 || leasedID == "" {
		t.Fatalf("boots=%d leasedID=%q", boots.Load(), leasedID)
	}
	m.mu.Lock()
	inflight := m.instances[leasedID].inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight after file lease = %d, want 0", inflight)
	}
}

func TestWithToolVMFileSharesWarmLeaseWithToolExecutions(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	var ids []string
	m.executeTool = func(_ context.Context, id string, _ ToolExecRequest) (ToolExecResponse, error) {
		ids = append(ids, id)
		return ToolExecResponse{ExitCode: 0}, nil
	}
	opts := runtimemanager.StartOpts{
		Timezone:         "UTC",
		CompartmentID:    "cmp_a",
		ShapeFingerprint: "shape-a",
		RuntimeClass:     runtimemanager.RuntimeClassToolExecutor,
	}
	firstID, _, err := m.ExecuteToolLeased(context.Background(), "agent", "cmp_a", opts, ToolExecRequest{Operation: ToolOpExec})
	if err != nil {
		t.Fatalf("first leased exec: %v", err)
	}
	var fileID string
	if err := m.WithToolVMFile(context.Background(), "agent", "cmp_a", opts, func(instanceID string) error {
		fileID = instanceID
		return nil
	}); err != nil {
		t.Fatalf("file lease: %v", err)
	}
	secondID, _, err := m.ExecuteToolLeased(context.Background(), "agent", "cmp_a", opts, ToolExecRequest{Operation: ToolOpExec})
	if err != nil {
		t.Fatalf("second leased exec: %v", err)
	}
	if boots.Load() != 1 {
		t.Fatalf("boots = %d, want 1", boots.Load())
	}
	if firstID == "" || fileID != firstID || secondID != firstID {
		t.Fatalf("instance ids first=%q file=%q second=%q, want all same", firstID, fileID, secondID)
	}
	if len(ids) != 2 || ids[0] != firstID || ids[1] != firstID {
		t.Fatalf("execute ids = %#v, want both %q", ids, firstID)
	}
	m.mu.Lock()
	inflight := m.instances[firstID].inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight after interleaved operations = %d, want 0", inflight)
	}
}

func TestWithToolVMFileUsesEphemeralFallbackWhenReuseDisabled(t *testing.T) {
	m := newWarmToolTestManager(t)
	m.cfg.MicroVMBridgeCIDR = ""
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	handshakes := make(chan string, 4)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		boots.Add(1)
		if opts.RuntimeClass != runtimemanager.RuntimeClassToolExecutor || !opts.Ephemeral {
			t.Fatalf("start opts = %+v, want ephemeral tool executor", opts)
		}
		id := fmt.Sprintf("fc-%s-%s-ephemeral", agentID, compartmentID)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, vsockUDS: socketPath, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	var leasedID string
	err := m.WithToolVMFile(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, func(instanceID string) error {
		leasedID = instanceID
		return nil
	})
	if err != nil {
		t.Fatalf("ephemeral file transfer: %v", err)
	}
	if boots.Load() != 1 || leasedID != "fc-agent-cmp_a-ephemeral" {
		t.Fatalf("boots=%d leasedID=%q", boots.Load(), leasedID)
	}
	select {
	case <-handshakes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ephemeral workspace freeze")
	}
	m.mu.Lock()
	_, stillPresent := m.instances[leasedID]
	m.mu.Unlock()
	if stillPresent {
		t.Fatalf("ephemeral instance %q still tracked after file transfer", leasedID)
	}
}

func TestEphemeralToolAndFileTransferShareCompartmentMountLock(t *testing.T) {
	m := newWarmToolTestManager(t)
	m.cfg.MicroVMBridgeCIDR = ""
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	firstStartEntered := make(chan struct{})
	releaseFirstStart := make(chan struct{})
	var startCount atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := startCount.Add(1)
		if n == 1 {
			close(firstStartEntered)
			<-releaseFirstStart
		}
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, vsockUDS: socketPath, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}

	toolDone := make(chan error, 1)
	go func() {
		_, _, err := m.ExecuteEphemeralTool(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, ToolExecRequest{
			Operation:     ToolOpExec,
			Command:       "sh",
			Args:          []string{"-c", "printf ok"},
			Cwd:           ".",
			Env:           map[string]string{"A": "B"},
			TimeoutMillis: 1000,
		})
		toolDone <- err
	}()
	select {
	case <-firstStartEntered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first ephemeral tool boot")
	}

	fileDone := make(chan error, 1)
	go func() {
		fileDone <- m.WithToolVMFile(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, func(string) error {
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)
	if got := startCount.Load(); got != 1 {
		t.Fatalf("file transfer started another VM while tool VM held mount lock; starts=%d", got)
	}
	close(releaseFirstStart)
	if err := <-toolDone; err != nil {
		t.Fatalf("ephemeral tool: %v", err)
	}
	if err := <-fileDone; err != nil {
		t.Fatalf("file transfer: %v", err)
	}
	if got := startCount.Load(); got != 2 {
		t.Fatalf("start count = %d, want 2 serialized starts", got)
	}
}

func TestExecuteToolLeasedConcurrentOpsShareOneInstance(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		time.Sleep(20 * time.Millisecond)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	var execs atomic.Int32
	m.executeTool = func(_ context.Context, _ string, _ ToolExecRequest) (ToolExecResponse, error) {
		execs.Add(1)
		time.Sleep(10 * time.Millisecond)
		return ToolExecResponse{ExitCode: 0}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := m.ExecuteToolLeased(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, ToolExecRequest{Operation: ToolOpExec})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("leased exec: %v", err)
		}
	}
	if boots.Load() != 1 {
		t.Fatalf("boots = %d, want 1", boots.Load())
	}
	if execs.Load() != 8 {
		t.Fatalf("execs = %d, want 8", execs.Load())
	}
}

func TestExecuteToolLeasedFailureStillReleases(t *testing.T) {
	m := newWarmToolTestManager(t)
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		id := "fc-agent-cmp-a"
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	m.executeTool = func(context.Context, string, ToolExecRequest) (ToolExecResponse, error) {
		return ToolExecResponse{}, errors.New("boom")
	}
	id, _, err := m.ExecuteToolLeased(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}, ToolExecRequest{Operation: ToolOpExec})
	if err == nil {
		t.Fatal("expected exec error")
	}
	m.mu.Lock()
	inflight := m.instances[id].inflight
	m.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d, want 0", inflight)
	}
}

func TestWarmToolVMDistinctCompartmentsBootDistinctInstances(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	a, err := m.acquireWarmToolVM(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{})
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer m.releaseWarmToolVM(a.instanceID)
	b, err := m.acquireWarmToolVM(context.Background(), "agent", "cmp_b", runtimemanager.StartOpts{})
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	defer m.releaseWarmToolVM(b.instanceID)
	if a.instanceID == b.instanceID || boots.Load() != 2 {
		t.Fatalf("leases a=%+v b=%+v boots=%d, want distinct two boots", a, b, boots.Load())
	}
}

func TestWarmToolVMWinnerBootFailureLetsWaiterRecover(t *testing.T) {
	m := newWarmToolTestManager(t)
	var boots atomic.Int32
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		if n == 1 {
			time.Sleep(20 * time.Millisecond)
			return "", errors.New("boot failed")
		}
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			lease, err := m.acquireWarmToolVM(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{})
			if err == nil {
				m.releaseWarmToolVM(lease.instanceID)
			}
			errs <- err
		}()
	}
	var successes, failures int
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			failures++
		} else {
			successes++
		}
	}
	if successes != 1 || failures != 1 || boots.Load() != 2 {
		t.Fatalf("successes=%d failures=%d boots=%d, want 1/1/2", successes, failures, boots.Load())
	}
	m.mu.Lock()
	provisioning := len(m.provisioning)
	m.mu.Unlock()
	if provisioning != 0 {
		t.Fatalf("provisioning entries leaked: %d", provisioning)
	}
}

func TestWarmToolVMBootPanicClearsProvisioning(t *testing.T) {
	m := newWarmToolTestManager(t)
	m.startCompartment = func(context.Context, string, string, runtimemanager.StartOpts) (string, error) {
		panic("kaboom")
	}
	if _, err := m.acquireWarmToolVM(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("expected panic error, got %v", err)
	}
	m.mu.Lock()
	provisioning := len(m.provisioning)
	m.mu.Unlock()
	if provisioning != 0 {
		t.Fatalf("provisioning entries leaked: %d", provisioning)
	}
}

func TestWarmToolVMTagMissClearsProvisioning(t *testing.T) {
	m := newWarmToolTestManager(t)
	m.startCompartment = func(context.Context, string, string, runtimemanager.StartOpts) (string, error) {
		return "fc-vanished", nil
	}
	if _, err := m.acquireWarmToolVM(context.Background(), "agent", "cmp_a", runtimemanager.StartOpts{}); err == nil || !strings.Contains(err.Error(), "exited before lease registration") {
		t.Fatalf("expected tag-miss error, got %v", err)
	}
	m.mu.Lock()
	provisioning := len(m.provisioning)
	m.mu.Unlock()
	if provisioning != 0 {
		t.Fatalf("provisioning entries leaked: %d", provisioning)
	}
}

func TestWarmToolVMFingerprintMismatchDrainsBeforeFreshBoot(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	old := &instance{
		id:                 "fc-old",
		agentID:            key.agentID,
		compartmentID:      key.compartmentID,
		startupFingerprint: "stale",
		warmToolVM:         true,
		lastUsedAt:         time.Now(),
		done:               make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.instancesByKey[key] = old.id
	m.mu.Unlock()
	var bootedAfterOldDone atomic.Bool
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		select {
		case <-old.done:
			bootedAfterOldDone.Store(true)
		default:
		}
		id := "fc-fresh"
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	lease, err := m.acquireWarmToolVM(context.Background(), key.agentID, key.compartmentID, runtimemanager.StartOpts{})
	if err != nil {
		t.Fatalf("acquire after mismatch: %v", err)
	}
	defer m.releaseWarmToolVM(lease.instanceID)
	if lease.instanceID != "fc-fresh" || !bootedAfterOldDone.Load() {
		t.Fatalf("lease=%+v bootedAfterOldDone=%v, want fresh after old done", lease, bootedAfterOldDone.Load())
	}
}

func TestWarmToolVMRuntimeActorContextMismatchDrainsBeforeReuse(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	baseActor := runtimecontext.VerifiedActorContext{
		Principal:      "slack:user:U1",
		PlatformUserID: "platform-user-1",
		SessionID:      "session-1",
		TurnContextID:  "turn-1",
		RequestID:      "request-1",
		Verified:       true,
		Source:         "actor_context",
		ExpiresAt:      time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
	firstOpts := runtimemanager.StartOpts{
		CompartmentID:       key.compartmentID,
		RuntimeActorContext: baseActor,
	}
	firstFingerprint, err := m.startupFingerprint(key.agentID, key.compartmentID, firstOpts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	old := &instance{
		id:                 "fc-old-actor",
		agentID:            key.agentID,
		compartmentID:      key.compartmentID,
		startupFingerprint: firstFingerprint,
		warmToolVM:         true,
		lastUsedAt:         time.Now(),
		done:               make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.instancesByKey[key] = old.id
	m.mu.Unlock()

	var bootedAfterOldDone atomic.Bool
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		select {
		case <-old.done:
			bootedAfterOldDone.Store(true)
		default:
		}
		id := "fc-fresh-actor"
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}

	nextActor := baseActor
	nextActor.RequestID = "request-2"
	lease, err := m.acquireWarmToolVM(context.Background(), key.agentID, key.compartmentID, runtimemanager.StartOpts{
		CompartmentID:       key.compartmentID,
		RuntimeActorContext: nextActor,
	})
	if err != nil {
		t.Fatalf("acquire after actor mismatch: %v", err)
	}
	defer m.releaseWarmToolVM(lease.instanceID)
	if lease.instanceID != "fc-fresh-actor" || lease.reused || !bootedAfterOldDone.Load() {
		t.Fatalf("lease=%+v bootedAfterOldDone=%v, want fresh actor VM after old done", lease, bootedAfterOldDone.Load())
	}
}

func TestWarmToolVMRuntimeContextProjectionMismatchDrainsBeforeReuse(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	firstProjection := runtimecontext.Projection{
		Files: []runtimecontext.File{{
			Path:    runtimecontext.PathGitHubContext,
			Content: `{"service":"github","accessibleRepos":["owner/repo"]}`,
		}},
		MigratedServicePaths: []string{runtimecontext.PathGitHubContext},
	}
	firstOpts := runtimemanager.StartOpts{
		CompartmentID:            key.compartmentID,
		RuntimeContextProjection: firstProjection,
	}
	firstFingerprint, err := m.startupFingerprint(key.agentID, key.compartmentID, firstOpts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	old := &instance{
		id:                 "fc-old-context",
		agentID:            key.agentID,
		compartmentID:      key.compartmentID,
		startupFingerprint: firstFingerprint,
		warmToolVM:         true,
		lastUsedAt:         time.Now(),
		done:               make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.instancesByKey[key] = old.id
	m.mu.Unlock()

	var bootedAfterOldDone atomic.Bool
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		select {
		case <-old.done:
			bootedAfterOldDone.Store(true)
		default:
		}
		id := "fc-fresh-context"
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}

	nextProjection := firstProjection
	nextProjection.Files = []runtimecontext.File{{
		Path:    runtimecontext.PathGitHubContext,
		Content: `{"service":"github","accessibleRepos":["owner/other"]}`,
	}}
	lease, err := m.acquireWarmToolVM(context.Background(), key.agentID, key.compartmentID, runtimemanager.StartOpts{
		CompartmentID:            key.compartmentID,
		RuntimeContextProjection: nextProjection,
	})
	if err != nil {
		t.Fatalf("acquire after context mismatch: %v", err)
	}
	defer m.releaseWarmToolVM(lease.instanceID)
	if lease.instanceID != "fc-fresh-context" || lease.reused || !bootedAfterOldDone.Load() {
		t.Fatalf("lease=%+v bootedAfterOldDone=%v, want fresh context VM after old done", lease, bootedAfterOldDone.Load())
	}
}

func TestWarmToolVMDrainRequesterCancelStillPromotesOnRelease(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	old := &instance{
		id:                 "fc-old-cancel",
		agentID:            key.agentID,
		compartmentID:      key.compartmentID,
		startupFingerprint: "stale",
		warmToolVM:         true,
		inflight:           1,
		lastUsedAt:         time.Now(),
		done:               make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.instancesByKey[key] = old.id
	m.mu.Unlock()

	removed := make(chan struct{})
	m.removeInstance = func(_ context.Context, id string) error {
		if id != old.id {
			t.Fatalf("remove id = %q, want %q", id, old.id)
		}
		m.finishInstance(old)
		close(removed)
		return nil
	}
	m.startCompartment = func(context.Context, string, string, runtimemanager.StartOpts) (string, error) {
		t.Fatal("acquire should cancel while waiting for the draining instance")
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := m.acquireWarmToolVM(ctx, key.agentID, key.compartmentID, runtimemanager.StartOpts{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire err = %v, want deadline", err)
	}
	m.mu.Lock()
	draining := old.draining
	reapingBeforeRelease := old.reaping
	m.mu.Unlock()
	if !draining || reapingBeforeRelease {
		t.Fatalf("draining=%v reapingBeforeRelease=%v, want draining only", draining, reapingBeforeRelease)
	}

	m.releaseWarmToolVM(old.id)
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("release did not promote canceled drain to teardown")
	}
}

func TestWarmToolVMAcquireWaitsForReapingInstanceBeforeBoot(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	old := &instance{
		id:            "fc-old-reaping",
		agentID:       key.agentID,
		compartmentID: key.compartmentID,
		warmToolVM:    true,
		reaping:       true,
		lastUsedAt:    time.Now().Add(-2 * time.Minute),
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.instancesByKey[key] = old.id
	m.mu.Unlock()

	var boots atomic.Int32
	var bootedAfterDone atomic.Bool
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		boots.Add(1)
		select {
		case <-old.done:
			bootedAfterDone.Store(true)
		default:
		}
		id := "fc-fresh-after-reap"
		fp, err := m.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		m.addInstanceLocked(&instance{id: id, agentID: agentID, compartmentID: compartmentID, startupFingerprint: fp, done: make(chan struct{})})
		m.mu.Unlock()
		return id, nil
	}
	leaseCh := make(chan warmToolLease, 1)
	errCh := make(chan error, 1)
	go func() {
		lease, err := m.acquireWarmToolVM(context.Background(), key.agentID, key.compartmentID, runtimemanager.StartOpts{})
		if err != nil {
			errCh <- err
			return
		}
		leaseCh <- lease
	}()
	time.Sleep(50 * time.Millisecond)
	if boots.Load() != 0 {
		t.Fatalf("booted before reaping instance exited")
	}
	m.finishInstance(old)
	select {
	case err := <-errCh:
		t.Fatalf("acquire: %v", err)
	case lease := <-leaseCh:
		defer m.releaseWarmToolVM(lease.instanceID)
		if lease.instanceID != "fc-fresh-after-reap" || !bootedAfterDone.Load() {
			t.Fatalf("lease=%+v bootedAfterDone=%v", lease, bootedAfterDone.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not boot after reaping instance exited")
	}
}

func TestRemoveInstanceLockedWarmStaleKeyGuard(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	old := &instance{id: "old", agentID: key.agentID, compartmentID: key.compartmentID, warmToolVM: true, done: make(chan struct{})}
	fresh := &instance{id: "fresh", agentID: key.agentID, compartmentID: key.compartmentID, warmToolVM: true, done: make(chan struct{})}
	m.mu.Lock()
	m.addInstanceLocked(old)
	m.addInstanceLocked(fresh)
	m.instancesByKey[key] = fresh.id
	m.removeInstanceLocked(old)
	got := m.instancesByKey[key]
	m.mu.Unlock()
	if got != fresh.id {
		t.Fatalf("instancesByKey[%+v] = %q, want %q", key, got, fresh.id)
	}
}

func TestSweepIdleToolVMsReapsIdleWarmInstance(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	inst := &instance{
		id:            "fc-agent-cmp-a-idle",
		agentID:       key.agentID,
		compartmentID: key.compartmentID,
		warmToolVM:    true,
		lastUsedAt:    time.Now().Add(-2 * time.Minute),
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[key] = inst.id
	m.mu.Unlock()

	if got := m.sweepIdleToolVMs(time.Now()); got != 1 {
		t.Fatalf("sweep count = %d, want 1", got)
	}
	select {
	case <-inst.done:
	case <-time.After(time.Second):
		t.Fatal("idle warm instance was not reaped")
	}
	m.mu.Lock()
	_, stillIndexed := m.instancesByKey[key]
	_, stillPresent := m.instances[inst.id]
	m.mu.Unlock()
	if stillIndexed || stillPresent {
		t.Fatalf("instance still tracked after reap: indexed=%v present=%v", stillIndexed, stillPresent)
	}
}

func TestSweepIdleToolVMsFreezesBeforeRemove(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_order"}
	inst := &instance{
		id:            "fc-agent-cmp-order",
		agentID:       key.agentID,
		compartmentID: key.compartmentID,
		warmToolVM:    true,
		lastUsedAt:    time.Now().Add(-2 * time.Minute),
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[key] = inst.id
	m.mu.Unlock()

	var mu sync.Mutex
	var calls []string
	m.freezeWorkspace = func(context.Context, string) (BackupResponse, error) {
		mu.Lock()
		calls = append(calls, "freeze")
		mu.Unlock()
		return BackupResponse{}, nil
	}
	m.removeInstance = func(context.Context, string) error {
		mu.Lock()
		calls = append(calls, "remove")
		mu.Unlock()
		m.finishInstance(inst)
		return nil
	}
	if got := m.sweepIdleToolVMs(time.Now()); got != 1 {
		t.Fatalf("sweep count = %d, want 1", got)
	}
	select {
	case <-inst.done:
	case <-time.After(time.Second):
		t.Fatal("idle warm instance was not reaped")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "freeze,remove" {
		t.Fatalf("teardown calls = %#v, want freeze then remove", calls)
	}
}

func TestSweepIdleToolVMsSkipsBusyWarmInstance(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_busy"}
	inst := &instance{
		id:            "fc-agent-cmp-busy",
		agentID:       key.agentID,
		compartmentID: key.compartmentID,
		warmToolVM:    true,
		inflight:      1,
		lastUsedAt:    time.Now().Add(-2 * time.Minute),
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[key] = inst.id
	m.mu.Unlock()
	if got := m.sweepIdleToolVMs(time.Now()); got != 0 {
		t.Fatalf("sweep count = %d, want 0", got)
	}
	select {
	case <-inst.done:
		t.Fatal("busy warm instance was reaped")
	default:
	}
}

func TestSweepIdleToolVMsSkipsDrainingAndRecentlyUsed(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		inst *instance
	}{
		{
			name: "draining",
			inst: &instance{id: "fc-agent-cmp-draining", agentID: "agent", compartmentID: "cmp_draining", warmToolVM: true, draining: true, lastUsedAt: now.Add(-2 * time.Minute), done: make(chan struct{})},
		},
		{
			name: "recent",
			inst: &instance{id: "fc-agent-cmp-recent", agentID: "agent", compartmentID: "cmp_recent", warmToolVM: true, lastUsedAt: now, done: make(chan struct{})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newWarmToolTestManager(t)
			key := runtimeInstanceKey{agentID: tc.inst.agentID, compartmentID: tc.inst.compartmentID}
			m.mu.Lock()
			m.addInstanceLocked(tc.inst)
			m.instancesByKey[key] = tc.inst.id
			m.mu.Unlock()
			if got := m.sweepIdleToolVMs(now); got != 0 {
				t.Fatalf("sweep count = %d, want 0", got)
			}
			select {
			case <-tc.inst.done:
				t.Fatal("instance was reaped")
			default:
			}
		})
	}
}

func TestSweepIdleToolVMsToleratesRemoveErrorWithoutDoubleTeardown(t *testing.T) {
	originalSignal := signalFirecrackerByIDFunc
	t.Cleanup(func() { signalFirecrackerByIDFunc = originalSignal })
	escalated := make(chan syscall.Signal, 1)
	signalFirecrackerByIDFunc = func(_ string, signal syscall.Signal) error {
		escalated <- signal
		return nil
	}
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_error"}
	inst := &instance{
		id:            "fc-agent-cmp-error",
		agentID:       key.agentID,
		compartmentID: key.compartmentID,
		warmToolVM:    true,
		lastUsedAt:    time.Now().Add(-2 * time.Minute),
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[key] = inst.id
	m.mu.Unlock()
	var removes atomic.Int32
	m.removeInstance = func(context.Context, string) error {
		removes.Add(1)
		return errors.New("still shutting down")
	}
	if got := m.sweepIdleToolVMs(time.Now()); got != 1 {
		t.Fatalf("first sweep count = %d, want 1", got)
	}
	select {
	case signal := <-escalated:
		if signal != syscall.SIGKILL {
			t.Fatalf("escalation signal = %v, want SIGKILL", signal)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for teardown escalation")
	}
	m.mu.Lock()
	_, stillIndexed := m.instancesByKey[key]
	_, stillPresent := m.instances[inst.id]
	reaping := inst.reaping
	m.mu.Unlock()
	if !stillIndexed || !stillPresent || !reaping {
		t.Fatalf("remove error tracking: indexed=%v present=%v reaping=%v", stillIndexed, stillPresent, reaping)
	}
	if got := m.sweepIdleToolVMs(time.Now()); got != 0 {
		t.Fatalf("second sweep count = %d, want 0", got)
	}
	if removes.Load() != 1 {
		t.Fatalf("remove calls = %d, want 1", removes.Load())
	}
}

func TestWarmToolVMTeardownEscalatesThroughPrivilegedLauncher(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan privilegedLauncherRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		if decodeErr := json.NewDecoder(conn).Decode(&req); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		requestSeen <- req
		serverErr <- json.NewEncoder(conn).Encode(privilegedLauncherResponse{OK: true})
	}()

	originalSignal := signalFirecrackerByIDFunc
	t.Cleanup(func() { signalFirecrackerByIDFunc = originalSignal })
	signalFirecrackerByIDFunc = func(string, syscall.Signal) error {
		t.Fatal("unprivileged signal fallback must not run in launcher mode")
		return nil
	}
	m := newWarmToolTestManager(t)
	m.launcher = newPrivilegedLauncherClient(socketPath)
	m.freezeWorkspace = func(context.Context, string) (BackupResponse, error) { return BackupResponse{}, nil }
	m.removeInstance = func(context.Context, string) error { return errors.New("initial remove failed") }
	inst := &instance{id: "fc-agent-cmp-privileged", agentID: "agent", compartmentID: "cmp", warmToolVM: true, done: make(chan struct{})}
	m.teardownWarmToolVMContext(context.Background(), inst)
	if err := <-serverErr; err != nil {
		t.Fatalf("launcher server: %v", err)
	}
	if req := <-requestSeen; req.Op != "stop" || req.ID != inst.id {
		t.Fatalf("launcher escalation request = %+v", req)
	}
}

func TestShutdownReapsWarmToolVMs(t *testing.T) {
	m := newWarmToolTestManager(t)
	inst := &instance{
		id:            "fc-agent-cmp-a-shutdown",
		agentID:       "agent",
		compartmentID: "cmp_a",
		warmToolVM:    true,
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}] = inst.id
	m.mu.Unlock()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-inst.done:
	default:
		t.Fatal("shutdown did not reap warm instance")
	}
}

func TestShutdownReapsNonWarmRuntimeVMs(t *testing.T) {
	m := newWarmToolTestManager(t)
	inst := &instance{
		id:            "fc-agent-cmp-session-shutdown",
		agentID:       "agent",
		compartmentID: "cmp_session",
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.mu.Unlock()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-inst.done:
	default:
		t.Fatal("shutdown did not reap non-warm runtime instance")
	}
}

func TestShutdownWaitsForInflightWarmToolVMWork(t *testing.T) {
	m := newWarmToolTestManager(t)
	inst := &instance{
		id:            "fc-agent-cmp-a-inflight-shutdown",
		agentID:       "agent",
		compartmentID: "cmp_a",
		warmToolVM:    true,
		inflight:      1,
		done:          make(chan struct{}),
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.instancesByKey[runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}] = inst.id
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- m.Shutdown(context.Background())
	}()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before in-flight work drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-inst.done:
		t.Fatal("shutdown reaped warm instance before in-flight work drained")
	default:
	}
	m.mu.Lock()
	inst.inflight = 0
	m.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after in-flight work drained")
	}
	select {
	case <-inst.done:
	default:
		t.Fatal("shutdown did not reap warm instance after drain")
	}
}

func TestShutdownWaitsForProvisioningAndRejectsAcquire(t *testing.T) {
	m := newWarmToolTestManager(t)
	key := runtimeInstanceKey{agentID: "agent", compartmentID: "cmp_a"}
	ch := make(chan struct{})
	m.mu.Lock()
	m.provisioning[key] = ch
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- m.Shutdown(context.Background())
	}()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before provisioning cleared: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.mu.Lock()
	delete(m.provisioning, key)
	close(ch)
	m.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after provisioning cleared")
	}
	if _, err := m.acquireWarmToolVM(context.Background(), key.agentID, key.compartmentID, runtimemanager.StartOpts{}); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("acquire during shutdown err = %v", err)
	}
}

func TestStartSweepsStartupOrphanRunDir(t *testing.T) {
	m := newWarmToolTestManager(t)
	runDir := filepath.Join(m.cfg.MicroVMRunDir, "fc-agent-cmp-a-orphan")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { firecrackerProcessRunningFunc = orig })

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("orphan run dir still exists, stat err=%v", err)
	}
	_ = m.Shutdown(context.Background())
}

func TestStartTreatsStartupOrphanSweepErrorsAsNonFatal(t *testing.T) {
	m := newWarmToolTestManager(t)
	runDir := filepath.Join(m.cfg.MicroVMRunDir, "fc-agent-cmp-a-orphan-error")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(string) (bool, error) { return false, errors.New("proc unavailable") }
	t.Cleanup(func() { firecrackerProcessRunningFunc = orig })

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start should not fail on orphan sweep error: %v", err)
	}
	_ = m.Shutdown(context.Background())
}

func TestStartBoundsRunningStartupOrphanStop(t *testing.T) {
	m := newWarmToolTestManager(t)
	runDir := filepath.Join(m.cfg.MicroVMRunDir, "fc-agent-cmp-a-unkillable")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { firecrackerProcessRunningFunc = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start should not fail on bounded orphan stop timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("start elapsed = %s, want bounded by caller context", elapsed)
	}
	_ = m.Shutdown(context.Background())
}

func TestStopInstanceLauncherOnlyRetriesProcessScanErrors(t *testing.T) {
	m := newWarmToolTestManager(t)
	inst := &instance{id: "fc-agent-cmp-a-stop", launcherOnly: true, done: make(chan struct{})}
	orig := firecrackerProcessRunningFunc
	var calls atomic.Int32
	firecrackerProcessRunningFunc = func(string) (bool, error) {
		if calls.Add(1) == 1 {
			return false, errors.New("temporary proc scan failure")
		}
		return false, nil
	}
	t.Cleanup(func() { firecrackerProcessRunningFunc = orig })

	done := make(chan error, 1)
	go func() {
		done <- m.stopInstance(context.Background(), inst, false)
	}()
	select {
	case <-inst.done:
		t.Fatal("done closed on transient process scan error")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopInstance: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopInstance did not finish after process exited")
	}
}

func TestCreateAndStartCompartmentDoesNotRemoveSibling(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"},
		instances: map[string]*instance{},
	}
	a := &instance{id: "fc-agent-start-cmp-a", agentID: "agent-start", compartmentID: "cmp_a", done: make(chan struct{})}
	m.mu.Lock()
	m.addInstanceLocked(a)
	m.mu.Unlock()
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, _ runtimemanager.StartOpts) (string, error) {
		inst := &instance{id: "fc-agent-start-cmp-b", agentID: agentID, compartmentID: compartmentID, done: make(chan struct{})}
		m.mu.Lock()
		m.addInstanceLocked(inst)
		m.mu.Unlock()
		return inst.id, nil
	}

	b, err := m.createAndStart(context.Background(), "agent-start", runtimemanager.StartOpts{CompartmentID: "cmp_b"})
	if err != nil {
		t.Fatalf("create cmp_b: %v", err)
	}
	if b != "fc-agent-start-cmp-b" {
		t.Fatalf("created instance = %q", b)
	}
	if got := m.lookup("fc-agent-start-cmp-a"); got != a {
		t.Fatalf("sibling cmp_a was removed: %#v", got)
	}
}

func TestCreateAndStartRequiresCIDRForSecondCompartment(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{MicroVMGuestIP: "172.30.0.10"},
		instances: map[string]*instance{},
	}
	m.mu.Lock()
	m.addInstanceLocked(&instance{id: "fc-agent-start-cmp-a", agentID: "agent-start", compartmentID: "cmp_a", done: make(chan struct{})})
	m.mu.Unlock()
	m.startCompartment = func(context.Context, string, string, runtimemanager.StartOpts) (string, error) {
		t.Fatal("start should be rejected by admission guard before launching")
		return "", nil
	}

	_, err := m.createAndStart(context.Background(), "agent-start", runtimemanager.StartOpts{CompartmentID: "cmp_b"})
	if err == nil || !strings.Contains(err.Error(), "SANDBOX_HOST_MICROVM_BRIDGE_CIDR") {
		t.Fatalf("expected missing bridge CIDR refusal, got %v", err)
	}
}

func TestBuildFirecrackerConfigIncludesWorkspaceAndVsock(t *testing.T) {
	cfg := &config.Config{
		MicroVMKernelPath:      "/artifacts/vmlinux",
		MicroVMSharedImagePath: "/artifacts/shared.erofs",
		MicroVMVCPUs:           2,
		MicroVMMemoryMiB:       2048,
		MicroVMCPUTemplate:     "None",
	}

	got := buildFirecrackerConfig(cfg, cfg.MicroVMKernelPath, "/run/rootfs.ext4", "/vol/workspace.ext4", cfg.MicroVMSharedImagePath, "/run/agent-manager.vsock", "", "")

	if got.BootSource.KernelImagePath != "/artifacts/vmlinux" {
		t.Fatalf("kernel path = %q", got.BootSource.KernelImagePath)
	}
	if !strings.Contains(got.BootSource.BootArgs, "root=/dev/vda") {
		t.Fatalf("boot args missing root device: %q", got.BootSource.BootArgs)
	}
	if !strings.Contains(got.BootSource.BootArgs, "init=/init") {
		t.Fatalf("boot args missing generated image init: %q", got.BootSource.BootArgs)
	}
	if !strings.Contains(got.BootSource.BootArgs, "noxsave") {
		t.Fatalf("boot args missing noxsave: %q", got.BootSource.BootArgs)
	}
	if got.Machine.VCPUCount != 2 || got.Machine.MemSizeMiB != 2048 || got.Machine.SMT || got.Machine.CPUTemplate != "None" {
		t.Fatalf("machine config = %#v", got.Machine)
	}
	if got.Vsock.GuestCID == 0 || got.Vsock.UDSPath != "/run/agent-manager.vsock" {
		t.Fatalf("vsock config = %#v", got.Vsock)
	}
	if len(got.Drives) != 3 {
		t.Fatalf("drives len = %d, want 3", len(got.Drives))
	}
	if got.Drives[0].DriveID != "rootfs" || !got.Drives[0].IsRootDevice {
		t.Fatalf("root drive = %#v", got.Drives[0])
	}
	if got.Drives[1].DriveID != "workspace" || got.Drives[1].IsReadOnly {
		t.Fatalf("workspace drive = %#v", got.Drives[1])
	}
	if got.Drives[2].DriveID != "shared" || !got.Drives[2].IsReadOnly {
		t.Fatalf("shared drive = %#v", got.Drives[2])
	}
}

func TestBuildFirecrackerConfigEnforcesSandboxRuntimePolicy(t *testing.T) {
	cfg := &config.Config{MicroVMKernelPath: "/vmlinux", MicroVMVCPUs: 2, MicroVMMemoryMiB: 512}
	policy := &runtimemanager.SandboxRuntimePolicy{
		VCPUs: 1, MemoryMiB: 128, WorkspaceSizeMiB: 64, ProcessLimit: 16,
		WorkspaceWritable: false, SharedReadOnly: true,
	}
	got := buildFirecrackerConfigWithPolicy(cfg, "/vmlinux", "/rootfs", "/workspace", "/shared", "/vsock", "", "", policy)
	if got.Machine.VCPUCount != 1 || got.Machine.MemSizeMiB != 128 {
		t.Fatalf("machine config = %+v", got.Machine)
	}
	if len(got.Drives) != 3 || !got.Drives[1].IsReadOnly || !got.Drives[2].IsReadOnly {
		t.Fatalf("drive policy = %+v", got.Drives)
	}
	if !strings.Contains(got.BootSource.BootArgs, "agent-manager.process_limit=16") {
		t.Fatalf("boot args = %q", got.BootSource.BootArgs)
	}
}

func TestMicroVMImageForStartSelectsToolExecutorImage(t *testing.T) {
	m := &Manager{cfg: &config.Config{
		MicroVMRootfsPath:          "/images/agent-rootfs.ext4",
		MicroVMSharedImagePath:     "/images/agent-shared.img",
		MicroVMToolRootfsPath:      "/images/tool-rootfs.ext4",
		MicroVMToolSharedImagePath: "/images/tool-shared.img",
	}}
	// Empty runtime class defaults to the tool executor (the only microVM class).
	defaultImage, err := m.microVMImageForStart(runtimemanager.StartOpts{})
	if err != nil {
		t.Fatalf("default image: %v", err)
	}
	if defaultImage.RuntimeClass != runtimemanager.RuntimeClassToolExecutor || defaultImage.RootfsPath != "/images/tool-rootfs.ext4" || defaultImage.SharedImagePath != "/images/tool-shared.img" {
		t.Fatalf("default (tool) image = %#v", defaultImage)
	}
	toolImage, err := m.microVMImageForStart(runtimemanager.StartOpts{RuntimeClass: runtimemanager.RuntimeClassToolExecutor})
	if err != nil {
		t.Fatalf("tool image: %v", err)
	}
	if toolImage.RuntimeClass != runtimemanager.RuntimeClassToolExecutor || toolImage.RootfsPath != "/images/tool-rootfs.ext4" || toolImage.SharedImagePath != "/images/tool-shared.img" {
		t.Fatalf("tool image = %#v", toolImage)
	}
}

func TestPrepareLaunchImageResolvesFallbackBeforeSettingDestination(t *testing.T) {
	sourceRootfs := filepath.Join(t.TempDir(), "source-rootfs.ext4")
	m := &Manager{cfg: &config.Config{MicroVMRootfsPath: sourceRootfs}}

	_, err := m.prepareLaunchImage(t.TempDir(), microVMImageSelection{})
	if err == nil {
		t.Fatal("expected missing source rootfs to fail")
	}
	if !strings.Contains(err.Error(), sourceRootfs) {
		t.Fatalf("prepare error = %q, want resolved source %q", err, sourceRootfs)
	}
}

func TestStartupFingerprintIncludesRuntimeClassAndSelectedImage(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	agentRootfs := write("agent-rootfs.ext4", "agent-rootfs")
	toolRootfs := write("tool-rootfs.ext4", "tool-rootfs")
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     agentRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	toolOpts := runtimemanager.StartOpts{CompartmentID: "cmp_a", RuntimeClass: runtimemanager.RuntimeClassToolExecutor}
	toolFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", toolOpts)
	if err != nil {
		t.Fatalf("tool fingerprint: %v", err)
	}
	nextTime := time.Now().Add(time.Second)
	if err := os.WriteFile(toolRootfs, []byte("changed-tool-rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(toolRootfs, nextTime, nextTime); err != nil {
		t.Fatal(err)
	}
	changedToolFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", toolOpts)
	if err != nil {
		t.Fatalf("changed tool fingerprint: %v", err)
	}
	if changedToolFingerprint == toolFingerprint {
		t.Fatal("tool image identity change should change startup fingerprint")
	}
}

func TestStartupFingerprintIncludesToolExecutorContractAndCapabilities(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	toolRootfs := write("tool-rootfs.ext4", "tool-rootfs")
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     toolRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	toolOpts := runtimemanager.StartOpts{CompartmentID: "cmp_a", RuntimeClass: runtimemanager.RuntimeClassToolExecutor}
	baseFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", toolOpts)
	if err != nil {
		t.Fatalf("base fingerprint: %v", err)
	}

	origContractVersion := toolExecutorFingerprintContractVersion
	origCapabilities := append([]string(nil), toolExecutorFingerprintCapabilities...)
	t.Cleanup(func() {
		toolExecutorFingerprintContractVersion = origContractVersion
		toolExecutorFingerprintCapabilities = origCapabilities
	})

	toolExecutorFingerprintContractVersion++
	contractFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", toolOpts)
	if err != nil {
		t.Fatalf("contract fingerprint: %v", err)
	}
	if contractFingerprint == baseFingerprint {
		t.Fatal("tool executor contract version change should change startup fingerprint")
	}

	toolExecutorFingerprintContractVersion = origContractVersion
	toolExecutorFingerprintCapabilities = append(append([]string(nil), origCapabilities...), "new-tool-mount-capability")
	capabilityFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", toolOpts)
	if err != nil {
		t.Fatalf("capability fingerprint: %v", err)
	}
	if capabilityFingerprint == baseFingerprint {
		t.Fatal("tool executor capability change should change startup fingerprint")
	}
}

func TestStartupFingerprintUsesEffectiveProfileHash(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	toolRootfs := write("tool-rootfs.ext4", "tool-rootfs")
	shared := write("shared.img", "shared")
	m := &Manager{cfg: &config.Config{
		DataDir:                    dir,
		PlatformAPIURL:             "http://platform.test",
		MicroVMRootfsPath:          toolRootfs,
		MicroVMToolRootfsPath:      toolRootfs,
		MicroVMSharedImagePath:     shared,
		MicroVMToolSharedImagePath: shared,
	}}
	opts := runtimemanager.StartOpts{CompartmentID: "cmp_a", RuntimeClass: runtimemanager.RuntimeClassToolExecutor, ShapeFingerprint: "profile-a"}
	firstFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	opts.ShapeFingerprint = "profile-b"
	secondFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if firstFingerprint == secondFingerprint {
		t.Fatal("fingerprint did not change after effective profile hash changed")
	}
}

func TestStartupFingerprintIncludesActorPrincipal(t *testing.T) {
	dir := t.TempDir()
	toolRootfs := filepath.Join(dir, "tool-rootfs.ext4")
	if err := os.WriteFile(toolRootfs, []byte("tool-rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     toolRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	opts := runtimemanager.StartOpts{
		CompartmentID:  "cmp_a",
		RuntimeClass:   runtimemanager.RuntimeClassToolExecutor,
		ActorPrincipal: "slack:user:U1",
	}
	firstFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	opts.ActorPrincipal = "slack:user:U2"
	secondFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if firstFingerprint == secondFingerprint {
		t.Fatal("fingerprint did not change after actor principal changed")
	}
}

func TestStartupFingerprintIncludesRuntimeActorContext(t *testing.T) {
	dir := t.TempDir()
	toolRootfs := filepath.Join(dir, "tool-rootfs.ext4")
	if err := os.WriteFile(toolRootfs, []byte("tool-rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     toolRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	actor := runtimecontext.VerifiedActorContext{
		Principal:      "slack:user:U1",
		PlatformUserID: "platform-user-1",
		SessionID:      "session-1",
		TurnContextID:  "turn-1",
		RequestID:      "request-1",
		Verified:       true,
		Source:         "actor_context",
		ExpiresAt:      time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
	opts := runtimemanager.StartOpts{
		CompartmentID:       "cmp_a",
		RuntimeClass:        runtimemanager.RuntimeClassToolExecutor,
		ActorPrincipal:      "slack:user:U1",
		RuntimeActorContext: actor,
	}
	firstFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	opts.RuntimeActorContext.RequestID = "request-2"
	secondFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if firstFingerprint == secondFingerprint {
		t.Fatal("fingerprint did not change after runtime actor request binding changed")
	}
	opts.RuntimeActorContext = actor
	opts.RuntimeActorContext.PlatformUserID = "platform-user-2"
	thirdFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("third fingerprint: %v", err)
	}
	if firstFingerprint == thirdFingerprint {
		t.Fatal("fingerprint did not change after runtime actor platform user changed")
	}
}

func TestStartupFingerprintIncludesRuntimeContextProjection(t *testing.T) {
	dir := t.TempDir()
	toolRootfs := filepath.Join(dir, "tool-rootfs.ext4")
	if err := os.WriteFile(toolRootfs, []byte("tool-rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     toolRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	opts := runtimemanager.StartOpts{
		CompartmentID: "cmp_a",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
		RuntimeContextProjection: runtimecontext.Projection{
			Files: []runtimecontext.File{{
				Path:    runtimecontext.PathGitHubContext,
				Content: `{"service":"github","accessibleRepos":["owner/repo"]}`,
			}},
			MigratedServicePaths:  []string{runtimecontext.PathGitHubContext},
			PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
		},
	}
	firstFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	opts.RuntimeContextProjection.Files[0].Content = `{"service":"github","accessibleRepos":["owner/other"]}`
	contentFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("content fingerprint: %v", err)
	}
	if contentFingerprint == firstFingerprint {
		t.Fatal("fingerprint did not change after runtime context content changed")
	}
	opts.RuntimeContextProjection.Files = nil
	opts.RuntimeContextProjection.OmittedPaths = []string{runtimecontext.PathGitHubContext}
	omittedFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", opts)
	if err != nil {
		t.Fatalf("omitted fingerprint: %v", err)
	}
	if omittedFingerprint == contentFingerprint || omittedFingerprint == firstFingerprint {
		t.Fatal("fingerprint did not change after runtime context file became omitted")
	}
}

func TestStartupFingerprintCanonicalizesRuntimeContextProjection(t *testing.T) {
	dir := t.TempDir()
	toolRootfs := filepath.Join(dir, "tool-rootfs.ext4")
	if err := os.WriteFile(toolRootfs, []byte("tool-rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{
		DataDir:               dir,
		PlatformAPIURL:        "http://platform.test",
		MicroVMRootfsPath:     toolRootfs,
		MicroVMToolRootfsPath: toolRootfs,
	}}
	base := runtimemanager.StartOpts{
		CompartmentID: "cmp_a",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
		RuntimeContextProjection: runtimecontext.Projection{
			Files: []runtimecontext.File{
				{Path: runtimecontext.PathNotionContext, Content: `{"service":"notion"}`},
				{Path: runtimecontext.PathGitHubContext, Content: `{"service":"github"}`},
			},
			OmittedPaths:          []string{runtimecontext.PathGitLabContext, runtimecontext.PathJiraContext},
			MigratedServicePaths:  []string{runtimecontext.PathNotionContext, runtimecontext.PathGitHubContext},
			PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
		},
	}
	first, err := m.startupFingerprint("agent-1", "cmp_a", base)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	reordered := base
	reordered.RuntimeContextProjection.Files = []runtimecontext.File{
		{Path: runtimecontext.PathGitHubContext, Content: `{"service":"github"}`},
		{Path: runtimecontext.PathNotionContext, Content: `{"service":"notion"}`},
	}
	reordered.RuntimeContextProjection.OmittedPaths = []string{runtimecontext.PathJiraContext, runtimecontext.PathGitLabContext}
	reordered.RuntimeContextProjection.MigratedServicePaths = []string{runtimecontext.PathGitHubContext, runtimecontext.PathNotionContext}
	second, err := m.startupFingerprint("agent-1", "cmp_a", reordered)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatal("equivalent runtime context projection order changed startup fingerprint")
	}
}

func TestBuildFirecrackerConfigIncludesTapInterface(t *testing.T) {
	cfg := &config.Config{
		MicroVMKernelPath: "/artifacts/vmlinux",
		MicroVMVCPUs:      1,
		MicroVMMemoryMiB:  512,
	}
	got := buildFirecrackerConfig(cfg, cfg.MicroVMKernelPath, "/run/rootfs.ext4", "/vol/workspace.ext4", cfg.MicroVMSharedImagePath, "/run/agent-manager.vsock", "agfc123456", "")
	if len(got.NetworkIfaces) != 1 {
		t.Fatalf("network interfaces = %#v", got.NetworkIfaces)
	}
	if got.NetworkIfaces[0].IfaceID != "eth0" || got.NetworkIfaces[0].HostDevName != "agfc123456" {
		t.Fatalf("network interface = %#v", got.NetworkIfaces[0])
	}
	if !strings.HasPrefix(got.NetworkIfaces[0].GuestMAC, "02:") {
		t.Fatalf("guest mac = %q", got.NetworkIfaces[0].GuestMAC)
	}
}

func TestPrepareLaunchUnjailedIncludesInstanceID(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{cfg: &config.Config{
		MicroVMAllowUnjailed: true,
		FirecrackerPath:      "firecracker",
		MicroVMVCPUs:         1,
		MicroVMMemoryMiB:     512,
		MicroVMKernelPath:    "/kernel",
	}}
	launch, err := m.prepareLaunch(context.Background(), "fc-agent-cmp-a-id", dir, "/kernel", "/rootfs.ext4", "/workspace.ext4", "", "", "")
	if err != nil {
		t.Fatalf("prepare launch: %v", err)
	}
	got := strings.Join(launch.args, " ")
	if !strings.Contains(got, "--id fc-agent-cmp-a-id") {
		t.Fatalf("launch args = %q, want --id", got)
	}
}

func TestRegisterSourceBindingUsesSandboxIdentityAndConfiguredGuestAddress(t *testing.T) {
	registrar := &fakeSourceBindingRegistrar{}
	m := &Manager{
		cfg:            &config.Config{MicroVMGuestIP: "172.30.0.10"},
		guestIPs:       map[string]string{},
		sourceBindings: registrar,
	}
	if _, err := m.reserveGuestIP("fc-agent-1"); err != nil {
		t.Fatalf("reserve guest IP: %v", err)
	}
	token, err := m.registerSourceBinding(
		context.Background(), "fc-agent-1", "env-1", "instance-1", 4, []string{"connection-1"},
	)
	if err != nil {
		t.Fatalf("register source binding: %v", err)
	}
	if len(registrar.registered) != 1 {
		t.Fatalf("registered bindings = %#v", registrar.registered)
	}
	got := registrar.registered[0]
	if got.EnvironmentID != "env-1" || got.InstanceID != "instance-1" || got.SourceAddress != "172.30.0.10" || got.Generation != 4 {
		t.Fatalf("binding = %#v", got)
	}
	if token != registrar.sourceToken {
		t.Fatalf("source token = %q, want %q", token, registrar.sourceToken)
	}
}

func TestRegisterSourceBindingRequiresSandboxIdentity(t *testing.T) {
	m := &Manager{
		cfg:            &config.Config{MicroVMGuestIP: "172.30.0.10"},
		guestIPs:       map[string]string{"runtime-1": "172.30.0.10"},
		sourceBindings: &fakeSourceBindingRegistrar{},
	}
	_, err := m.registerSourceBinding(
		context.Background(), "runtime-1", "", "", 0, []string{"connection-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "Environment") {
		t.Fatalf("expected Sandbox identity error, got %v", err)
	}
}

func TestRemoveInstanceLockedDoesNotRemoveReplacement(t *testing.T) {
	m := &Manager{instances: map[string]*instance{}}
	m.mu.Lock()
	old := &instance{id: "fc-old", agentID: "agent-x", compartmentID: "cmp_a"}
	m.addInstanceLocked(old)
	m.addInstanceLocked(&instance{id: "fc-new", agentID: "agent-x", compartmentID: "cmp_a"})
	m.removeInstanceLocked(old)
	m.mu.Unlock()

	got := m.lookup("fc-new")
	if got == nil || got.id != "fc-new" {
		t.Fatalf("replacement lookup after stale removal = %#v, want fc-new", got)
	}
	if _, ok := m.instances["fc-old"]; ok {
		t.Fatal("old instance was not removed from m.instances")
	}
}

func TestUntrackedInstanceIDsExcludesTrackedSiblings(t *testing.T) {
	m := &Manager{instances: map[string]*instance{}}
	m.mu.Lock()
	m.addInstanceLocked(&instance{id: "fc-tracked-default", agentID: "agent-x", compartmentID: "default"})
	m.addInstanceLocked(&instance{id: "fc-tracked-sibling", agentID: "agent-x", compartmentID: "cmp_b"})
	m.mu.Unlock()

	got := m.untrackedInstanceIDs([]string{"fc-tracked-default", "fc-orphan-1", "fc-tracked-sibling", "fc-orphan-2"})
	want := map[string]bool{"fc-orphan-1": true, "fc-orphan-2": true}
	if len(got) != len(want) {
		t.Fatalf("untracked = %v, want only the two orphans", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("untracked included a tracked instance: %q in %v", id, got)
		}
	}
}

func TestPrepareJailedLaunchStagesArtifactsAndCommand(t *testing.T) {
	dir, err := os.MkdirTemp("", "ag-jail-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	write := func(path, text string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	kernel := write(filepath.Join(dir, "kernel"), "kernel")
	rootfs := write(filepath.Join(dir, "rootfs.ext4"), "rootfs")
	workspace := write(filepath.Join(dir, "workspace.ext4"), "workspace")
	shared := write(filepath.Join(dir, "shared.erofs"), "shared")
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	m := &Manager{cfg: &config.Config{
		FirecrackerPath:            filepath.Join(dir, "firecracker"),
		JailerPath:                 filepath.Join(dir, "jailer"),
		MicroVMKernelPath:          kernel,
		MicroVMSharedImagePath:     shared,
		MicroVMJailerChrootBaseDir: filepath.Join(dir, "jailer-root"),
		MicroVMJailerUID:           os.Getuid(),
		MicroVMJailerGID:           os.Getgid(),
		MicroVMJailerCgroupVersion: 2,
		MicroVMJailerParentCgroup:  "agent-manager-test",
		MicroVMMemoryMiB:           512,
		MicroVMVCPUs:               1,
		MicroVMWorkspaceSizeMiB:    8,
		MicroVMAllowUnjailed:       false,
	}}
	launch, err := m.prepareLaunch(context.Background(), "fc-agent-123", runDir, kernel, rootfs, workspace, shared, "agfc123", "")
	if err != nil {
		t.Fatalf("prepare launch: %v", err)
	}
	if launch.executable != m.cfg.JailerPath {
		t.Fatalf("executable = %q", launch.executable)
	}
	args := strings.Join(launch.args, " ")
	for _, want := range []string{"--id fc-agent-123", "--exec-file " + m.cfg.FirecrackerPath, "--chroot-base-dir " + m.cfg.MicroVMJailerChrootBaseDir, "--new-pid-ns", "--cgroup memory.max=805306368", "-- --api-sock firecracker.sock --config-file firecracker.json"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if launch.config.BootSource.KernelImagePath != kernelName {
		t.Fatalf("kernel path = %q", launch.config.BootSource.KernelImagePath)
	}
	if launch.config.Drives[0].PathOnHost != rootfsName || launch.config.Drives[1].PathOnHost != workspaceName {
		t.Fatalf("drive paths = %#v", launch.config.Drives)
	}
	if launch.config.Vsock.UDSPath != vsockUDSName || launch.vsockUDS != filepath.Join(launch.jailRoot, vsockUDSName) || launch.socketPath != filepath.Join(launch.jailRoot, firecrackerSockName) {
		t.Fatalf("socket/vsock = %q %q %q", launch.socketPath, launch.vsockUDS, launch.config.Vsock.UDSPath)
	}
	for _, name := range []string{kernelName, rootfsName, workspaceName, sharedImageName} {
		if _, err := os.Stat(filepath.Join(launch.jailRoot, name)); err != nil {
			t.Fatalf("staged %s: %v", name, err)
		}
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	stagedWorkspaceInfo, err := os.Stat(filepath.Join(launch.jailRoot, workspaceName))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(workspaceInfo, stagedWorkspaceInfo) {
		t.Fatal("workspace must be linked into the jail, not copied")
	}
}

func TestPrepareJailedLaunchRejectsUnixSocketPathOverflowBeforeStaging(t *testing.T) {
	base := filepath.Join(os.TempDir(), strings.Repeat("j", maxUnixSocketPathLen))
	m := &Manager{cfg: &config.Config{
		FirecrackerPath:            "/usr/local/bin/firecracker",
		MicroVMJailerChrootBaseDir: base,
		MicroVMJailerUID:           os.Getuid(),
		MicroVMJailerGID:           os.Getgid(),
		MicroVMAllowUnjailed:       false,
	}}

	_, err := m.prepareLaunch(
		context.Background(),
		"fc-agent-123",
		t.TempDir(),
		"missing-kernel",
		"missing-rootfs",
		"missing-workspace",
		"",
		"",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "exceeding the unix socket limit") {
		t.Fatalf("overlong jailed socket path error = %v", err)
	}
	if !strings.Contains(err.Error(), "SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR") {
		t.Fatalf("overlong jailed socket path did not identify its setting: %v", err)
	}
	if _, statErr := os.Stat(m.jailerRoot("fc-agent-123")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("overlong jailed socket path staged a partial jail: %v", statErr)
	}
}

func TestJailerMemoryCgroupAddsHostOverhead(t *testing.T) {
	if got := jailerMemoryCgroup(2, 2048); got != "memory.max=2415919104" {
		t.Fatalf("cgroup v2 = %q", got)
	}
	if got := jailerMemoryCgroup(1, 512); got != "memory.limit_in_bytes=805306368" {
		t.Fatalf("cgroup v1 = %q", got)
	}
}

func TestTapOwnerUIDUsesJailerUIDWhenJailed(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMJailerUID: 4242}}
	if got := m.tapOwnerUID(); got != 4242 {
		t.Fatalf("tap owner uid = %d", got)
	}
}

func TestStageLinkedJailFileFallsBackToCopyAcrossDevices(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm unavailable")
	}
	src := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(src, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstDir, err := os.MkdirTemp("/dev/shm", "agent-manager-jail-stage-*")
	if err != nil {
		t.Skipf("create tmpfs dir: %v", err)
	}
	defer os.RemoveAll(dstDir)

	probe := filepath.Join(dstDir, "probe")
	if err := os.Link(src, probe); err == nil {
		_ = os.Remove(probe)
		t.Skip("test requires src and dst on different devices")
	} else if !errors.Is(err, syscall.EXDEV) {
		t.Skipf("hard link probe did not fail with EXDEV: %v", err)
	}

	dst := filepath.Join(dstDir, "rootfs.ext4")
	if err := stageLinkedJailFile(dst, src, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("stage linked jail file: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "rootfs" {
		t.Fatalf("copied contents = %q", got)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(srcInfo, dstInfo) {
		t.Fatal("cross-device fallback should copy, not link")
	}
}

func TestStageWorkspaceJailFileRejectsCrossDeviceCopyFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "workspace.ext4")
	if err := os.WriteFile(src, []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "jail-workspace.ext4")
	orig := hardLinkFile
	hardLinkFile = func(_, _ string) error {
		return syscall.EXDEV
	}
	t.Cleanup(func() { hardLinkFile = orig })

	err := stageWorkspaceJailFile(dst, src, os.Getuid(), os.Getgid())
	if err == nil {
		t.Fatal("expected EXDEV error")
	}
	if !strings.Contains(err.Error(), "jailer chroot base dir must be on the same filesystem as SANDBOX_HOST_MICROVM_WORKSPACE_DIR") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("workspace staging should not leave a copied file, stat err=%v", statErr)
	}
}

func TestReflinkOnlyFileFailsAcrossDevicesWithoutCopy(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm unavailable")
	}
	src := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(src, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstDir, err := os.MkdirTemp("/dev/shm", "agent-manager-reflink-*")
	if err != nil {
		t.Skipf("create tmpfs dir: %v", err)
	}
	defer os.RemoveAll(dstDir)

	// The reflink only fails when src and dst are on different filesystems; probe
	// with a hard link (same requirement) and skip when the sandbox happens to
	// place both on one device.
	probe := filepath.Join(dstDir, "probe")
	if err := os.Link(src, probe); err == nil {
		_ = os.Remove(probe)
		t.Skip("test requires src and dst on different devices")
	} else if !errors.Is(err, syscall.EXDEV) {
		t.Skipf("hard link probe did not fail with EXDEV: %v", err)
	}

	dst := filepath.Join(dstDir, "rootfs.ext4")
	if err := reflinkOnlyFile(dst, src, 0o600); err == nil {
		t.Fatal("reflinkOnlyFile must fail across devices instead of copying")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("a failed reflink must not leave a dst file, stat err = %v", err)
	}
}

func TestCopyFilePreservesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("snapshot-memory"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyFile(dst, src, 0o600); err != nil {
		t.Fatalf("copy file: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "snapshot-memory" {
		t.Fatalf("dst content = %q", string(data))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("dst mode = %o", info.Mode().Perm())
	}
}

func TestEnsureWorkspaceImageCreatesExt4Image(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	path := filepath.Join(t.TempDir(), "workspace.ext4")
	if err := ensureWorkspaceImage(context.Background(), path, 8, ""); err != nil {
		t.Fatalf("ensure workspace image: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat workspace image: %v", err)
	}
	if info.Size() != 8*1024*1024 {
		t.Fatalf("image size = %d", info.Size())
	}
	if err := ensureWorkspaceImage(context.Background(), path, 8, ""); err != nil {
		t.Fatalf("ensure existing workspace image: %v", err)
	}
}

func TestEnsureWorkspaceImageSeedsFromExistingWorkspace(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs unavailable")
	}
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "artifacts", "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "artifacts", "images", "seed.txt"), []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workspace.ext4")
	if err := ensureWorkspaceImage(context.Background(), path, 8, seed); err != nil {
		t.Fatalf("ensure seeded workspace image: %v", err)
	}
	out, err := exec.Command("debugfs", "-R", "cat /artifacts/images/seed.txt", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read seeded artifact: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); !strings.Contains(got, "artifact") {
		t.Fatalf("seeded artifact = %q", got)
	}
}

func TestPrepareWorkspaceFromPreparedCompartmentHasNoGeneratedConfig(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs unavailable")
	}
	dir := t.TempDir()
	m := &Manager{cfg: &config.Config{
		DataDir:                 dir,
		MicroVMWorkspaceDir:     filepath.Join(dir, "workspaces"),
		MicroVMWorkspaceSizeMiB: 8,
	}}
	seed := filepath.Join(m.cfg.AgentsDir(), "agent-1", "compartments", "cmp_a", "workspace")
	if err := os.MkdirAll(filepath.Join(seed, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "artifacts", "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspacePath, err := m.prepareWorkspace(context.Background(), "agent-1", "cmp_a")
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if _, ok, err := debugfsReadFile(context.Background(), workspacePath, "/config/runtime.json"); err != nil {
		t.Fatalf("read runtime config: %v", err)
	} else if ok {
		t.Fatal("fresh prepared workspace image contains generated runtime config")
	}
	if _, ok, err := debugfsReadFile(context.Background(), workspacePath, "/config/auth.json"); err != nil {
		t.Fatalf("read auth config: %v", err)
	} else if ok {
		t.Fatal("fresh prepared workspace image contains generated auth config")
	}
}

func TestEnsureWorkspaceImageLeavesExistingImageUntouched(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.ext4")
	if err := ensureWorkspaceImage(context.Background(), path, 8, ""); err != nil {
		t.Fatalf("ensure blank workspace image: %v", err)
	}
	seed := filepath.Join(dir, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "config", "runtime.json"), []byte(`{"model":"openai:test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceImage(context.Background(), path, 8, seed); err != nil {
		t.Fatalf("ensure existing workspace image: %v", err)
	}
	if _, ok, err := debugfsReadFile(context.Background(), path, "/config/runtime.json"); err != nil {
		t.Fatalf("read runtime config: %v", err)
	} else if ok {
		t.Fatal("existing workspace image was mutated with seed config")
	}
}

func TestEnsureWorkspaceImageDoesNotSyncConfigIntoExistingWorkspace(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs unavailable")
	}
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "config", "runtime.json"), []byte(`{"model":"openai:old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "artifacts", "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "artifacts", "images", "kept.txt"), []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workspace.ext4")
	if err := ensureWorkspaceImage(context.Background(), path, 8, seed); err != nil {
		t.Fatalf("ensure seeded workspace image: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seed, "config", "runtime.json"), []byte(`{"model":"openai:new"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceImage(context.Background(), path, 8, seed); err != nil {
		t.Fatalf("ensure existing workspace image: %v", err)
	}
	out, err := exec.Command("debugfs", "-R", "cat /config/runtime.json", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read original runtime.json: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); !strings.Contains(got, `{"model":"openai:old"}`) {
		t.Fatalf("runtime.json was unexpectedly synced from seed = %q", got)
	}
	out, err = exec.Command("debugfs", "-R", "cat /artifacts/images/kept.txt", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read preserved artifact: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); !strings.Contains(got, "artifact") {
		t.Fatalf("preserved artifact = %q", got)
	}
}

func TestNewFailsClosedWhenFirecrackerMissing(t *testing.T) {
	_, err := New(&config.Config{
		FirecrackerPath:         filepath.Join(t.TempDir(), "missing-firecracker"),
		MicroVMKernelPath:       "/missing/kernel",
		MicroVMRootfsPath:       "/missing/rootfs",
		MicroVMWorkspaceDir:     t.TempDir(),
		MicroVMRunDir:           t.TempDir(),
		MicroVMLogDir:           t.TempDir(),
		MicroVMAllowUnjailed:    true,
		MicroVMMemoryMiB:        128,
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "firecracker") {
		t.Fatalf("expected missing firecracker error, got %v", err)
	}
}

func TestEnsureFirecrackerVersionAcceptsPinnedVersion(t *testing.T) {
	fc := writeFakeFirecrackerVersion(t, "Firecracker v1.16.1")
	if err := ensureFirecrackerVersion(fc); err != nil {
		t.Fatalf("ensureFirecrackerVersion: %v", err)
	}
}

func TestEnsureFirecrackerVersionRejectsMismatch(t *testing.T) {
	fc := writeFakeFirecrackerVersion(t, "Firecracker v1.16.2")
	err := ensureFirecrackerVersion(fc)
	if err == nil || !strings.Contains(err.Error(), "does not match pinned version 1.16.1") {
		t.Fatalf("expected pinned-version mismatch, got %v", err)
	}
}

func TestEnsureFirecrackerVersionRejectsUnparseableOutput(t *testing.T) {
	fc := writeFakeFirecrackerVersion(t, "Firecracker dev-build")
	err := ensureFirecrackerVersion(fc)
	if err == nil || !strings.Contains(err.Error(), "parse firecracker --version output") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func writeFakeFirecrackerVersion(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' "+shellQuote(output)+"\n"), 0o700); err != nil {
		t.Fatalf("write fake firecracker: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func TestLogsReturnsTailFromStoppedInstanceLog(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	instanceID := "fc-agent-abc123"
	if err := os.WriteFile(filepath.Join(logDir, instanceID+".log"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{MicroVMLogDir: logDir}, instances: map[string]*instance{}}
	rc, err := m.Logs(context.Background(), instanceID, "2")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(data) != "two\nthree\n" {
		t.Fatalf("tail = %q", string(data))
	}
}

func TestPruneLogsRemovesOnlyExpiredLogFiles(t *testing.T) {
	logDir := t.TempDir()
	now := time.Now().UTC()
	oldPath := filepath.Join(logDir, "fc-old.log")
	newPath := filepath.Join(logDir, "fc-new.log")
	otherPath := filepath.Join(logDir, "keep.txt")
	for _, path := range []string{oldPath, newPath, otherPath} {
		if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldPath, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &config.Config{MicroVMLogDir: logDir}}
	deleted, err := m.pruneLogs(now, 7*24*time.Hour)
	if err != nil || deleted != 1 {
		t.Fatalf("prune logs: deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old log still exists: %v", err)
	}
	for _, path := range []string{newPath, otherPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file %s: %v", path, err)
		}
	}
}

func TestManagerUsesGuestControlForLiveVerbs(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	handshakes := make(chan string, 16)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	instanceID := "fc-agent-control"
	m := &Manager{
		cfg: &config.Config{MicroVMLogDir: t.TempDir()},
		instances: map[string]*instance{
			instanceID: {id: instanceID, agentID: "agent-control", vsockUDS: socketPath},
		},
	}
	ctx := context.Background()
	running, err := m.IsRunning(ctx, instanceID)
	if err != nil {
		t.Fatalf("is running: %v", err)
	}
	if !running {
		t.Fatalf("expected guest heartbeat to mark instance running")
	}
	logs, err := m.Logs(ctx, instanceID, "10")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	data, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(data) != "guest-log\n" {
		t.Fatalf("logs = %q", string(data))
	}
	if err := m.ApplySecrets(ctx, instanceID, SecretBundle{Env: map[string]string{"TOKEN": "secret"}}); err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	if _, err := m.ListWorkspace(ctx, instanceID, "artifacts"); err != nil {
		t.Fatalf("list workspace: %v", err)
	}
	if _, err := m.ReadWorkspaceFile(ctx, instanceID, "artifacts/report.txt", 1024); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if _, err := m.FreezeWorkspace(ctx, instanceID); err != nil {
		t.Fatalf("freeze workspace: %v", err)
	}
	if _, err := m.ThawWorkspace(ctx, instanceID); err != nil {
		t.Fatalf("thaw workspace: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < 7; i++ {
		select {
		case <-handshakes:
		case <-deadline:
			t.Fatalf("timed out waiting for control handshakes")
		}
	}
}

type fakeSourceBindingRegistrar struct {
	registered   []SourceBinding
	unregistered []SourceBinding
	sourceToken  string
	err          error
}

func (f *fakeSourceBindingRegistrar) Register(_ context.Context, binding SourceBinding) (SourceBindingRegistration, error) {
	if f.err != nil {
		return SourceBindingRegistration{}, f.err
	}
	f.registered = append(f.registered, binding)
	if f.sourceToken == "" {
		f.sourceToken = "opaque-source-token"
	}
	return SourceBindingRegistration{SourceToken: f.sourceToken}, nil
}

func (f *fakeSourceBindingRegistrar) Unregister(_ context.Context, binding SourceBinding) error {
	if f.err != nil {
		return f.err
	}
	f.unregistered = append(f.unregistered, binding)
	return nil
}

type fakeHostEgressRouter struct {
	registered   []TransparentRoute
	unregistered []string
	err          error
}

func (f *fakeHostEgressRouter) RegisterTransparentRoute(_ context.Context, route TransparentRoute) error {
	if f.err != nil {
		return f.err
	}
	f.registered = append(f.registered, route)
	return nil
}

func (f *fakeHostEgressRouter) UnregisterContainer(instanceID string) {
	f.unregistered = append(f.unregistered, instanceID)
}

func TestReserveGuestIPAllocatesDistinctAddresses(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"}, guestIPs: map[string]string{}}
	a, err := m.reserveGuestIP("fc-a")
	if err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	b, err := m.reserveGuestIP("fc-b")
	if err != nil {
		t.Fatalf("reserve b: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct IPs, got %q and %q", a, b)
	}
	// gateway (.1) must never be allocated.
	if a == "10.0.0.1" || b == "10.0.0.1" {
		t.Fatalf("allocated the gateway address: a=%q b=%q", a, b)
	}
	// release frees the address for reuse.
	m.releaseGuestIP("fc-a")
	c, err := m.reserveGuestIP("fc-c")
	if err != nil {
		t.Fatalf("reserve c: %v", err)
	}
	if c != a {
		t.Fatalf("expected reused address %q, got %q", a, c)
	}
}

func TestReserveGuestIPAllocatesDistinctAddressesForCompartments(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMBridgeCIDR: "10.0.1.1/24"}, guestIPs: map[string]string{}}
	aID, err := newInstanceID("agent-1", "cmp_a")
	if err != nil {
		t.Fatalf("new cmp_a instance id: %v", err)
	}
	bID, err := newInstanceID("agent-1", "cmp_b")
	if err != nil {
		t.Fatalf("new cmp_b instance id: %v", err)
	}
	a, err := m.reserveGuestIP(aID)
	if err != nil {
		t.Fatalf("reserve cmp_a IP: %v", err)
	}
	b, err := m.reserveGuestIP(bID)
	if err != nil {
		t.Fatalf("reserve cmp_b IP: %v", err)
	}
	if a == b {
		t.Fatalf("compartment guest IPs collided: %s", a)
	}
}

func TestPrepareWorkspaceUsesPerCompartmentImages(t *testing.T) {
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{
		DataDir:                 root,
		MicroVMWorkspaceDir:     filepath.Join(root, "workspaces"),
		MicroVMWorkspaceSizeMiB: 8,
		MicroVMWorkspaceBackend: "ext4",
	}}
	if err := os.MkdirAll(filepath.Join(m.cfg.AgentsDir(), "agent-1", "workspace"), 0o700); err != nil {
		t.Fatalf("create seed workspace: %v", err)
	}
	a, err := m.prepareWorkspace(context.Background(), "agent-1", "cmp_a")
	if err != nil {
		t.Fatalf("prepare cmp_a workspace: %v", err)
	}
	b, err := m.prepareWorkspace(context.Background(), "agent-1", "cmp_b")
	if err != nil {
		t.Fatalf("prepare cmp_b workspace: %v", err)
	}
	if a == b {
		t.Fatalf("compartments share workspace image %q", a)
	}
	if !strings.Contains(a, filepath.Join("agent-1", "cmp_a."+workspaceName)) || !strings.Contains(b, filepath.Join("agent-1", "cmp_b."+workspaceName)) {
		t.Fatalf("workspace paths are not compartment-scoped: a=%q b=%q", a, b)
	}
}

func TestPrepareWorkspaceUsesCompartmentImagePath(t *testing.T) {
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{
		DataDir:                 root,
		MicroVMWorkspaceDir:     filepath.Join(root, "workspaces"),
		MicroVMWorkspaceSizeMiB: 8,
		MicroVMWorkspaceBackend: "ext4",
	}}
	if err := os.MkdirAll(filepath.Join(m.cfg.AgentsDir(), "agent-1", "workspace"), 0o700); err != nil {
		t.Fatalf("create seed workspace: %v", err)
	}
	path, err := m.prepareWorkspace(context.Background(), "agent-1", "cmp_a")
	if err != nil {
		t.Fatalf("prepare compartment workspace: %v", err)
	}
	want := filepath.Join(m.cfg.MicroVMWorkspaceDir, "agent-1", "cmp_a."+workspaceName)
	if path != want {
		t.Fatalf("compartment workspace path = %q, want %q", path, want)
	}
}

func TestReserveGuestIPExhaustionReturnsCleanError(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMBridgeCIDR: "10.0.0.1/30"}, guestIPs: map[string]string{}}
	if _, err := m.reserveGuestIP("fc-a"); err != nil {
		t.Fatalf("reserve first IP: %v", err)
	}
	if _, err := m.reserveGuestIP("fc-b"); err == nil || !strings.Contains(err.Error(), "no free guest IP") {
		t.Fatalf("expected guest IP exhaustion error, got %v", err)
	}
}

func TestRuntimeMetricsSnapshotReportsCountsCapacityAndP95(t *testing.T) {
	m := &Manager{
		cfg: &config.Config{
			MicroVMBridgeCIDR:          "10.0.0.1/29",
			MicroVMMaxConcurrentGlobal: 32,
			MicroVMMemoryMiB:           512,
			MicroVMMemoryBudgetMiB:     65536,
		},
		instances: map[string]*instance{
			"fc-a-1": {id: "fc-a-1", agentID: "agent-a", compartmentID: "cmp_a"},
			"fc-a-2": {id: "fc-a-2", agentID: "agent-a", compartmentID: "cmp_b"},
			"fc-b-1": {id: "fc-b-1", agentID: "agent-b", compartmentID: "cmp_c"},
		},
		guestIPs: map[string]string{
			"fc-a-1": "10.0.0.2",
			"fc-a-2": "10.0.0.3",
		},
	}
	m.recordStartDuration(10 * time.Millisecond)
	m.recordStartDuration(40 * time.Millisecond)
	m.recordStartDuration(20 * time.Millisecond)

	got := m.RuntimeMetricsSnapshot()
	if got.ConcurrentVMsByAgent["agent-a"] != 2 || got.ConcurrentVMsByAgent["agent-b"] != 1 {
		t.Fatalf("concurrent counts = %+v", got.ConcurrentVMsByAgent)
	}
	if got.GuestIPsInUse != 2 || got.GuestIPCapacity != 5 {
		t.Fatalf("guest IP metrics = used %d capacity %d", got.GuestIPsInUse, got.GuestIPCapacity)
	}
	if got.ConcurrentVMsTotal != 3 || got.MaxConcurrentGlobal != 32 || got.MemoryReservedMiB != 1536 || got.MemoryBudgetMiB != 65536 {
		t.Fatalf("global metrics = total %d cap %d reserved %d budget %d", got.ConcurrentVMsTotal, got.MaxConcurrentGlobal, got.MemoryReservedMiB, got.MemoryBudgetMiB)
	}
	if got.ColdStartCount != 3 || got.ColdStartP95 != 40*time.Millisecond {
		t.Fatalf("cold start metrics = count %d p95 %s", got.ColdStartCount, got.ColdStartP95)
	}
}

func TestAdmitCompartmentSpawnLocked(t *testing.T) {
	live := func(id, agent string, reaping bool) *instance {
		return &instance{id: id, agentID: agent, compartmentID: id, reaping: reaping}
	}
	tests := []struct {
		name      string
		cfg       config.Config
		instances map[string]*instance
		wantErr   string
	}{
		{
			name:      "per-agent cap",
			cfg:       config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentPerAgent: 1},
			instances: map[string]*instance{"a": live("cmp-a", "agent-1", false)},
			wantErr:   "SANDBOX_HOST_MICROVM_MAX_CONCURRENT_PER_AGENT",
		},
		{
			name: "global cap across agents",
			cfg:  config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentGlobal: 2},
			instances: map[string]*instance{
				"a": live("cmp-a", "agent-1", false),
				"b": live("cmp-b", "agent-2", false),
			},
			wantErr: "SANDBOX_HOST_MICROVM_MAX_CONCURRENT_GLOBAL",
		},
		{
			name: "memory budget",
			cfg:  config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMemoryMiB: 2048, MicroVMMemoryBudgetMiB: 4096},
			instances: map[string]*instance{
				"a": live("cmp-a", "agent-1", false),
				"b": live("cmp-b", "agent-2", false),
			},
			wantErr: "SANDBOX_HOST_MICROVM_MEMORY_BUDGET_MIB",
		},
		{
			name:      "all zero is unlimited",
			cfg:       config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"},
			instances: map[string]*instance{"a": live("cmp-a", "agent-2", false)},
		},
		{
			name:      "reaping instances retain capacity",
			cfg:       config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentPerAgent: 1, MicroVMMaxConcurrentGlobal: 1, MicroVMMemoryMiB: 2048, MicroVMMemoryBudgetMiB: 2048},
			instances: map[string]*instance{"a": live("cmp-a", "agent-1", true)},
			wantErr:   "SANDBOX_HOST_MICROVM_MAX_CONCURRENT_PER_AGENT",
		},
		{
			name:      "non-positive vm memory skips memory check",
			cfg:       config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMemoryMiB: 0, MicroVMMemoryBudgetMiB: 1},
			instances: map[string]*instance{"a": live("cmp-a", "agent-2", false)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{cfg: &tt.cfg, instances: tt.instances}
			err := m.admitCompartmentSpawnLocked(runtimeInstanceKey{agentID: "agent-1", compartmentID: "cmp-new"})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("admit: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("admit error = %v, want %s", err, tt.wantErr)
			}
		})
	}
}

func TestAdmitCompartmentSpawnLockedCountsPendingReservations(t *testing.T) {
	m := &Manager{
		cfg:           &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentGlobal: 2, MicroVMMemoryMiB: 1024, MicroVMMemoryBudgetMiB: 2048},
		instances:     map[string]*instance{},
		pendingSpawns: map[runtimeInstanceKey]int{{agentID: "agent-1", compartmentID: "cmp-a"}: 2},
	}
	if err := m.admitCompartmentSpawnLocked(runtimeInstanceKey{agentID: "agent-2", compartmentID: "cmp-b"}); err == nil || !strings.Contains(err.Error(), "SANDBOX_HOST_MICROVM_MAX_CONCURRENT_GLOBAL") {
		t.Fatalf("admit with pending reservations = %v, want global cap", err)
	}
	got := m.RuntimeMetricsSnapshot()
	if got.PendingVMsTotal != 2 || got.PendingVMsByAgent["agent-1"] != 2 || got.MemoryReservedMiB != 2048 {
		t.Fatalf("pending metrics = %+v", got)
	}
}

func TestRegisterStartingInstanceTransfersPendingCapacityToLive(t *testing.T) {
	key := runtimeInstanceKey{agentID: "agent-1", compartmentID: "cmp-a"}
	m := &Manager{
		cfg:           &config.Config{MicroVMMemoryMiB: 1024},
		instances:     map[string]*instance{},
		pendingSpawns: map[runtimeInstanceKey]int{key: 1},
	}
	m.registerStartingInstance(&instance{id: "fc-1", agentID: key.agentID, compartmentID: key.compartmentID}, func() {
		m.releaseCompartmentSpawnLocked(key)
	})
	got := m.RuntimeMetricsSnapshot()
	if got.ConcurrentVMsTotal != 1 || got.PendingVMsTotal != 0 || got.MemoryReservedMiB != 1024 {
		t.Fatalf("capacity after registration = %+v", got)
	}
}

func TestRegisterStartingInstanceTransfersCapacityAtomically(t *testing.T) {
	key := runtimeInstanceKey{agentID: "agent-1", compartmentID: "cmp-a"}
	m := &Manager{
		cfg:           &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentGlobal: 2},
		instances:     map[string]*instance{},
		pendingSpawns: map[runtimeInstanceKey]int{key: 1},
	}
	transferEntered := make(chan struct{})
	allowTransfer := make(chan struct{})
	registered := make(chan struct{})
	go func() {
		m.registerStartingInstance(&instance{id: "fc-1", agentID: key.agentID, compartmentID: key.compartmentID}, func() {
			m.releaseCompartmentSpawnLocked(key)
			close(transferEntered)
			<-allowTransfer
		})
		close(registered)
	}()
	<-transferEntered

	admitted := make(chan error, 1)
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		admitted <- m.admitCompartmentSpawnLocked(runtimeInstanceKey{agentID: "agent-2", compartmentID: "cmp-b"})
	}()
	select {
	case err := <-admitted:
		t.Fatalf("concurrent admission observed an in-progress transfer: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(allowTransfer)
	<-registered
	if err := <-admitted; err != nil {
		t.Fatalf("admission after atomic transfer: %v", err)
	}
}

func TestCreateAndStartAtomicAdmissionBurst(t *testing.T) {
	const cap = 2
	launchEntered := make(chan struct{}, 10)
	releaseLaunches := make(chan struct{})
	var launched atomic.Int64
	m := &Manager{
		cfg:           &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentGlobal: cap},
		instances:     map[string]*instance{},
		pendingSpawns: map[runtimeInstanceKey]int{},
	}
	m.startCompartment = func(ctx context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		launched.Add(1)
		launchEntered <- struct{}{}
		select {
		case <-releaseLaunches:
			return "fc-" + compartmentID, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	const attempts = 12
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			_, err := m.createAndStart(context.Background(), fmt.Sprintf("agent-%d", i), runtimemanager.StartOpts{CompartmentID: fmt.Sprintf("cmp-%d", i)})
			results <- err
		}(i)
	}
	for i := 0; i < cap; i++ {
		select {
		case <-launchEntered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for admitted cold start")
		}
	}
	denied := 0
	for denied < attempts-cap {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("cold start completed before launcher was released")
			}
			denied++
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for denied cold starts; got %d", denied)
		}
	}
	if got := launched.Load(); got != cap {
		t.Fatalf("launches reached launcher = %d, want exactly %d", got, cap)
	}
	metrics := m.RuntimeMetricsSnapshot()
	if metrics.PendingVMsTotal != cap {
		t.Fatalf("pending reservations = %d, want %d", metrics.PendingVMsTotal, cap)
	}
	close(releaseLaunches)
	for i := 0; i < cap; i++ {
		if err := <-results; err != nil {
			t.Fatalf("admitted cold start failed: %v", err)
		}
	}
	if denied != attempts-cap {
		t.Fatalf("denied starts = %d, want %d", denied, attempts-cap)
	}
	if got := m.RuntimeMetricsSnapshot().PendingVMsTotal; got != 0 {
		t.Fatalf("pending reservations after completion = %d, want 0", got)
	}
}

func TestCreateAndStartReleasesAdmissionReservationOnFailureAndCancellation(t *testing.T) {
	m := &Manager{
		cfg:           &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMMaxConcurrentGlobal: 1},
		instances:     map[string]*instance{},
		pendingSpawns: map[runtimeInstanceKey]int{},
	}
	m.startCompartment = func(context.Context, string, string, runtimemanager.StartOpts) (string, error) {
		return "", errors.New("launch failed")
	}
	if _, err := m.createAndStart(context.Background(), "agent-failure", runtimemanager.StartOpts{CompartmentID: "cmp-failure"}); err == nil {
		t.Fatal("expected launch failure")
	}
	if got := m.RuntimeMetricsSnapshot().PendingVMsTotal; got != 0 {
		t.Fatalf("pending after failure = %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.startCompartment = func(ctx context.Context, _ string, _ string, _ runtimemanager.StartOpts) (string, error) {
		return "", ctx.Err()
	}
	if _, err := m.createAndStart(ctx, "agent-cancel", runtimemanager.StartOpts{CompartmentID: "cmp-cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled launch = %v", err)
	}
	if got := m.RuntimeMetricsSnapshot().PendingVMsTotal; got != 0 {
		t.Fatalf("pending after cancellation = %d", got)
	}
}

func TestMicroVMGuestIPCapacitySingleFallback(t *testing.T) {
	if got := microVMGuestIPCapacity(&config.Config{MicroVMGuestIP: "172.30.0.10"}); got != 1 {
		t.Fatalf("capacity = %d, want 1", got)
	}
}

func TestBuildFirecrackerConfigAddsGuestIPBootArg(t *testing.T) {
	cfg := &config.Config{MicroVMKernelPath: "/vmlinux", MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMVCPUs: 1, MicroVMMemoryMiB: 512}
	got := buildFirecrackerConfig(cfg, cfg.MicroVMKernelPath, "/rootfs", "/workspace", cfg.MicroVMSharedImagePath, "/vsock", "agfc1", "10.0.0.7")
	if !strings.Contains(got.BootSource.BootArgs, "ip=10.0.0.7::10.0.0.1:255.255.255.0::eth0:off") {
		t.Fatalf("boot args missing per-VM ip=: %q", got.BootSource.BootArgs)
	}
	if !strings.Contains(got.BootSource.BootArgs, "noxsave") {
		t.Fatalf("boot args missing noxsave: %q", got.BootSource.BootArgs)
	}
}

func TestBuildStartupSecretBundleCarriesScopedToolEnv(t *testing.T) {
	t.Setenv("MOM_BROWSER_HEADLESS", "false")
	t.Setenv("MOM_BROWSER_PRESTART", "true")

	m := &Manager{cfg: &config.Config{PlatformAPIURL: "https://platform.example/"}}
	bundle, err := m.buildStartupSecretBundle("agent-9", "fc-agent-9-cmp-a-1", runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_a"})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	if bundle.Env["PLATFORM_API_URL"] != "https://platform.example/" {
		t.Fatalf("PLATFORM_API_URL = %q", bundle.Env["PLATFORM_API_URL"])
	}
	if bundle.Env["AGENT_ID"] != "agent-9" {
		t.Fatalf("AGENT_ID = %q", bundle.Env["AGENT_ID"])
	}
	if bundle.Env["AGENT_MANAGER_COMPARTMENT_ID"] != "cmp_a" {
		t.Fatalf("AGENT_MANAGER_COMPARTMENT_ID = %q", bundle.Env["AGENT_MANAGER_COMPARTMENT_ID"])
	}
	if bundle.Env["AGENT_MANAGER_RUNTIME_CREDENTIAL_ID"] != "agent-9:cmp_a:fc-agent-9-cmp-a-1" {
		t.Fatalf("AGENT_MANAGER_RUNTIME_CREDENTIAL_ID = %q", bundle.Env["AGENT_MANAGER_RUNTIME_CREDENTIAL_ID"])
	}
	if got := bundle.Env["AGENT_PLATFORM_TOKEN"]; got != "" {
		t.Fatalf("AGENT_PLATFORM_TOKEN must not be injected into persistent runtime env, got %q", got)
	}
	for _, key := range []string{"AGENT_MANAGER_SESSION_STORE_URL", "AGENT_MANAGER_SESSION_STORE_TOKEN", "AGENT_MANAGER_RUNTIME_TOKEN"} {
		if got := bundle.Env[key]; got != "" {
			t.Fatalf("%s must not be injected into the tool runtime, got %q", key, got)
		}
	}
	if _, ok := bundle.Env["MOM_BROWSER_HEADLESS"]; ok {
		t.Fatal("MOM_BROWSER_HEADLESS must not be injected into tool runtime env")
	}
	if _, ok := bundle.Env["MOM_BROWSER_PRESTART"]; ok {
		t.Fatal("MOM_BROWSER_PRESTART must not be injected into tool runtime env")
	}
}

func TestBuildStartupSecretBundleCarriesRuntimeContextProjection(t *testing.T) {
	m := &Manager{cfg: &config.Config{PlatformAPIURL: "https://platform.example/"}}
	bundle, err := m.buildStartupSecretBundle("agent-9", "fc-agent-9-cmp-a-1", runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_a",
		RuntimeContextProjection: runtimecontext.Projection{
			Files: []runtimecontext.File{{
				Path:    runtimecontext.PathGitHubContext,
				Content: `{"service":"github","accessibleRepos":["owner/repo"]}`,
			}},
			MigratedServicePaths:  []string{runtimecontext.PathGitHubContext},
			PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
		},
	})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	got, ok := bundle.RuntimeContextProjection.FileContent(runtimecontext.PathGitHubContext)
	if !ok || got != `{"service":"github","accessibleRepos":["owner/repo"]}` {
		t.Fatalf("github context file = %q", got)
	}
	if len(bundle.Files) != 0 {
		t.Fatalf("runtime context must not be delivered as secret files: %#v", bundle.Files)
	}
}

func TestIntegrationSourceBindingReturnsOpaqueToken(t *testing.T) {
	registrar := &fakeSourceBindingRegistrar{}
	instanceID := "fc-agent-9-cmp-a-1"
	m := &Manager{
		cfg:            &config.Config{PlatformAPIURL: "http://10.0.0.1:8081"},
		guestIPs:       map[string]string{instanceID: "10.0.0.7"},
		sourceBindings: registrar,
	}
	token, err := m.registerSourceBinding(
		context.Background(), instanceID, "environment-9", "instance-9", 2, []string{"connection-9"},
	)
	if err != nil {
		t.Fatalf("register source binding: %v", err)
	}
	if token == "" {
		t.Fatal("expected context token")
	}
	if len(registrar.registered) != 1 || registrar.registered[0].EnvironmentID != "environment-9" {
		t.Fatalf("registered binding = %#v", registrar.registered)
	}

	if token != "opaque-source-token" {
		t.Fatalf("source token = %q", token)
	}
}

func TestCreateAndStartColdCleansInstanceDirOnFailure(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	logDir := filepath.Join(root, "log")
	wsDir := filepath.Join(root, "workspace")
	for _, d := range []string{runDir, logDir, wsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{
		cfg: &config.Config{
			MicroVMRunDir:       runDir,
			MicroVMLogDir:       logDir,
			MicroVMWorkspaceDir: wsDir,
			MicroVMRootfsPath:   filepath.Join(root, "missing-rootfs.ext4"),
			MicroVMBridgeName:   "agbr0",
			MicroVMBridgeCIDR:   "10.0.0.1/24",
		},
		instances: map[string]*instance{},
		guestIPs:  map[string]string{},
	}
	network := &recordingHostNetworkConfigurer{}
	m.network = network

	// The rootfs source does not exist, so the cold start fails at "prepare
	// rootfs" — after the per-instance dir and the guest IP have been allocated.
	_, err := m.createAndStartCold(context.Background(), "agent-1", "cmp_a", runtimemanager.StartOpts{}, nil)
	if err == nil {
		t.Fatal("expected createAndStartCold to fail on missing rootfs")
	}
	if !strings.Contains(err.Error(), "prepare rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
	if network.tap.GuestIP != "10.0.0.2" || !ipWithinCIDR(network.tap.GuestIP, network.tap.BridgeCIDR) {
		t.Fatalf("manager tap config = %+v, want reserved guest IP inside bridge CIDR", network.tap)
	}

	entries, readErr := os.ReadDir(runDir)
	if readErr != nil {
		t.Fatalf("read run dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("instance dir leaked on failure: %d entries remain", len(entries))
	}
	if len(m.guestIPs) != 0 {
		t.Fatalf("guest IP reservation leaked on failure: %v", m.guestIPs)
	}
}

func TestWatchJailedExitReclaimsOnNaturalExit(t *testing.T) {
	prev := jailedExitPollInterval
	jailedExitPollInterval = 10 * time.Millisecond
	defer func() { jailedExitPollInterval = prev }()

	// No real process runs with this --id, so firecrackerProcessRunning reports
	// it as gone on the first poll, exercising the natural-exit path.
	const id = "fc-agent-watch-000000000000"
	inst := &instance{id: id, agentID: "agent-watch", launcherOnly: true, done: make(chan struct{})}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*instance{id: inst},
		guestIPs:  map[string]string{id: "172.30.0.20"},
	}

	returned := make(chan struct{})
	go func() {
		m.watchJailedExit(inst)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("watchJailedExit did not return after the jailed process was absent")
	}

	m.mu.Lock()
	_, tracked := m.instances[id]
	_, ipHeld := m.guestIPs[id]
	m.mu.Unlock()
	if tracked {
		t.Fatal("instance still tracked after natural exit")
	}
	if ipHeld {
		t.Fatal("guest IP not released after natural exit")
	}
	select {
	case <-inst.done:
	default:
		t.Fatal("done channel not closed after natural exit")
	}
}

func TestFinishInstanceRetainsGuestIdentityUntilTapCleanupCompletes(t *testing.T) {
	const (
		oldID = "fc-agent-recycle-old"
		newID = "fc-agent-recycle-new"
		oldIP = "10.0.0.2"
	)
	network := &blockingHostNetworkConfigurer{
		removeStarted: make(chan struct{}),
		allowRemove:   make(chan struct{}),
	}
	defer func() {
		select {
		case <-network.allowRemove:
		default:
			close(network.allowRemove)
		}
	}()
	inst := &instance{
		id:      oldID,
		tapName: "agfc-recycle",
		done:    make(chan struct{}),
	}
	m := &Manager{
		cfg:       &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"},
		instances: map[string]*instance{oldID: inst},
		guestIPs:  map[string]string{oldID: oldIP},
		network:   network,
	}

	go m.finishInstance(inst)
	select {
	case <-network.removeStarted:
	case <-time.After(time.Second):
		t.Fatal("tap cleanup did not start")
	}

	newIP, err := m.reserveGuestIP(newID)
	if err != nil {
		t.Fatalf("reserve successor guest IP: %v", err)
	}
	if newIP == oldIP {
		t.Fatalf("recycled guest identity %s before predecessor tap cleanup completed", oldIP)
	}
	m.releaseGuestIP(newID)

	close(network.allowRemove)
	select {
	case <-inst.done:
	case <-time.After(time.Second):
		t.Fatal("instance cleanup did not finish")
	}
	recycledIP, err := m.reserveGuestIP(newID)
	if err != nil {
		t.Fatalf("reserve reclaimed guest IP: %v", err)
	}
	if recycledIP != oldIP {
		t.Fatalf("guest identity after completed cleanup = %s, want %s", recycledIP, oldIP)
	}
}

func TestFinishInstanceRetainsGuestIdentityWhenTapCleanupFails(t *testing.T) {
	const (
		oldID = "fc-agent-fail-old"
		newID = "fc-agent-fail-new"
		oldIP = "10.0.0.2"
	)
	network := &failingHostNetworkConfigurer{}
	inst := &instance{id: oldID, tapName: "agfc-fail", done: make(chan struct{})}
	m := &Manager{
		cfg:       &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"},
		instances: map[string]*instance{oldID: inst},
		guestIPs:  map[string]string{oldID: oldIP},
		network:   network,
	}

	m.finishInstance(inst)
	select {
	case <-inst.done:
	case <-time.After(time.Second):
		t.Fatal("instance cleanup did not finish")
	}

	// The launcher tap removal failed, so its state may still claim the guest
	// identity. finishInstance must retain the reservation fail-closed so a
	// successor cannot recycle a still-claimed IP into an ownership conflict.
	newIP, err := m.reserveGuestIP(newID)
	if err != nil {
		t.Fatalf("reserve successor guest IP: %v", err)
	}
	if newIP == oldIP {
		t.Fatalf("recycled guest identity %s despite failed tap cleanup", oldIP)
	}
	if network.removeCalls < 2 {
		t.Fatalf("expected bounded retry of tap removal, got %d call(s)", network.removeCalls)
	}
}

func TestWatchJailedExitDefersToExplicitStop(t *testing.T) {
	prev := jailedExitPollInterval
	jailedExitPollInterval = time.Hour // never fires; only the closed done cancels
	defer func() { jailedExitPollInterval = prev }()

	const id = "fc-agent-cancel-000000000000"
	inst := &instance{id: id, agentID: "agent-cancel", launcherOnly: true, done: make(chan struct{})}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*instance{id: inst},
		guestIPs:  map[string]string{id: "172.30.0.21"},
	}
	close(inst.done) // an explicit stopInstance has already reclaimed this instance

	returned := make(chan struct{})
	go func() {
		m.watchJailedExit(inst)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("watchJailedExit did not return when done was closed")
	}

	// The watcher must not re-run reclamation; that is stopInstance's job.
	m.mu.Lock()
	_, ipHeld := m.guestIPs[id]
	m.mu.Unlock()
	if !ipHeld {
		t.Fatal("watcher reclaimed on the cancel path; should defer to stopInstance")
	}
}
