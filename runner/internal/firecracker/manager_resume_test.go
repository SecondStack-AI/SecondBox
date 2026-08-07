package firecracker

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

// stubRootfsReflink replaces the copy-on-write child clone with a byte copy so
// resume staging can be exercised on a filesystem without reflink support.
// Production has no such fallback; TestReflinkOnlyFileFailsAcrossDevices proves
// the real helper refuses to copy.
func stubRootfsReflink(t *testing.T) {
	t.Helper()
	original := reflinkRootfsChild
	reflinkRootfsChild = func(destination, source string, mode os.FileMode) error {
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	t.Cleanup(func() { reflinkRootfsChild = original })
}

func newResumeTestTemplate(t *testing.T) (*SnapshotTemplateCache, *AdmittedSnapshotTemplate, SnapshotTemplateKey) {
	t.Helper()
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	stageTestSnapshotTemplate(t, cache, key)
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve template: %v", err)
	}
	return cache, template, key
}

// shortResumeDir keeps the per-Instance API and vsock socket paths inside the
// kernel's sockaddr_un limit, which the Go test temp-dir naming exceeds.
func shortResumeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sb-res-")
	if err != nil {
		t.Fatalf("create short resume directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove short resume directory: %v", err)
		}
	})
	return dir
}

func newResumeTestManager(t *testing.T, runDir string) *Manager {
	t.Helper()
	return &Manager{cfg: &config.Config{
		FirecrackerPath:      "/usr/local/bin/firecracker",
		MicroVMAllowUnjailed: true,
		MicroVMRunDir:        runDir,
	}}
}

func writeResumeWorkspace(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "workspace-source.ext4")
	if err := os.WriteFile(path, []byte("workspace-bytes"), 0o600); err != nil {
		t.Fatalf("write workspace image: %v", err)
	}
	return path
}

func TestStageSharedTemplateFileSharesOneInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "memory.snap")
	if err := os.WriteFile(source, []byte("golden-memory"), 0o444); err != nil {
		t.Fatalf("write golden memory: %v", err)
	}
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.Mkdir(first, 0o700); err != nil {
		t.Fatalf("create first jail: %v", err)
	}
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatalf("create second jail: %v", err)
	}
	firstStaged := filepath.Join(first, snapshotResumeMemoryName)
	secondStaged := filepath.Join(second, snapshotResumeMemoryName)
	if err := stageSharedTemplateFile(firstStaged, source); err != nil {
		t.Fatalf("stage first: %v", err)
	}
	if err := stageSharedTemplateFile(secondStaged, source); err != nil {
		t.Fatalf("stage second: %v", err)
	}
	shared, err := sharesInode(firstStaged, secondStaged)
	if err != nil {
		t.Fatalf("compare staged inodes: %v", err)
	}
	if !shared {
		t.Fatal("two staged golden memory files do not share one inode, so they would not share one page cache")
	}
	shared, err = sharesInode(firstStaged, source)
	if err != nil {
		t.Fatalf("compare staged and source inodes: %v", err)
	}
	if !shared {
		t.Fatal("the staged golden memory file is not the golden file")
	}
}

func TestStageSharedTemplateFileNamesTheTemplateRootOnCrossDeviceFailure(t *testing.T) {
	original := hardLinkFile
	hardLinkFile = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { hardLinkFile = original })
	err := stageSharedTemplateFile(filepath.Join(t.TempDir(), "memory.snap"), "/golden/memory.snap")
	if err == nil {
		t.Fatal("a cross-device template link was accepted")
	}
	if !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("error does not wrap EXDEV: %v", err)
	}
	if !strings.Contains(err.Error(), "snapshot template cache root") ||
		!strings.Contains(err.Error(), "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT") {
		t.Fatalf("error does not name the misconfigured pair of locations: %v", err)
	}
}

func TestPrepareSnapshotResumeLaunchUnjailedReferencesTheTemplateInPlace(t *testing.T) {
	stubRootfsReflink(t)
	_, template, _ := newResumeTestTemplate(t)
	runDir := shortResumeDir(t)
	manager := newResumeTestManager(t, runDir)
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	workspacePath := writeResumeWorkspace(t, runDir)

	launch, err := manager.prepareSnapshotResumeLaunch(
		"fc-resume",
		instanceDir,
		template,
		workspacePath,
		"",
		0,
		nil,
	)
	if err != nil {
		t.Fatalf("prepare unjailed resume: %v", err)
	}
	if launch.vmStateResolvedPath != template.VMStatePath || launch.memoryResolvedPath != template.MemoryPath {
		t.Fatalf("unjailed resume copied the template instead of referencing it: %+v", launch)
	}
	if slices.Contains(launch.args, "--config-file") {
		t.Fatalf("resume passed a boot configuration file: %v", launch.args)
	}
	if !slices.Contains(launch.args, "--api-sock") {
		t.Fatalf("resume did not expose an API socket: %v", launch.args)
	}
	for _, name := range []string{snapshotTemplateRootfsName, workspaceName} {
		if _, err := os.Stat(filepath.Join(instanceDir, name)); err != nil {
			t.Fatalf("resume did not stage %s: %v", name, err)
		}
	}
	staged, err := sharesInode(filepath.Join(instanceDir, workspaceName), workspacePath)
	if err != nil {
		t.Fatalf("compare workspace inodes: %v", err)
	}
	if !staged {
		t.Fatal("the staged Workspace is a copy, not the WorkspaceStore's image")
	}
}

func TestPrepareSnapshotResumeLaunchRefusesUnusableTemplates(t *testing.T) {
	stubRootfsReflink(t)
	runDir := shortResumeDir(t)
	manager := newResumeTestManager(t, runDir)
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	workspacePath := writeResumeWorkspace(t, runDir)

	t.Run("absent template", func(t *testing.T) {
		_, err := manager.prepareSnapshotResumeLaunch("fc-resume", instanceDir, nil, workspacePath, "", 0, nil)
		if !errors.Is(err, ErrSnapshotTemplateUnavailable) {
			t.Fatalf("absent template error = %v, want ErrSnapshotTemplateUnavailable", err)
		}
	})

	t.Run("unadmitted template", func(t *testing.T) {
		if _, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume",
			instanceDir,
			&AdmittedSnapshotTemplate{TemplateID: "unadmitted"},
			workspacePath,
			"",
			0,
			nil,
		); err == nil {
			t.Fatal("an unadmitted template was accepted")
		}
	})

	t.Run("template replaced after admission", func(t *testing.T) {
		cache, template, _ := newResumeTestTemplate(t)
		directory := filepath.Join(cache.Root(), template.TemplateID)
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatalf("chmod template dir: %v", err)
		}
		replacement := filepath.Join(directory, "replacement")
		if err := os.WriteFile(replacement, []byte("memory-bytes"), 0o600); err != nil {
			t.Fatalf("write replacement: %v", err)
		}
		if err := os.Rename(replacement, template.MemoryPath); err != nil {
			t.Fatalf("replace memory: %v", err)
		}
		if _, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume",
			instanceDir,
			template,
			workspacePath,
			"",
			0,
			nil,
		); err == nil {
			t.Fatal("a template replaced after admission was accepted")
		}
	})

	t.Run("absent workspace", func(t *testing.T) {
		_, template, _ := newResumeTestTemplate(t)
		if _, err := manager.prepareSnapshotResumeLaunch("fc-resume", instanceDir, template, "  ", "", 0, nil); err == nil {
			t.Fatal("a resume without a Workspace image was accepted")
		}
	})
}

func TestPrepareSnapshotResumeLaunchJailedRequiresPolicyAndUID(t *testing.T) {
	stubRootfsReflink(t)
	_, template, _ := newResumeTestTemplate(t)
	runDir := shortResumeDir(t)
	manager := newResumeTestManager(t, runDir)
	manager.cfg.MicroVMAllowUnjailed = false
	manager.cfg.MicroVMJailerChrootBaseDir = filepath.Join(runDir, "jail")
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	workspacePath := writeResumeWorkspace(t, runDir)

	if _, err := manager.prepareSnapshotResumeLaunch("fc-resume", instanceDir, template, workspacePath, "", 0, nil); err == nil {
		t.Fatal("a jailed resume without a jailer UID was accepted")
	}
	if _, err := manager.prepareSnapshotResumeLaunch("fc-resume", instanceDir, template, workspacePath, "", 12000, nil); err == nil {
		t.Fatal("a jailed resume without a runtime policy was accepted")
	}
	if _, err := manager.prepareSnapshotResumeLaunch(
		"fc-resume",
		instanceDir,
		template,
		workspacePath,
		"",
		12000,
		&runtimemanager.SandboxRuntimePolicy{VCPUs: 1, MemoryMiB: 512, CPUMillis: 0, ProcessLimit: 64},
	); err == nil {
		t.Fatal("a jailed resume without a CPU budget was accepted")
	}
}

func TestSnapshotResumeLoadRequestJailedUsesChrootRelativePaths(t *testing.T) {
	launch := snapshotResumeLaunch{
		jailRoot:            "/srv/jail/firecracker/fc-1/root",
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}
	request := snapshotResumeLoadRequest(launch, " agfc0a1b ")
	if request.SnapshotPath != snapshotResumeVMStateName {
		t.Fatalf("snapshot path = %q, want the chroot-relative name", request.SnapshotPath)
	}
	if request.MemBackend == nil ||
		request.MemBackend.BackendPath != snapshotResumeMemoryName ||
		request.MemBackend.BackendType != "File" {
		t.Fatalf("memory backend = %+v", request.MemBackend)
	}
	if !request.ResumeVM {
		t.Fatal("resume request does not resume the VM")
	}
	if !request.ClockRealtime {
		t.Fatal("resume request does not correct the guest clock from the host")
	}
	if request.VsockOverride == nil || request.VsockOverride.UDSPath != vsockUDSName {
		t.Fatalf("vsock override = %+v", request.VsockOverride)
	}
	if len(request.NetworkOverrides) != 1 ||
		request.NetworkOverrides[0].IfaceID != snapshotResumeNetworkInterfaceID ||
		request.NetworkOverrides[0].HostDevName != "agfc0a1b" {
		t.Fatalf("network overrides = %+v", request.NetworkOverrides)
	}
	if request.TrackDirtyPages {
		t.Fatal("resume request enabled dirty page tracking, which writes to the shared golden memory backing")
	}
}

func TestSnapshotResumeLoadRequestWithoutNetworkOmitsOverride(t *testing.T) {
	request := snapshotResumeLoadRequest(snapshotResumeLaunch{
		vmStateResolvedPath: "/cache/t/vmstate.snap",
		memoryResolvedPath:  "/cache/t/memory.snap",
		vsockResolvedPath:   "/run/fc/guest.vsock",
	}, "   ")
	if len(request.NetworkOverrides) != 0 {
		t.Fatalf("network overrides = %+v, want none", request.NetworkOverrides)
	}
	if request.SnapshotPath != "/cache/t/vmstate.snap" || request.MemBackend.BackendPath != "/cache/t/memory.snap" {
		t.Fatalf("unjailed resume request = %+v", request)
	}
}

func TestResumeSnapshotTemplateLoadsThroughTheAPI(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "firecracker.sock")
	seen := make(chan apiCall, 2)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	launch := snapshotResumeLaunch{
		socketPath:          socketPath,
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}
	if err := resumeSnapshotTemplate(t.Context(), launch, "agfc0a1b", 2*time.Second, 2*time.Second); err != nil {
		t.Fatalf("resume snapshot template: %v", err)
	}
	call := drainAPICalls(seen, 1)[0]
	if call.Path != "/snapshot/load" {
		t.Fatalf("resume called %q", call.Path)
	}
	if call.Body["snapshot_path"] != snapshotResumeVMStateName {
		t.Fatalf("resume load body = %#v", call.Body)
	}
	if call.Body["resume_vm"] != true || call.Body["clock_realtime"] != true {
		t.Fatalf("resume load body = %#v", call.Body)
	}
}

func TestResumeSnapshotTemplateRepointsUnjailedDrivesBeforeResuming(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "firecracker.sock")
	seen := make(chan apiCall, 8)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	launch := snapshotResumeLaunch{
		socketPath:          socketPath,
		vmStateResolvedPath: "/cache/t/vmstate.snap",
		memoryResolvedPath:  "/cache/t/memory.snap",
		vsockResolvedPath:   "/run/fc/guest.vsock",
		stagedDrives: []stagedResumeDrive{
			{driveID: resumeRootfsDriveID, path: "/run/fc/rootfs.ext4"},
			{driveID: resumeWorkspaceDriveID, path: "/run/fc/workspace.ext4"},
		},
	}
	if err := resumeSnapshotTemplate(t.Context(), launch, "", 2*time.Second, 2*time.Second); err != nil {
		t.Fatalf("resume unjailed template: %v", err)
	}
	calls := drainAPICalls(seen, 4)
	if calls[0].Path != "/snapshot/load" {
		t.Fatalf("first call = %#v", calls[0])
	}
	if calls[0].Body["resume_vm"] == true {
		t.Fatal("an unjailed resume started the guest before repointing its drives")
	}
	if calls[1].Path != "/drives/rootfs" || calls[1].Body["path_on_host"] != "/run/fc/rootfs.ext4" {
		t.Fatalf("rootfs repoint = %#v", calls[1])
	}
	if calls[2].Path != "/drives/workspace" || calls[2].Body["path_on_host"] != "/run/fc/workspace.ext4" {
		t.Fatalf("workspace repoint = %#v", calls[2])
	}
	if calls[3].Path != "/vm" || calls[3].Body["state"] != "Resumed" {
		t.Fatalf("resume call = %#v", calls[3])
	}
}

func TestResumeSnapshotTemplateJailedResumesInOneCall(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "firecracker.sock")
	seen := make(chan apiCall, 4)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	launch := snapshotResumeLaunch{
		socketPath:          socketPath,
		jailRoot:            "/srv/jail/firecracker/fc-1/root",
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}
	if err := resumeSnapshotTemplate(t.Context(), launch, "agfc0a1b", 2*time.Second, 2*time.Second); err != nil {
		t.Fatalf("resume jailed template: %v", err)
	}
	call := drainAPICalls(seen, 1)[0]
	if call.Path != "/snapshot/load" || call.Body["resume_vm"] != true {
		t.Fatalf("jailed resume did not resume in one call: %#v", call)
	}
	select {
	case extra := <-seen:
		t.Fatalf("jailed resume issued an extra call: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPrepareSnapshotResumeLaunchUnjailedStagesEveryDrive(t *testing.T) {
	stubRootfsReflink(t)
	_, template, _ := newResumeTestTemplate(t)
	runDir := shortResumeDir(t)
	manager := newResumeTestManager(t, runDir)
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	sharedPath := filepath.Join(runDir, "shared-source.img")
	if err := os.WriteFile(sharedPath, []byte("shared-bytes"), 0o600); err != nil {
		t.Fatalf("write shared image: %v", err)
	}
	launch, err := manager.prepareSnapshotResumeLaunch(
		"fc-resume",
		instanceDir,
		template,
		writeResumeWorkspace(t, runDir),
		sharedPath,
		0,
		nil,
	)
	if err != nil {
		t.Fatalf("prepare unjailed resume: %v", err)
	}
	want := map[string]string{
		resumeRootfsDriveID:    filepath.Join(instanceDir, snapshotTemplateRootfsName),
		resumeWorkspaceDriveID: filepath.Join(instanceDir, workspaceName),
		resumeSharedDriveID:    filepath.Join(instanceDir, sharedImageName),
	}
	if len(launch.stagedDrives) != len(want) {
		t.Fatalf("staged drives = %+v, want %d entries", launch.stagedDrives, len(want))
	}
	for _, staged := range launch.stagedDrives {
		if want[staged.driveID] != staged.path {
			t.Fatalf("drive %q staged at %q, want %q", staged.driveID, staged.path, want[staged.driveID])
		}
		if _, err := os.Stat(staged.path); err != nil {
			t.Fatalf("drive %q was not staged: %v", staged.driveID, err)
		}
	}
}

func TestResumeSnapshotTemplateFailsWhenTheProcessNeverListens(t *testing.T) {
	launch := snapshotResumeLaunch{
		socketPath:          filepath.Join(t.TempDir(), "absent.sock"),
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}
	err := resumeSnapshotTemplate(t.Context(), launch, "", 25*time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("resume succeeded without a Firecracker process")
	}
	if !strings.Contains(err.Error(), "wait for restored Firecracker API socket") {
		t.Fatalf("unexpected resume error: %v", err)
	}
}
