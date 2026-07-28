package microvm

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-manager/internal/harness"
	"agent-manager/internal/runtimemanager"
)

func TestLauncherOutputBufferPreservesModelVisibleInventory(t *testing.T) {
	buf := newLauncherOutputBuffer()
	if _, err := buf.Write([]byte(`model_visible_runtime_inventory={"toolNames":["bash"],"handNames":["platform_sandbox"]}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write([]byte(strings.Repeat("x", maxLauncherHarnessOutputBytes+128))); err != nil {
		t.Fatal(err)
	}
	if inventory := buf.String(); !strings.Contains(inventory, `"toolNames":["bash"]`) {
		t.Fatalf("inventory line was evicted from bounded launcher output: %q", inventory[:min(len(inventory), 256)])
	}
}

func testPrivilegedLauncherConfig(t *testing.T) PrivilegedLauncherConfig {
	t.Helper()
	managerUID := os.Getuid()
	managerGID := os.Getgid()
	if managerUID == 0 {
		managerUID = 12345
	}
	if managerGID == 0 {
		managerGID = 12345
	}
	dir := t.TempDir()
	artifactRoot := filepath.Join(dir, "artifacts")
	for _, path := range []string{artifactRoot, filepath.Join(dir, "workspaces"), filepath.Join(dir, "run"), filepath.Join(dir, "logs"), filepath.Join(dir, "jailer"), filepath.Join(dir, "state")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fc := filepath.Join(dir, "firecracker")
	if err := os.WriteFile(fc, []byte("#!/bin/sh\necho 'Firecracker v1.16.1'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	jailer := filepath.Join(dir, "jailer")
	if err := os.RemoveAll(jailer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jailer, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(artifactRoot, "release", kernelName)
	if err := os.MkdirAll(filepath.Dir(kernel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	return PrivilegedLauncherConfig{
		SocketPath:             filepath.Join(dir, "launcher.sock"),
		SocketGID:              managerGID,
		AllowedUID:             managerUID,
		ManagerGID:             managerGID,
		FirecrackerPath:        fc,
		JailerPath:             jailer,
		ArtifactRoot:           artifactRoot,
		KernelPath:             kernel,
		WorkspaceRoot:          filepath.Join(dir, "workspaces"),
		RunRoot:                filepath.Join(dir, "run"),
		LogRoot:                filepath.Join(dir, "logs"),
		JailRoot:               filepath.Join(dir, "jailer-root"),
		StateRoot:              filepath.Join(dir, "state"),
		BridgeName:             "agfc0",
		BridgeCIDR:             "172.30.0.1/24",
		TapPrefix:              "agfc",
		JailerUID:              1234,
		JailerGID:              1234,
		JailerCgroupVersion:    2,
		JailerParentCgroup:     "agent-manager",
		MemoryMiB:              2048,
		VCPUs:                  2,
		WorkspaceSizeMiB:       8192,
		CPUTemplate:            "None",
		TransparentHTTPPort:    18080,
		HarnessCIDR:            "169.254.77.0/24",
		HarnessProxyIP:         "172.30.0.1",
		HarnessPlatformIP:      "172.30.0.1",
		HarnessBubblewrap:      "/bin/true",
		HarnessIPCommand:       "/bin/true",
		HarnessSystemdRun:      "/bin/true",
		HarnessSystemctl:       "/bin/true",
		HarnessShell:           "/bin/sh",
		HarnessEnvCommand:      "/usr/bin/env",
		NftPath:                "/bin/true",
		HarnessResultRoot:      filepath.Join(dir, "harness-results"),
		HarnessMemoryBytes:     2 * 1024 * 1024 * 1024,
		HarnessNanoCPUs:        2_000_000_000,
		HarnessPidsLimit:       256,
		HarnessMaxRuntime:      10 * time.Minute,
		HarnessIdleTimeout:     5 * time.Minute,
		allowUnprivilegedTests: true,
	}
}

func TestOpenLauncherLogGrantsManagerGroupReadAccess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "instance.log")
	if err := os.WriteFile(logPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	managerGID := os.Getgid()
	logFile, err := openLauncherLog(logPath, managerGID)
	if err != nil {
		t.Fatalf("open launcher log: %v", err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected stat payload %T", info.Sys())
	}
	if got := int(stat.Gid); got != managerGID {
		t.Fatalf("launcher log gid = %d, want manager gid %d", got, managerGID)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("launcher log mode = %04o, want 0640", got)
	}
}

func TestManagerSocketAliasKeepsJailedSocketAddressShort(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	runtimeDir, err := os.MkdirTemp("/tmp", "ag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	cfg.SocketPath = filepath.Join(runtimeDir, "launcher.sock")
	server := &PrivilegedLauncherServer{cfg: cfg}
	instanceID := "fc-agent-with-a-very-long-identity-cmp-with-a-very-long-compartment-12345678"
	target := filepath.Join(t.TempDir(), "guest.vsock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	alias, err := server.installManagerSocketAlias(instanceID, target, "vsock")
	if err != nil {
		t.Fatalf("install manager socket alias: %v", err)
	}
	defer server.removeManagerSocketAliases(instanceID)
	if len(alias) >= 108 {
		t.Fatalf("manager socket alias exceeds Linux sockaddr_un limit: %q (%d bytes)", alias, len(alias))
	}
	conn, err := net.Dial("unix", alias)
	if err != nil {
		t.Fatalf("connect through manager socket alias: %v", err)
	}
	_ = conn.Close()
	info, err := os.Lstat(alias)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("manager socket alias mode = %s, want symlink", info.Mode())
	}
}

func TestPrivilegedLauncherValidatesDerivedPathsAndSymlinks(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-12345678"
	agentID := "agent-1"
	compartmentID := "cmp-1"
	runRootfs := filepath.Join(cfg.RunRoot, instanceID, rootfsName)
	workspace := filepath.Join(cfg.WorkspaceRoot, agentID, compartmentID+"."+workspaceName)
	rootfsImage := filepath.Join(cfg.ArtifactRoot, "release", rootfsName)
	shared := filepath.Join(cfg.ArtifactRoot, "release", sharedImageName)
	for _, path := range []string{runRootfs, workspace, rootfsImage, shared} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := privilegedLaunchRequest{
		InstanceID: instanceID, AgentID: agentID, CompartmentID: compartmentID,
		RootfsPath: runRootfs, RootfsImage: rootfsImage, WorkspacePath: workspace,
		SharedImage: shared, TapName: tapNameForInstance(cfg.TapPrefix, instanceID), GuestIP: "172.30.0.2",
		SandboxPolicy: &runtimemanager.SandboxRuntimePolicy{VCPUs: 1, MemoryMiB: 512, WorkspaceSizeMiB: 1024, ProcessLimit: 32, WorkspaceWritable: true, SharedReadOnly: true},
	}
	if err := server.validateLaunchRequest(req); err != nil {
		t.Fatalf("valid launch request: %v", err)
	}
	req.SandboxPolicy.MemoryMiB = cfg.MemoryMiB + 1
	if err := server.validateLaunchRequest(req); err == nil || !strings.Contains(err.Error(), "launcher maxima") {
		t.Fatalf("oversized sandbox policy error = %v", err)
	}
	req.SandboxPolicy.MemoryMiB = 512

	escape := filepath.Join(t.TempDir(), "escape.ext4")
	if err := os.WriteFile(escape, []byte("escape"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(cfg.ArtifactRoot, "release", "escape.ext4")
	if err := os.Symlink(escape, symlink); err != nil {
		t.Fatal(err)
	}
	req.SharedImage = symlink
	if err := server.validateLaunchRequest(req); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape error = %v", err)
	}
	req.SharedImage = shared
	req.WorkspacePath = filepath.Join(cfg.WorkspaceRoot, agentID, "other."+workspaceName)
	if err := server.validateLaunchRequest(req); err == nil || !strings.Contains(err.Error(), "derived identity") {
		t.Fatalf("mismatched workspace error = %v", err)
	}
}

func TestPrivilegedLauncherRejectsRootManagerIdentity(t *testing.T) {
	for _, mutate := range []func(*PrivilegedLauncherConfig){
		func(cfg *PrivilegedLauncherConfig) { cfg.AllowedUID = 0 },
		func(cfg *PrivilegedLauncherConfig) { cfg.ManagerGID = 0 },
		func(cfg *PrivilegedLauncherConfig) { cfg.SocketGID = 0 },
	} {
		cfg := testPrivilegedLauncherConfig(t)
		mutate(&cfg)
		if _, err := NewPrivilegedLauncherServer(cfg); err == nil || !strings.Contains(err.Error(), "non-root") {
			t.Fatalf("root manager identity error = %v", err)
		}
	}
}

func TestPrivilegedLauncherRejectsSymlinkedHarnessResultRoot(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.HarnessResultRoot); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPrivilegedLauncherServer(cfg); err == nil || !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("symlinked harness result root error = %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("symlink target mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestPrivilegedLauncherRejectsUntrackedHostMutations(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	var commands int
	server.network.run = func(context.Context, string, ...string) ([]byte, error) {
		commands++
		return nil, nil
	}
	bad := TapConfig{
		InstanceID: "fc-agent-12345678", AgentID: "agent-1", TapName: "eth0",
		BridgeName: cfg.BridgeName, BridgeCIDR: cfg.BridgeCIDR, OwnerUID: cfg.JailerUID,
	}
	resp := server.handle(context.Background(), privilegedLauncherRequest{Version: privilegedLauncherProtocolVersion, Op: "configure_tap", Tap: &bad})
	if resp.OK || !strings.Contains(resp.Error, "policy") {
		t.Fatalf("invalid tap response = %+v", resp)
	}
	if commands != 0 {
		t.Fatalf("invalid request executed %d host commands", commands)
	}
	resp = server.handle(context.Background(), privilegedLauncherRequest{Version: privilegedLauncherProtocolVersion, Op: "remove_tap", TapName: "lo"})
	if resp.OK || commands != 0 {
		t.Fatalf("untracked tap removal response=%+v commands=%d", resp, commands)
	}
}

func TestPrivilegedLauncherTracksTapBeforeMutationAndRetainsFailedCleanup(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-tap-crash"
	tapName := tapNameForInstance(cfg.TapPrefix, instanceID)
	sawPersistedIntent := false
	server.network.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		if strings.Contains(command, "tuntap add dev "+tapName) {
			state, readErr := server.readState(instanceID)
			if readErr != nil || state.TapName != tapName {
				t.Fatalf("tap mutation preceded durable intent: state=%+v err=%v", state, readErr)
			}
			sawPersistedIntent = true
		}
		if strings.Contains(command, "link set "+tapName+" up") || strings.Contains(command, "link delete "+tapName) {
			return nil, os.ErrPermission
		}
		return nil, nil
	}
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "configure_tap",
		Tap: &TapConfig{
			AgentID:    "agent-1",
			InstanceID: instanceID,
			TapName:    tapName,
			GuestIP:    "172.30.0.2",
			BridgeName: cfg.BridgeName,
			BridgeCIDR: cfg.BridgeCIDR,
			OwnerUID:   cfg.JailerUID,
		},
	})
	if resp.OK || !sawPersistedIntent {
		t.Fatalf("tap failure response=%+v sawPersistedIntent=%v", resp, sawPersistedIntent)
	}
	state, err := server.readState(instanceID)
	if err != nil || state.TapName != tapName {
		t.Fatalf("failed tap cleanup lost recovery intent: state=%+v err=%v", state, err)
	}
}

func TestPrivilegedLauncherInstallsNetdevSourceGuardBeforeTapReady(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.network.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	var commands []string
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	instanceID := "fc-agent-source-guard"
	tapName := tapNameForInstance(cfg.TapPrefix, instanceID)
	guestIP := "172.30.0.2"
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "configure_tap",
		Tap: &TapConfig{
			AgentID:    "agent-1",
			InstanceID: instanceID,
			TapName:    tapName,
			GuestIP:    guestIP,
			BridgeName: cfg.BridgeName,
			BridgeCIDR: cfg.BridgeCIDR,
			OwnerUID:   cfg.JailerUID,
		},
	})
	if !resp.OK {
		t.Fatalf("configure guarded tap response=%+v", resp)
	}
	joined := strings.Join(commands, "\n")
	for _, required := range []string{
		cfg.NftPath + " add table netdev " + launcherSourceGuardTable,
		"hook ingress device " + tapName,
		"ether saddr != " + guestMACForInstance(tapName) + " drop",
		"ether type ip ip saddr " + guestIP + " accept",
		"ether type arp arp saddr ip " + guestIP + " accept",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("tap source guard omits %q:\n%s", required, joined)
		}
	}
	state, err := server.readState(instanceID)
	if err != nil || !state.SourceGuard || state.GuestIP != guestIP || state.GuestMAC != guestMACForInstance(tapName) {
		t.Fatalf("guarded tap state=%+v err=%v", state, err)
	}
}

func TestPrivilegedLauncherReclaimsStoppedGuestIdentityOwner(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ownedID := "fc-agent-compartment-old"
	ownedTap := tapNameForInstance(cfg.TapPrefix, ownedID)
	guestIP := "172.30.0.2"
	route := TransparentRoute{
		AgentID:     "agent-1",
		InstanceID:  ownedID,
		SourceIP:    guestIP,
		HTTPPort:    cfg.TransparentHTTPPort,
		InterfaceID: ownedTap,
	}
	if err := server.writeState(privilegedLauncherState{
		InstanceID:  ownedID,
		TapName:     ownedTap,
		GuestIP:     guestIP,
		GuestMAC:    guestMACForInstance(ownedTap),
		SourceGuard: true,
		Started:     true,
		Route:       &route,
	}); err != nil {
		t.Fatal(err)
	}
	server.router.routes[ownedID] = route
	originalProcessRunning := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(instanceID string) (bool, error) {
		if instanceID != ownedID {
			t.Fatalf("checked unexpected firecracker process %q", instanceID)
		}
		return false, nil
	}
	t.Cleanup(func() { firecrackerProcessRunningFunc = originalProcessRunning })

	var networkCommands []string
	server.network.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		networkCommands = append(networkCommands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	var hostCommands []string
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		hostCommands = append(hostCommands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	routeRemoved := false
	server.router.run = func(_ context.Context, _ string, args ...string) error {
		if sliceContains(args, "-D") {
			routeRemoved = true
		}
		return nil
	}

	instanceID := "fc-agent-compartment-new"
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "configure_tap",
		Tap: &TapConfig{
			AgentID:    "agent-1",
			InstanceID: instanceID,
			TapName:    tapNameForInstance(cfg.TapPrefix, instanceID),
			GuestIP:    guestIP,
			BridgeName: cfg.BridgeName,
			BridgeCIDR: cfg.BridgeCIDR,
			OwnerUID:   cfg.JailerUID,
		},
	})
	if !resp.OK {
		t.Fatalf("reclaim stopped source owner response=%+v", resp)
	}
	if _, err := server.readState(ownedID); !os.IsNotExist(err) {
		t.Fatalf("stale predecessor state remains: %v", err)
	}
	if !routeRemoved {
		t.Fatal("stale predecessor egress route was not removed")
	}
	if !strings.Contains(strings.Join(networkCommands, "\n"), "link delete "+ownedTap) {
		t.Fatalf("stale predecessor tap was not removed: %v", networkCommands)
	}
	oldGuard := launcherSourceGuardChain(ownedID)
	joinedHostCommands := strings.Join(hostCommands, "\n")
	if !strings.Contains(joinedHostCommands, "flush chain netdev "+launcherSourceGuardTable+" "+oldGuard) ||
		!strings.Contains(joinedHostCommands, "delete chain netdev "+launcherSourceGuardTable+" "+oldGuard) {
		t.Fatalf("stale predecessor source guard was not removed:\n%s", joinedHostCommands)
	}
}

func TestPrivilegedLauncherRemoveTapSelfHealsStoppedInstanceRoute(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-compartment-dead"
	tap := tapNameForInstance(cfg.TapPrefix, instanceID)
	guestIP := "172.30.0.2"
	route := TransparentRoute{
		AgentID:     "agent-1",
		InstanceID:  instanceID,
		SourceIP:    guestIP,
		HTTPPort:    cfg.TransparentHTTPPort,
		InterfaceID: tap,
	}
	// A stopped instance whose earlier unregister_route call never completed, so
	// the state still records an egress route. remove_tap must self-heal it.
	if err := server.writeState(privilegedLauncherState{
		InstanceID:  instanceID,
		TapName:     tap,
		GuestIP:     guestIP,
		GuestMAC:    guestMACForInstance(tap),
		SourceGuard: true,
		Started:     true,
		Route:       &route,
	}); err != nil {
		t.Fatal(err)
	}
	server.router.routes[instanceID] = route
	originalProcessRunning := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { firecrackerProcessRunningFunc = originalProcessRunning })

	var networkCommands []string
	server.network.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		networkCommands = append(networkCommands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	server.runHost = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, nil }
	routeRemoved := false
	server.router.run = func(_ context.Context, _ string, args ...string) error {
		if sliceContains(args, "-D") {
			routeRemoved = true
		}
		return nil
	}

	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "remove_tap",
		TapName: tap,
	})
	if !resp.OK {
		t.Fatalf("remove_tap for a stopped routed instance must self-heal: %+v", resp)
	}
	if !routeRemoved {
		t.Fatal("lingering egress route was not self-healed before tap removal")
	}
	if _, err := server.readState(instanceID); !os.IsNotExist(err) {
		t.Fatalf("state remains after remove_tap: %v", err)
	}
	if !strings.Contains(strings.Join(networkCommands, "\n"), "link delete "+tap) {
		t.Fatalf("tap was not removed: %v", networkCommands)
	}
}

func TestPrivilegedLauncherRemoveTapStillRefusesRunningInstance(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-compartment-live"
	tap := tapNameForInstance(cfg.TapPrefix, instanceID)
	if err := server.writeState(privilegedLauncherState{
		InstanceID: instanceID, TapName: tap, GuestIP: "172.30.0.2",
		GuestMAC: guestMACForInstance(tap), SourceGuard: true, Started: true,
	}); err != nil {
		t.Fatal(err)
	}
	originalProcessRunning := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { firecrackerProcessRunningFunc = originalProcessRunning })

	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "remove_tap",
		TapName: tap,
	})
	if resp.OK {
		t.Fatal("remove_tap must refuse a running instance")
	}
	if _, err := server.readState(instanceID); err != nil {
		t.Fatalf("running instance state must be retained: %v", err)
	}
}

func TestPrivilegedLauncherRejectsRunningDuplicateGuestSourceIdentity(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ownedID := "fc-agent-owned-source"
	ownedTap := tapNameForInstance(cfg.TapPrefix, ownedID)
	if err := server.writeState(privilegedLauncherState{
		InstanceID:  ownedID,
		TapName:     ownedTap,
		GuestIP:     "172.30.0.2",
		GuestMAC:    guestMACForInstance(ownedTap),
		SourceGuard: true,
		Started:     true,
	}); err != nil {
		t.Fatal(err)
	}
	originalProcessRunning := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(instanceID string) (bool, error) {
		if instanceID != ownedID {
			t.Fatalf("checked unexpected firecracker process %q", instanceID)
		}
		return true, nil
	}
	t.Cleanup(func() { firecrackerProcessRunningFunc = originalProcessRunning })
	commands := 0
	server.network.run = func(context.Context, string, ...string) ([]byte, error) {
		commands++
		return nil, nil
	}
	instanceID := "fc-agent-duplicate-source"
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "configure_tap",
		Tap: &TapConfig{
			AgentID:    "agent-2",
			InstanceID: instanceID,
			TapName:    tapNameForInstance(cfg.TapPrefix, instanceID),
			GuestIP:    "172.30.0.2",
			BridgeName: cfg.BridgeName,
			BridgeCIDR: cfg.BridgeCIDR,
			OwnerUID:   cfg.JailerUID,
		},
	})
	if resp.OK || !strings.Contains(resp.Error, "already owned") || commands != 0 {
		t.Fatalf("duplicate source response=%+v commands=%d", resp, commands)
	}
}

func TestPrivilegedLauncherTracksRouteBeforeMutationAndRetainsFailedCleanup(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-route-crash"
	tapName := tapNameForInstance(cfg.TapPrefix, instanceID)
	if err := server.writeState(privilegedLauncherState{
		InstanceID:  instanceID,
		TapName:     tapName,
		GuestIP:     "172.30.0.2",
		GuestMAC:    guestMACForInstance(tapName),
		SourceGuard: true,
		Started:     true,
	}); err != nil {
		t.Fatal(err)
	}
	route := TransparentRoute{
		AgentID:     "agent-1",
		InstanceID:  instanceID,
		SourceIP:    "172.30.0.2",
		HTTPPort:    cfg.TransparentHTTPPort,
		InterfaceID: tapName,
	}
	sawPersistedIntent := false
	server.router.run = func(_ context.Context, _ string, args ...string) error {
		if sliceContains(args, "-A") {
			state, readErr := server.readState(instanceID)
			if readErr != nil || state.Route == nil || state.Route.InstanceID != instanceID {
				t.Fatalf("route mutation preceded durable intent: state=%+v err=%v", state, readErr)
			}
			sawPersistedIntent = true
		}
		return os.ErrPermission
	}
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "register_route",
		Route:   &route,
	})
	if resp.OK || !sawPersistedIntent {
		t.Fatalf("route failure response=%+v sawPersistedIntent=%v", resp, sawPersistedIntent)
	}
	state, err := server.readState(instanceID)
	if err != nil || state.Route == nil {
		t.Fatalf("failed route cleanup lost recovery intent: state=%+v err=%v", state, err)
	}

	server.router.run = func(_ context.Context, _ string, args ...string) error {
		if sliceContains(args, "-D") {
			return &iptablesMissingRuleError{}
		}
		return nil
	}
	resp = server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "unregister_route",
		ID:      instanceID,
	})
	if !resp.OK {
		t.Fatalf("idempotent missing-route cleanup response=%+v", resp)
	}
	state, err = server.readState(instanceID)
	if err != nil || state.Route != nil {
		t.Fatalf("cleaned route state=%+v err=%v", state, err)
	}
}

func TestPrivilegedLauncherRefusesRouteWithoutActiveSourceGuard(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-unguarded-route"
	tapName := tapNameForInstance(cfg.TapPrefix, instanceID)
	if err := server.writeState(privilegedLauncherState{
		InstanceID: instanceID,
		TapName:    tapName,
		GuestIP:    "172.30.0.2",
		GuestMAC:   guestMACForInstance(tapName),
		Started:    true,
	}); err != nil {
		t.Fatal(err)
	}
	commands := 0
	server.router.run = func(context.Context, string, ...string) error {
		commands++
		return nil
	}
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "register_route",
		Route: &TransparentRoute{
			AgentID:     "agent-1",
			InstanceID:  instanceID,
			SourceIP:    "172.30.0.2",
			HTTPPort:    cfg.TransparentHTTPPort,
			InterfaceID: tapName,
		},
	})
	if resp.OK || !strings.Contains(resp.Error, "identity") || commands != 0 {
		t.Fatalf("unguarded route response=%+v commands=%d", resp, commands)
	}
}

func TestPrivilegedLauncherReconcilesProcessStateAndKillsUntrackedVM(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	liveTracked := "fc-agent-live-tracked"
	staleTracked := "fc-agent-stale-tracked"
	untracked := "fc-agent-live-untracked"
	if err := server.writeState(privilegedLauncherState{InstanceID: liveTracked}); err != nil {
		t.Fatal(err)
	}
	if err := server.writeState(privilegedLauncherState{InstanceID: staleTracked, Started: true}); err != nil {
		t.Fatal(err)
	}
	var killed []string
	err = server.reconcileLauncherProcessState(map[string]struct{}{
		liveTracked: {},
		untracked:   {},
	}, func(instanceID string) error {
		killed = append(killed, instanceID)
		return nil
	})
	if err != nil {
		t.Fatalf("reconcile process state: %v", err)
	}
	liveState, err := server.readState(liveTracked)
	if err != nil || !liveState.Started {
		t.Fatalf("live tracked state=%+v err=%v", liveState, err)
	}
	staleState, err := server.readState(staleTracked)
	if err != nil || staleState.Started {
		t.Fatalf("stale tracked state=%+v err=%v", staleState, err)
	}
	if len(killed) != 1 || killed[0] != untracked {
		t.Fatalf("killed untracked instances = %v", killed)
	}
}

func TestPrivilegedLauncherRecoversMissingTapAndPersistedRoute(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	instanceID := "fc-agent-missing-tap"
	tapName := tapNameForInstance(cfg.TapPrefix, instanceID)
	route := &TransparentRoute{
		AgentID:     "agent-1",
		InstanceID:  instanceID,
		SourceIP:    "172.30.0.2",
		HTTPPort:    cfg.TransparentHTTPPort,
		InterfaceID: tapName,
	}
	if err := server.writeState(privilegedLauncherState{
		InstanceID:  instanceID,
		TapName:     tapName,
		GuestIP:     route.SourceIP,
		GuestMAC:    guestMACForInstance(tapName),
		SourceGuard: true,
		Route:       route,
	}); err != nil {
		t.Fatal(err)
	}
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == cfg.HarnessIPCommand && sliceContainsSequence(args, "link", "show", tapName) {
			return []byte("Device does not exist"), os.ErrNotExist
		}
		if name == cfg.NftPath {
			return []byte("No such file or directory"), os.ErrNotExist
		}
		return nil, nil
	}
	removedRoute := false
	server.router.run = func(_ context.Context, _ string, args ...string) error {
		if sliceContains(args, "-D") {
			removedRoute = true
			return &iptablesMissingRuleError{}
		}
		return nil
	}
	if err := server.restoreTapSourceGuards(context.Background()); err != nil {
		t.Fatalf("recover missing tap: %v", err)
	}
	if !removedRoute {
		t.Fatal("missing tap recovery did not reconcile persisted route")
	}
	if _, err := server.readState(instanceID); !os.IsNotExist(err) {
		t.Fatalf("missing tap state remains: %v", err)
	}
}

type iptablesMissingRuleError struct{}

func (*iptablesMissingRuleError) Error() string {
	return "iptables: Bad rule (does a matching rule exist in that chain?)"
}

func TestPrivilegedLauncherDerivesAndRecoversHarnessNetwork(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	var commands []string
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	cellID := "hcell-agent-session-request"
	namespace, err := harness.DeriveNetworkNamespace(cellID, cfg.HarnessCIDR)
	if err != nil {
		t.Fatal(err)
	}
	namespace.ProxyIP = cfg.HarnessProxyIP
	namespace.PlatformIP = cfg.HarnessPlatformIP

	tampered := *namespace
	tampered.GuestIP = "169.254.77.254"
	resp := server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "prepare_harness_netns",
		HarnessPrepare: &privilegedHarnessPrepareRequest{
			CellID:    cellID,
			Namespace: tampered,
		},
	})
	if resp.OK || len(commands) != 0 {
		t.Fatalf("tampered harness network response=%+v commands=%v", resp, commands)
	}

	resp = server.handle(context.Background(), privilegedLauncherRequest{
		Version: privilegedLauncherProtocolVersion,
		Op:      "prepare_harness_netns",
		HarnessPrepare: &privilegedHarnessPrepareRequest{
			CellID:    cellID,
			Namespace: *namespace,
		},
	})
	if !resp.OK || resp.ResultPath != filepath.Join(cfg.HarnessResultRoot, cellID+".json") {
		t.Fatalf("prepare harness network response = %+v", resp)
	}
	joined := strings.Join(commands, "\n")
	for _, required := range []string{
		cfg.HarnessSystemdRun + " --quiet --wait --pipe --collect --service-type=exec --property NoNewPrivileges=yes",
		"--property CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_ADMIN",
		"--property AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN",
		"--property RestrictAddressFamilies=AF_UNIX AF_NETLINK",
		"-- " + cfg.HarnessIPCommand + " netns add " + namespace.NamespaceName,
		"-- " + cfg.HarnessIPCommand + " link add " + namespace.HostVethName + " type veth peer name " + namespace.GuestVethName,
		"-- " + cfg.HarnessIPCommand + " link set " + namespace.GuestVethName + " address " + launcherSourceMAC(cellID),
		"route replace " + cfg.HarnessProxyIP + "/32 via " + namespace.HostIP,
		cfg.NftPath + " add table netdev " + launcherSourceGuardTable,
		"hook ingress device " + namespace.HostVethName,
		"ether type ip ip saddr " + namespace.GuestIP + " accept",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("harness network commands omit %q:\n%s", required, joined)
		}
	}
	if _, err := server.readHarnessState(cellID); err != nil {
		t.Fatalf("read prepared harness state: %v", err)
	}
	for _, want := range []struct {
		path string
		mode os.FileMode
		uid  uint32
		gid  uint32
	}{
		{resp.ResultPath, 0o660, uint32(cfg.AllowedUID), uint32(cfg.ManagerGID)},
		{filepath.Join(cfg.HarnessResultRoot, cellID+".bpf"), 0o640, uint32(launcherRootUID(cfg)), uint32(cfg.ManagerGID)},
	} {
		info, err := os.Lstat(want.path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode().Perm() != want.mode || stat.Uid != want.uid || stat.Gid != want.gid {
			t.Fatalf("exchange path %s mode/owner = %o %d:%d, want %o %d:%d", want.path, info.Mode().Perm(), stat.Uid, stat.Gid, want.mode, want.uid, want.gid)
		}
	}
	partialCellID := "hcell-agent-session-partial"
	partialNamespace, err := harness.DeriveNetworkNamespace(partialCellID, cfg.HarnessCIDR)
	if err != nil {
		t.Fatal(err)
	}
	partialNamespace.ProxyIP = cfg.HarnessProxyIP
	partialNamespace.PlatformIP = cfg.HarnessPlatformIP
	partialState := privilegedHarnessState{
		CellID:      partialCellID,
		Namespace:   *partialNamespace,
		ResultPath:  filepath.Join(cfg.HarnessResultRoot, partialCellID+".json"),
		SeccompPath: filepath.Join(cfg.HarnessResultRoot, partialCellID+".bpf"),
		UnitName:    harnessUnitName(*partialNamespace),
		GuestMAC:    launcherSourceMAC(partialCellID),
		SourceGuard: launcherSourceGuardChain(partialCellID),
	}
	if err := server.writeHarnessState(partialState); err != nil {
		t.Fatalf("persist partial harness intent: %v", err)
	}

	// A launcher restart removes any transient unit, namespace, exchange files,
	// and persisted state before accepting new work, including a crash after
	// intent persistence but before the first host mutation.
	if _, err := NewPrivilegedLauncherServer(cfg); err != nil {
		t.Fatalf("recover launcher: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateRoot, "harness", cellID+".json")); !os.IsNotExist(err) {
		t.Fatalf("recovered harness state still exists: %v", err)
	}
	if _, err := os.Stat(resp.ResultPath); !os.IsNotExist(err) {
		t.Fatalf("recovered harness result still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateRoot, "harness", partialCellID+".json")); !os.IsNotExist(err) {
		t.Fatalf("partial harness intent still exists: %v", err)
	}
	if _, err := NewPrivilegedLauncherServer(cfg); err != nil {
		t.Fatalf("idempotent launcher recovery: %v", err)
	}
}

func TestPrivilegedLauncherHarnessExecIsFixedUnprivilegedTransientUnit(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	cellID := "hcell-agent-session-exec"
	namespace, err := harness.DeriveNetworkNamespace(cellID, cfg.HarnessCIDR)
	if err != nil {
		t.Fatal(err)
	}
	namespace.ProxyIP = cfg.HarnessProxyIP
	namespace.PlatformIP = cfg.HarnessPlatformIP
	state := privilegedHarnessState{
		CellID:      cellID,
		Namespace:   *namespace,
		ResultPath:  filepath.Join(cfg.HarnessResultRoot, cellID+".json"),
		SeccompPath: filepath.Join(cfg.HarnessResultRoot, cellID+".bpf"),
		UnitName:    harnessUnitName(*namespace),
		GuestMAC:    launcherSourceMAC(cellID),
		SourceGuard: launcherSourceGuardChain(cellID),
	}
	req := &privilegedHarnessExecRequest{
		CellID: cellID, Namespace: *namespace, Command: cfg.HarnessBubblewrap,
		Args: []string{
			"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
			"--uid", "65534", "--gid", "65534", "--disable-userns", "--assert-userns-disabled",
			"--die-with-parent", "--new-session", "--proc", "/proc", "--dev", "/dev",
			"--tmpfs", "/tmp", "--dir", "/run", "--setenv", "HOME", "/tmp",
			"--bind", state.ResultPath, state.ResultPath, "--seccomp", "3", "/bin/true",
		},
		Env:         []string{"PATH=/usr/bin", "HARNESS_RESULT_PATH=" + state.ResultPath},
		SeccompBPF:  []byte{1, 2, 3},
		MemoryBytes: cfg.HarnessMemoryBytes,
		NanoCPUs:    cfg.HarnessNanoCPUs,
		PidsLimit:   cfg.HarnessPidsLimit,
		MaxRuntime:  cfg.HarnessMaxRuntime.Milliseconds(),
		IdleTimeout: cfg.HarnessIdleTimeout.Milliseconds(),
	}
	if err := server.validateHarnessExecutionRequest(req); err != nil {
		t.Fatalf("valid harness execution: %v", err)
	}
	args := server.harnessSystemdRunArgs(state, req)
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"User=" + strconv.Itoa(cfg.AllowedUID),
		"Group=" + strconv.Itoa(cfg.ManagerGID),
		"SupplementaryGroups=",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETFCAP",
		"NetworkNamespacePath=/run/netns/" + namespace.NamespaceName,
		"MemoryMax=" + strconv.FormatInt(cfg.HarnessMemoryBytes, 10),
		" -- " + cfg.HarnessEnvCommand + " -i -- ",
		cfg.HarnessShell + " -c",
	} {
		if !strings.Contains(" "+joined+" ", required) {
			t.Fatalf("transient harness command omits %q:\n%s", required, joined)
		}
	}
	req.Command = "/bin/sh"
	if err := server.validateHarnessExecutionRequest(req); err == nil || !strings.Contains(err.Error(), "command does not match") {
		t.Fatalf("arbitrary harness command error = %v", err)
	}
	req.Command = cfg.HarnessBubblewrap
	command := req.Args[len(req.Args)-1]
	req.Args = append(append(append([]string(nil), req.Args[:len(req.Args)-1]...), "--uid", "0"), command)
	if err := server.validateHarnessExecutionRequest(req); err == nil || !strings.Contains(err.Error(), "exactly one --uid 65534") {
		t.Fatalf("duplicate harness uid error = %v", err)
	}
}

func TestPrivilegedLauncherKeepsHarnessGuardUntilUnitStops(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	cellID := "hcell-cleanup-order"
	namespace, err := harness.DeriveNetworkNamespace(cellID, cfg.HarnessCIDR)
	if err != nil {
		t.Fatal(err)
	}
	namespace.ProxyIP = cfg.HarnessProxyIP
	namespace.PlatformIP = cfg.HarnessPlatformIP
	state := privilegedHarnessState{
		CellID:      cellID,
		Namespace:   *namespace,
		UnitName:    harnessUnitName(*namespace),
		GuestMAC:    launcherSourceMAC(cellID),
		SourceGuard: launcherSourceGuardChain(cellID),
	}
	var commands []string
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == cfg.HarnessSystemctl && len(args) > 0 && args[0] == "show" {
			return []byte("inactive\n"), nil
		}
		return nil, nil
	}
	if err := server.cleanupHarnessNetwork(context.Background(), state); err != nil {
		t.Fatalf("cleanup harness network: %v", err)
	}
	joined := strings.Join(commands, "\n")
	killIndex := strings.Index(joined, cfg.HarnessSystemctl+" kill --signal=SIGKILL --kill-whom=all "+state.UnitName)
	guardIndex := strings.Index(joined, cfg.NftPath+" flush chain netdev "+launcherSourceGuardTable+" "+state.SourceGuard)
	netnsIndex := strings.Index(joined, cfg.HarnessIPCommand+" netns delete "+namespace.NamespaceName)
	if killIndex < 0 || guardIndex <= killIndex || netnsIndex <= guardIndex {
		t.Fatalf("cleanup order must be kill, guard, namespace:\n%s", joined)
	}

	commands = nil
	server.runHost = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == cfg.HarnessSystemctl && len(args) > 0 && args[0] == "kill" {
			return []byte("permission denied"), os.ErrPermission
		}
		return nil, nil
	}
	if err := server.cleanupHarnessNetwork(context.Background(), state); err == nil {
		t.Fatal("expected kill failure")
	}
	joined = strings.Join(commands, "\n")
	if strings.Contains(joined, " flush chain netdev "+launcherSourceGuardTable) || strings.Contains(joined, " netns delete "+namespace.NamespaceName) {
		t.Fatalf("kill failure removed guard or namespace:\n%s", joined)
	}
}

func TestPrivilegedLauncherUnixClientUsesPeerCredentialedProtocol(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "launcher.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &PrivilegedLauncherServer{cfg: PrivilegedLauncherConfig{AllowedUID: os.Getuid()}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			server.serveConn(conn)
		}
	}()
	client := newPrivilegedLauncherClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping launcher: %v", err)
	}
	<-done
}

func TestPrivilegedLauncherHandsManagerOnlySocketAccessAcrossJail(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "agent-manager-launcher-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	jailRoot := filepath.Join(dir, "jailer")
	vmRoot := filepath.Join(jailRoot, "firecracker", "fc-agent-12345678", "root")
	if err := os.MkdirAll(vmRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(vmRoot, firecrackerSockName), filepath.Join(vmRoot, vsockUDSName)}
	var listeners []net.Listener
	for _, path := range paths {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
	}
	t.Cleanup(func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	})
	server := &PrivilegedLauncherServer{cfg: PrivilegedLauncherConfig{JailRoot: jailRoot, SocketGID: os.Getgid()}}
	if err := server.grantManagerSocketAccess(paths...); err != nil {
		t.Fatalf("grant manager socket access: %v", err)
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o660 {
			t.Fatalf("socket %s mode = %o, want 660", path, info.Mode().Perm())
		}
	}
	for _, path := range []string{jailRoot, filepath.Join(jailRoot, "firecracker"), filepath.Join(jailRoot, "firecracker", "fc-agent-12345678"), vmRoot} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("socket directory %s mode = %o, want 750", path, info.Mode().Perm())
		}
	}
}

func TestVerifiedLauncherHardlinkPreservesCanonicalManagerAccess(t *testing.T) {
	cfg := testPrivilegedLauncherConfig(t)
	server, err := NewPrivilegedLauncherServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	destinationDir := filepath.Join(t.TempDir(), "jail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "workspace.ext4")
	destination := filepath.Join(destinationDir, "workspace.ext4")
	if err := os.WriteFile(source, []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.stageVerifiedLauncherLink(root, source, destination, os.Getuid(), os.Getgid(), 0o660, false); err != nil {
		t.Fatalf("stage verified workspace: %v", err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	sourceStat := sourceInfo.Sys().(*syscall.Stat_t)
	destinationStat := destinationInfo.Sys().(*syscall.Stat_t)
	if sourceInfo.Mode().Perm() != 0o660 || sourceStat.Uid != uint32(os.Getuid()) || sourceStat.Gid != uint32(os.Getgid()) {
		t.Fatalf("canonical workspace mode/owner = %o %d:%d", sourceInfo.Mode().Perm(), sourceStat.Uid, sourceStat.Gid)
	}
	if sourceStat.Dev != destinationStat.Dev || sourceStat.Ino != destinationStat.Ino {
		t.Fatal("jail workspace is not the atomically verified canonical inode")
	}
}

func TestVerifiedLauncherSourceSwapCannotChownOutsideTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	destinationDir := filepath.Join(t.TempDir(), "jail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "workspace.ext4")
	original := filepath.Join(root, "original.ext4")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(source, []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideBefore, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	outsideBeforeStat := *outsideBefore.Sys().(*syscall.Stat_t)
	verified, err := openVerifiedLauncherFile(root, source, true)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if err := os.Rename(source, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, source); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDir, "workspace.ext4")
	err = stageVerifiedLauncherLink(verified, destination, os.Getuid(), os.Getgid(), 0o660, false, true)
	if err == nil || !strings.Contains(err.Error(), "source binding") {
		t.Fatalf("source swap error = %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("staged link survived rejected source swap: %v", err)
	}
	outsideAfter, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	outsideAfterStat := outsideAfter.Sys().(*syscall.Stat_t)
	if outsideAfter.Mode().Perm() != outsideBefore.Mode().Perm() || outsideAfterStat.Uid != outsideBeforeStat.Uid || outsideAfterStat.Gid != outsideBeforeStat.Gid {
		t.Fatalf("outside target mutated from %o %d:%d to %o %d:%d", outsideBefore.Mode().Perm(), outsideBeforeStat.Uid, outsideBeforeStat.Gid, outsideAfter.Mode().Perm(), outsideAfterStat.Uid, outsideAfterStat.Gid)
	}
}
