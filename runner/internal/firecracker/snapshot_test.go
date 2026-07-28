package firecracker

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
)

func TestCreateGoldenSnapshotPausesSnapshotsResumesAndWritesManifest(t *testing.T) {
	dir := t.TempDir()
	socketPath := shortUnixSocketPath(t, "firecracker.sock")
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
			instanceID: {id: instanceID, sandboxID: "agent-1", socket: socketPath},
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
	if manifest.InstanceID != instanceID || manifest.SandboxID != "agent-1" || manifest.Machine.VCPUCount != 2 || manifest.Machine.MemSizeMiB != 2048 || manifest.Machine.CPUTemplate != "None" {
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

func TestGoldenSnapshotCodeCannotLaunchFirecracker(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snapshot.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse snapshot.go: %v", err)
	}
	for _, spec := range file.Imports {
		if spec.Path.Value == `"os/exec"` {
			t.Fatal("snapshot.go must not execute Firecracker; restores belong in the runner composition backend")
		}
	}

	file, err = parser.ParseFile(fset, "snapshot.go", nil, 0)
	if err != nil {
		t.Fatalf("parse snapshot.go declarations: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "RestoreGoldenSnapshot" {
			t.Fatal("dormant unjailed golden-snapshot restore surface was reintroduced")
		}
	}
}

func writeSnapshotFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
