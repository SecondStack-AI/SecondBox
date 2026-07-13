package microvm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentcy/internal/config"
	"agentcy/internal/runtimecontext"
	"agentcy/internal/runtimemanager"
)

func TestCreateGoldenSnapshotPausesSnapshotsResumesAndWritesManifest(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "firecracker.sock")
	seen := make(chan apiCall, 8)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	kernel := writeSnapshotFixture(t, dir, "kernel", "kernel-data")
	rootfs := writeSnapshotFixture(t, dir, "rootfs.ext4", "rootfs-data")
	shared := writeSnapshotFixture(t, dir, "shared.img", "shared-data")
	instanceID := "fc-agent-snapshot"
	m := &Manager{
		cfg: &config.Config{
			FirecrackerPath:        "/usr/bin/firecracker",
			MicroVMKernelPath:      kernel,
			MicroVMRootfsPath:      rootfs,
			MicroVMSharedImagePath: shared,
			MicroVMVCPUs:           2,
			MicroVMMemoryMiB:       2048,
			MicroVMCPUTemplate:     "None",
		},
		instances: map[string]*instance{
			instanceID: {id: instanceID, agentID: "agent-1", socket: socketPath},
		},
	}
	outDir := filepath.Join(dir, "snapshot")
	manifest, err := m.CreateGoldenSnapshot(context.Background(), instanceID, outDir, map[string]string{"artifactVersion": "local"})
	if err != nil {
		t.Fatalf("create golden snapshot: %v", err)
	}
	calls := drainAPICalls(seen, 3)
	if calls[0].Path != "/vm" || calls[0].Method != http.MethodPatch || calls[0].Body["state"] != "Paused" {
		t.Fatalf("pause call = %#v", calls[0])
	}
	if calls[1].Path != "/snapshot/create" || calls[1].Body["snapshot_type"] != "Full" {
		t.Fatalf("snapshot call = %#v", calls[1])
	}
	if calls[2].Path != "/vm" || calls[2].Method != http.MethodPatch || calls[2].Body["state"] != "Resumed" {
		t.Fatalf("resume call = %#v", calls[2])
	}
	if manifest.InstanceID != instanceID || manifest.AgentID != "agent-1" || manifest.Machine.VCPUCount != 2 || manifest.Machine.MemSizeMiB != 2048 || manifest.Machine.CPUTemplate != "None" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if !strings.Contains(manifest.KernelArgs, "noxsave") {
		t.Fatalf("manifest kernel args = %q", manifest.KernelArgs)
	}
	if manifest.KernelSHA256 == "" || manifest.RootfsSHA256 == "" || manifest.SharedSHA256 == "" {
		t.Fatalf("missing artifact hashes: %#v", manifest)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var fromDisk GoldenSnapshotManifest
	if err := json.Unmarshal(data, &fromDisk); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if fromDisk.Metadata["artifactVersion"] != "local" || fromDisk.CreatedAt == "" || fromDisk.FirecrackerVersion != expectedFirecrackerVersionString() {
		t.Fatalf("manifest from disk = %#v", fromDisk)
	}
	if _, err := time.Parse(time.RFC3339, fromDisk.CreatedAt); err != nil {
		t.Fatalf("createdAt is not RFC3339: %q", fromDisk.CreatedAt)
	}
}

func TestVerifySnapshotArtifactsHashIsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	kernel := writeSnapshotFixture(t, dir, "kernel-hash", "aaaa")
	snapshotPath := writeSnapshotFixture(t, dir, "vmstate-hash.snap", "snapshot")
	memPath := writeSnapshotFixture(t, dir, "memory-hash.snap", "memory")
	sum, err := fileSHA256(kernel)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fileArtifactIdentity(kernel)
	if err != nil {
		t.Fatal(err)
	}
	manifest := GoldenSnapshotManifest{
		SnapshotPath:       snapshotPath,
		MemFilePath:        memPath,
		KernelPath:         kernel,
		KernelSHA256:       sum,
		KernelIdentity:     identity,
		FirecrackerVersion: expectedFirecrackerVersionString(),
	}

	if err := os.WriteFile(kernel, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalModTime := time.Unix(0, identity.ModTimeUnixNano)
	if err := os.Chtimes(kernel, originalModTime, originalModTime); err != nil {
		t.Fatal(err)
	}
	mutatedIdentity, err := fileArtifactIdentity(kernel)
	if err != nil {
		t.Fatal(err)
	}
	if mutatedIdentity.Size != identity.Size || mutatedIdentity.ModTimeUnixNano != identity.ModTimeUnixNano {
		t.Fatalf("fixture did not preserve identity: got %+v want %+v", mutatedIdentity, identity)
	}
	if err := verifySnapshotArtifacts(manifest); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("same-identity content mutation error = %v", err)
	}

	if err := os.WriteFile(kernel, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedModTime := originalModTime.Add(2 * time.Second)
	if err := os.Chtimes(kernel, changedModTime, changedModTime); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshotArtifacts(manifest); err != nil {
		t.Fatalf("matching hash with changed identity: %v", err)
	}
}

func TestRestoreGoldenSnapshotStartsFirecrackerAndLoadsSnapshot(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "agfc-restore-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kernel := writeSnapshotFixture(t, dir, "kernel", "kernel-data")
	rootfs := writeSnapshotFixture(t, dir, "rootfs.ext4", "rootfs-data")
	shared := writeSnapshotFixture(t, dir, "shared.img", "shared-data")
	snapshotPath := writeSnapshotFixture(t, dir, "vmstate.snap", "snapshot-data")
	memPath := writeSnapshotFixture(t, dir, "memory.snap", "memory-data")
	vsockPath := filepath.Join(dir, "agentcy.vsock")
	callsPath := filepath.Join(dir, "calls.jsonl")
	fakeFirecracker := filepath.Join(dir, "firecracker")
	if err := os.WriteFile(fakeFirecracker, []byte("#!/bin/sh\nexec "+os.Args[0]+" -test.run=TestFakeFirecrackerProcess -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		cfg: &config.Config{
			FirecrackerPath:         fakeFirecracker,
			MicroVMRunDir:           filepath.Join(dir, "run"),
			MicroVMLogDir:           filepath.Join(dir, "logs"),
			MicroVMWorkspaceDir:     filepath.Join(dir, "workspaces"),
			MicroVMWorkspaceSizeMiB: 8,
			MicroVMWorkspaceBackend: "ext4",
			MicroVMKernelPath:       kernel,
			MicroVMRootfsPath:       rootfs,
			MicroVMVCPUs:            1,
			MicroVMMemoryMiB:        512,
			MicroVMBridgeCIDR:       "172.30.0.1/24",
			AgentRuntimeAuthSecret:  "restore-runtime-secret",
		},
		instances:      map[string]*instance{},
		guestIPs:       map[string]string{},
		network:        &fakeHostNetworkConfigurer{},
		sourceBindings: &fakeSourceBindingRegistrar{},
	}
	manifest := GoldenSnapshotManifest{
		InstanceID:         "fc-source",
		AgentID:            "agent-1",
		SnapshotPath:       snapshotPath,
		MemFilePath:        memPath,
		KernelPath:         kernel,
		RootfsPath:         rootfs,
		SharedImagePath:    shared,
		FirecrackerPath:    fakeFirecracker,
		FirecrackerVersion: expectedFirecrackerVersionString(),
		VsockUDSPath:       vsockPath,
		Machine:            machineConfig{VCPUCount: 1, MemSizeMiB: 512},
	}
	manifest.KernelSHA256, _ = fileSHA256(kernel)
	manifest.RootfsSHA256, _ = fileSHA256(rootfs)
	manifest.SharedSHA256, _ = fileSHA256(shared)

	t.Setenv("AGENTCY_FAKE_FIRECRACKER", "1")
	t.Setenv("AGENTCY_FAKE_FIRECRACKER_CALLS", callsPath)
	secretsPath := filepath.Join(dir, "secrets.jsonl")
	t.Setenv("AGENTCY_FAKE_FIRECRACKER_SECRETS", secretsPath)
	runtimeProjection := runtimecontext.Projection{
		Files: []runtimecontext.File{{
			Path:    runtimecontext.PathGitHubContext,
			Content: `{"service":"github","accessibleRepos":["owner/repo"]}`,
		}},
		OmittedPaths:          []string{runtimecontext.PathNotionContext},
		MigratedServicePaths:  []string{runtimecontext.PathGitHubContext, runtimecontext.PathNotionContext},
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}
	actorContext := runtimecontext.VerifiedActorContext{
		Principal:      "slack:user:U1",
		PlatformUserID: "platform-user-1",
		SessionID:      "session-1",
		TurnContextID:  "turn-1",
		RequestID:      "request-1",
		Verified:       true,
		Source:         "actor_context",
		ExpiresAt:      time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
	instanceID, err := m.RestoreGoldenSnapshot(context.Background(), manifest, RestoreSnapshotOpts{
		CompartmentID:            "cmp_a",
		RuntimeActorContext:      actorContext,
		ProxyEgress:              &runtimemanager.ProxyEgressConfig{Enabled: true},
		RuntimeContextProjection: runtimeProjection,
		Resume:                   true,
		TrackDirtyPages:          true,
		ClockRealtime:            true,
	})
	if err != nil {
		t.Fatalf("restore golden snapshot: %v", err)
	}
	defer m.Remove(context.Background(), instanceID)

	waitForFileContains(t, callsPath, `"snapshot_path":"`)
	data, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"backend_type":"File"`,
		`"track_dirty_pages":true`,
		`"resume_vm":true`,
		`"clock_realtime":true`,
		`"vsock_override":{"uds_path":"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("restore request %q missing %q", got, want)
		}
	}
	if strings.Contains(got, vsockPath) {
		t.Fatalf("restore request reused source vsock path: %q", got)
	}
	if inst := m.lookup(instanceID); inst == nil || inst.vsockUDS == "" || inst.vsockUDS == vsockPath {
		t.Fatalf("restored instance = %#v", inst)
	}
	wantFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", runtimemanager.StartOpts{
		CompartmentID:            "cmp_a",
		RuntimeActorContext:      actorContext,
		ProxyEgress:              &runtimemanager.ProxyEgressConfig{Enabled: true},
		RuntimeContextProjection: runtimeProjection,
	})
	if err != nil {
		t.Fatalf("expected fingerprint: %v", err)
	}
	if inst := m.lookup(instanceID); inst == nil || inst.startupFingerprint != wantFingerprint {
		t.Fatalf("restored startup fingerprint = %q, want %q", inst.startupFingerprint, wantFingerprint)
	}
	staleActor := actorContext
	staleActor.RequestID = "request-old"
	staleActorFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", runtimemanager.StartOpts{
		CompartmentID:            "cmp_a",
		RuntimeActorContext:      staleActor,
		ProxyEgress:              &runtimemanager.ProxyEgressConfig{Enabled: true},
		RuntimeContextProjection: runtimeProjection,
	})
	if err != nil {
		t.Fatalf("stale actor fingerprint: %v", err)
	}
	if staleActorFingerprint == wantFingerprint {
		t.Fatal("restored startup fingerprint did not reject stale actor request id")
	}
	staleProjection := runtimeProjection
	staleProjection.Files = []runtimecontext.File{{
		Path:    runtimecontext.PathGitHubContext,
		Content: `{"service":"github","accessibleRepos":["owner/stale"]}`,
	}}
	staleProjectionFingerprint, err := m.startupFingerprint("agent-1", "cmp_a", runtimemanager.StartOpts{
		CompartmentID:            "cmp_a",
		RuntimeActorContext:      actorContext,
		ProxyEgress:              &runtimemanager.ProxyEgressConfig{Enabled: true},
		RuntimeContextProjection: staleProjection,
	})
	if err != nil {
		t.Fatalf("stale projection fingerprint: %v", err)
	}
	if staleProjectionFingerprint == wantFingerprint {
		t.Fatal("restored startup fingerprint did not reject stale runtime context projection")
	}
	waitForFileContains(t, secretsPath, `"runtimeContextProjection"`)
	secretData, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	secretText := string(secretData)
	for _, want := range []string{
		`"path":"oauth/github.context.json"`,
		`"content":"{\"service\":\"github\",\"accessibleRepos\":[\"owner/repo\"]}"`,
		`"omittedPaths":["oauth/notion.context.json"]`,
		`"migratedServicePaths":["oauth/github.context.json","oauth/notion.context.json"]`,
		`"AGENTCY_EGRESS_CONTEXT_TOKEN"`,
	} {
		if !strings.Contains(secretText, want) {
			t.Fatalf("restore secret payload missing %q: %s", want, secretText)
		}
	}
}

func TestRestoreGoldenSnapshotStagesDistinctCompartmentClones(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "agfc-restore-clones-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kernel := writeSnapshotFixture(t, dir, "kernel", "kernel-data")
	rootfs := writeSnapshotFixture(t, dir, "rootfs.ext4", "rootfs-data")
	snapshotPath := writeSnapshotFixture(t, dir, "vmstate.snap", "snapshot-data")
	memPath := writeSnapshotFixture(t, dir, "memory.snap", "memory-data")
	fakeFirecracker := filepath.Join(dir, "firecracker")
	callsPath := filepath.Join(dir, "calls.jsonl")
	if err := os.WriteFile(fakeFirecracker, []byte("#!/bin/sh\nexec "+os.Args[0]+" -test.run=TestFakeFirecrackerProcess -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	registrar := &fakeSourceBindingRegistrar{}
	m := &Manager{
		cfg: &config.Config{
			FirecrackerPath:         fakeFirecracker,
			MicroVMRunDir:           filepath.Join(dir, "run"),
			MicroVMLogDir:           filepath.Join(dir, "logs"),
			MicroVMWorkspaceDir:     filepath.Join(dir, "workspaces"),
			MicroVMWorkspaceSizeMiB: 8,
			MicroVMWorkspaceBackend: "ext4",
			MicroVMKernelPath:       kernel,
			MicroVMRootfsPath:       rootfs,
			MicroVMVCPUs:            1,
			MicroVMMemoryMiB:        512,
			MicroVMBridgeCIDR:       "172.30.0.1/24",
		},
		instances:      map[string]*instance{},
		guestIPs:       map[string]string{},
		network:        &fakeHostNetworkConfigurer{},
		sourceBindings: registrar,
	}
	manifest := GoldenSnapshotManifest{
		AgentID:            "source-agent",
		SnapshotPath:       snapshotPath,
		MemFilePath:        memPath,
		KernelPath:         kernel,
		RootfsPath:         rootfs,
		FirecrackerVersion: expectedFirecrackerVersionString(),
		Machine:            machineConfig{VCPUCount: 1, MemSizeMiB: 512},
	}
	manifest.KernelSHA256, _ = fileSHA256(kernel)
	manifest.RootfsSHA256, _ = fileSHA256(rootfs)

	t.Setenv("AGENTCY_FAKE_FIRECRACKER", "1")
	t.Setenv("AGENTCY_FAKE_FIRECRACKER_CALLS", callsPath)
	aID, err := m.RestoreGoldenSnapshot(context.Background(), manifest, RestoreSnapshotOpts{
		AgentID:       "agent-1",
		CompartmentID: "cmp_a",
		ProxyEgress:   &runtimemanager.ProxyEgressConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("restore cmp_a: %v", err)
	}
	defer m.Remove(context.Background(), aID)
	bID, err := m.RestoreGoldenSnapshot(context.Background(), manifest, RestoreSnapshotOpts{
		AgentID:       "agent-1",
		CompartmentID: "cmp_b",
		ProxyEgress:   &runtimemanager.ProxyEgressConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("restore cmp_b: %v", err)
	}
	defer m.Remove(context.Background(), bID)

	a, b := m.lookup(aID), m.lookup(bID)
	if a == nil || b == nil {
		t.Fatalf("missing restored instances: a=%#v b=%#v", a, b)
	}
	if a.id == b.id || a.dir == b.dir || a.vsockUDS == b.vsockUDS || a.rootfsPath == b.rootfsPath || a.workspacePath == b.workspacePath || a.guestIP == b.guestIP {
		t.Fatalf("clone resources not distinct:\na=%#v\nb=%#v", a, b)
	}
	if a.compartmentID != "cmp_a" || b.compartmentID != "cmp_b" {
		t.Fatalf("wrong compartment bindings: a=%q b=%q", a.compartmentID, b.compartmentID)
	}
	identityData, err := os.ReadFile(filepath.Join(a.dir, "identity.json"))
	if err != nil {
		t.Fatalf("read cmp_a identity: %v", err)
	}
	if !strings.Contains(string(identityData), `"compartmentId": "cmp_a"`) {
		t.Fatalf("identity missing cmp_a compartment: %s", identityData)
	}
	if len(registrar.registered) != 2 || registrar.registered[0].CompartmentID == registrar.registered[1].CompartmentID {
		t.Fatalf("source bindings not compartment-specific: %#v", registrar.registered)
	}
	waitForFileContains(t, callsPath, `"network_overrides"`)
	data, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, inst := range []*instance{a, b} {
		if !strings.Contains(got, `"host_dev_name":"`+inst.tapName+`"`) || !strings.Contains(got, `"uds_path":"`+inst.vsockUDS+`"`) {
			t.Fatalf("restore requests missing clone overrides for %s: %s", inst.id, got)
		}
	}
}

type fakeHostNetworkConfigurer struct {
	configured []TapConfig
	removed    []string
}

func (f *fakeHostNetworkConfigurer) ConfigureTap(_ context.Context, cfg TapConfig) error {
	f.configured = append(f.configured, cfg)
	return nil
}

func (f *fakeHostNetworkConfigurer) RemoveTap(_ context.Context, tapName string) error {
	f.removed = append(f.removed, tapName)
	return nil
}

func TestFakeFirecrackerProcess(t *testing.T) {
	if os.Getenv("AGENTCY_FAKE_FIRECRACKER") != "1" {
		t.Skip("helper process")
	}
	apiSock := ""
	args := os.Args
	for i, arg := range args {
		if arg == "--api-sock" && i+1 < len(args) {
			apiSock = args[i+1]
			break
		}
	}
	if apiSock == "" {
		fmt.Fprintln(os.Stderr, "missing --api-sock")
		os.Exit(2)
	}
	_ = os.Remove(apiSock)
	ln, err := net.Listen("unix", apiSock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot/load", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if calls := os.Getenv("AGENTCY_FAKE_FIRECRACKER_CALLS"); calls != "" {
			f, err := os.OpenFile(calls, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				_, _ = f.Write(append(data, '\n'))
				_ = f.Close()
			}
		}
		if secretsPath := os.Getenv("AGENTCY_FAKE_FIRECRACKER_SECRETS"); secretsPath != "" {
			var req snapshotLoadRequest
			if err := json.Unmarshal(data, &req); err == nil && req.VsockOverride != nil && strings.TrimSpace(req.VsockOverride.UDSPath) != "" {
				if _, err := startFakeRestoredControlServer(req.VsockOverride.UDSPath, secretsPath); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	_ = server.Close()
	os.Exit(0)
}

func startFakeRestoredControlServer(socketPath, secretsPath string) (func(), error) {
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{AgentID: "agent-1", InstanceID: "restored", Healthy: true})
	})
	mux.HandleFunc("/secrets/apply", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if f, err := os.OpenFile(secretsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"applied": true})
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				br := bufio.NewReader(conn)
				if _, err := br.ReadString('\n'); err != nil {
					_ = conn.Close()
					return
				}
				if _, err := conn.Write([]byte("OK 3\n")); err != nil {
					_ = conn.Close()
					return
				}
				http.Serve(&singleConnListener{Conn: &bufferedConn{Conn: conn, reader: br}}, mux)
			}(conn)
		}
	}()
	return func() {
		_ = ln.Close()
		<-done
	}, nil
}

func writeSnapshotFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForFileContains(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %s to contain %q; got %q", path, needle, string(data))
}
