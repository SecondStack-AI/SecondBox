package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func newResumeTestTemplate(t *testing.T) (*SnapshotTemplateCache, *AdmittedSnapshotTemplate) {
	t.Helper()
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	stageTestSnapshotTemplate(t, cache, key)
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve template: %v", err)
	}
	return cache, template
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

func newJailedResumeManager(t *testing.T, runDir string) *Manager {
	t.Helper()
	return &Manager{cfg: &config.Config{
		FirecrackerPath:            "/usr/local/bin/firecracker",
		JailerPath:                 "/usr/local/bin/jailer",
		MicroVMAllowUnjailed:       false,
		MicroVMRunDir:              runDir,
		MicroVMJailerChrootBaseDir: filepath.Join(runDir, "jail"),
	}}
}

func resumeTestPolicy() *runtimemanager.SandboxRuntimePolicy {
	return &runtimemanager.SandboxRuntimePolicy{
		VCPUs:        1,
		CPUMillis:    1000,
		MemoryMiB:    512,
		ProcessLimit: 128,
	}
}

func TestStageSharedTemplateFileSharesOneInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "memory.snap")
	if err := os.WriteFile(source, []byte("golden-memory"), 0o444); err != nil {
		t.Fatalf("write golden memory: %v", err)
	}
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	for _, jail := range []string{first, second} {
		if err := os.Mkdir(jail, 0o700); err != nil {
			t.Fatalf("create jail %q: %v", jail, err)
		}
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

// An unjailed restore opens the template source's absolute drive paths during
// the load itself, so it can neither be given per-Instance disks nor repaired
// afterwards. It must fail before any file is staged.
func TestPrepareSnapshotResumeLaunchRefusesUnjailedResume(t *testing.T) {
	_, template := newResumeTestTemplate(t)
	runDir := shortResumeDir(t)
	manager := newJailedResumeManager(t, runDir)
	manager.cfg.MicroVMAllowUnjailed = true
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	_, err := manager.prepareSnapshotResumeLaunch(
		"fc-resume",
		instanceDir,
		template,
		filepath.Join(runDir, "workspace.ext4"),
		"",
		12000,
		resumeTestPolicy(),
	)
	if err == nil {
		t.Fatal("an unjailed resume was accepted")
	}
	if !strings.Contains(err.Error(), "snapshot resume requires the jailer") {
		t.Fatalf("error does not name the jailer requirement: %v", err)
	}
	entries, readErr := os.ReadDir(instanceDir)
	if readErr != nil {
		t.Fatalf("read instance dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused resume staged %d files", len(entries))
	}
}

func TestPrepareSnapshotResumeLaunchRefusesUnusableTemplates(t *testing.T) {
	runDir := shortResumeDir(t)
	manager := newJailedResumeManager(t, runDir)
	instanceDir := filepath.Join(runDir, "fc-resume")
	if err := os.Mkdir(instanceDir, 0o700); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	workspacePath := filepath.Join(runDir, "workspace.ext4")

	t.Run("absent template", func(t *testing.T) {
		_, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume", instanceDir, nil, workspacePath, "", 12000, resumeTestPolicy(),
		)
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
			12000,
			resumeTestPolicy(),
		); err == nil {
			t.Fatal("an unadmitted template was accepted")
		}
	})

	t.Run("template replaced after admission", func(t *testing.T) {
		cache, template := newResumeTestTemplate(t)
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
			"fc-resume", instanceDir, template, workspacePath, "", 12000, resumeTestPolicy(),
		); err == nil {
			t.Fatal("a template replaced after admission was accepted")
		}
	})

	t.Run("absent workspace", func(t *testing.T) {
		_, template := newResumeTestTemplate(t)
		if _, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume", instanceDir, template, "  ", "", 12000, resumeTestPolicy(),
		); err == nil {
			t.Fatal("a resume without a Workspace image was accepted")
		}
	})

	t.Run("no jailer uid", func(t *testing.T) {
		_, template := newResumeTestTemplate(t)
		if _, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume", instanceDir, template, workspacePath, "", 0, resumeTestPolicy(),
		); err == nil {
			t.Fatal("a resume without a jailer UID was accepted")
		}
	})

	t.Run("no runtime policy", func(t *testing.T) {
		_, template := newResumeTestTemplate(t)
		if _, err := manager.prepareSnapshotResumeLaunch(
			"fc-resume", instanceDir, template, workspacePath, "", 12000, nil,
		); err == nil {
			t.Fatal("a resume without a runtime policy was accepted")
		}
	})
}

func TestSnapshotResumeLoadRequestUsesChrootRelativePaths(t *testing.T) {
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
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}, "   ")
	if len(request.NetworkOverrides) != 0 {
		t.Fatalf("network overrides = %+v, want none", request.NetworkOverrides)
	}
}

func TestResumeSnapshotTemplateLoadsAndResumesInOneCall(t *testing.T) {
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
		t.Fatalf("resume snapshot template: %v", err)
	}
	call := drainAPICalls(seen, 1)[0]
	if call.Path != "/snapshot/load" || call.Body["resume_vm"] != true || call.Body["clock_realtime"] != true {
		t.Fatalf("resume load call = %#v", call)
	}
	select {
	case extra := <-seen:
		t.Fatalf("resume issued an extra call: %#v", extra)
	case <-time.After(50 * time.Millisecond):
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

func TestTemplateGuestBootArgsCarryNoAssignmentIdentity(t *testing.T) {
	manager := &Manager{cfg: &config.Config{
		MicroVMGuestControlVsockPort:  1024,
		MicroVMGuestProtocolVsockPort: 1025,
	}}
	args, err := manager.templateGuestBootArgs()
	if err != nil {
		t.Fatalf("template guest boot arguments: %v", err)
	}
	for _, required := range []string{
		"secondbox.template_mode=1",
		"secondbox.guest_control_vsock_port=1024",
		"secondbox.guest_protocol_vsock_port=1025",
	} {
		if !strings.Contains(args, required) {
			t.Errorf("template boot arguments are missing %q: %s", required, args)
		}
	}
	// A template is captured before any Sandbox exists. Identity in these
	// arguments would be sealed into the shared memory image.
	for _, forbidden := range []string{
		"secondbox.instance_id",
		"secondbox.sandbox_id",
		"secondbox.sandbox_generation",
		"secondbox.guest_build_id",
		"secondbox.image_manifest_digest",
		"secondbox.toolchain_manifest_digest",
		"secondbox.guest_heartbeat_interval",
	} {
		if strings.Contains(args, forbidden) {
			t.Errorf("template boot arguments carry assignment identity %q: %s", forbidden, args)
		}
	}

	unconfigured := &Manager{cfg: &config.Config{
		MicroVMGuestControlVsockPort:  1024,
		MicroVMGuestProtocolVsockPort: 1024,
	}}
	if _, err := unconfigured.templateGuestBootArgs(); err == nil {
		t.Fatal("template boot accepted identical control and protocol vsock ports")
	}
}

func TestStartupGuestBootArgsSelectsTemplateModeOnly(t *testing.T) {
	manager := &Manager{cfg: &config.Config{
		MicroVMGuestControlVsockPort:  1024,
		MicroVMGuestProtocolVsockPort: 1025,
		MicroVMGuestHeartbeatInterval: time.Second,
	}}
	opts := runtimemanager.StartOpts{
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
	}
	cold, err := manager.startupGuestBootArgs("instance-1", "sandbox-1", opts)
	if err != nil {
		t.Fatalf("cold boot arguments: %v", err)
	}
	if !strings.Contains(cold, "secondbox.sandbox_id=sandbox-1") || strings.Contains(cold, "secondbox.template_mode") {
		t.Fatalf("cold boot arguments = %s", cold)
	}
	opts.TemplateMode = true
	template, err := manager.startupGuestBootArgs("instance-1", "sandbox-1", opts)
	if err != nil {
		t.Fatalf("template boot arguments: %v", err)
	}
	if !strings.Contains(template, "secondbox.template_mode=1") || strings.Contains(template, "sandbox-1") {
		t.Fatalf("template boot arguments = %s", template)
	}
}

// TestAssignmentStartupModeRefusesWhatThisRunnerCannotHonour pins the fail-closed
// gate. Cold boot is the runner's one implemented start path; a snapshot_resume
// assignment is refused with the same retryable template-unavailability the
// resume path raises, because quietly cold booting it would be exactly the
// silent fallback a snapshot_resume Profile is defined not to have.
func TestAssignmentStartupModeRefusesWhatThisRunnerCannotHonour(t *testing.T) {
	if err := validateAssignmentStartupMode(assignmentStartupModeColdBoot); err != nil {
		t.Fatalf("cold_boot startup mode was refused: %v", err)
	}
	err := validateAssignmentStartupMode(assignmentStartupModeSnapshotResume)
	if !errors.Is(err, ErrSnapshotTemplateUnavailable) {
		t.Fatalf("snapshot_resume error = %v, want ErrSnapshotTemplateUnavailable", err)
	}
	for _, mode := range []string{"", "ephemeral", "warm_pool"} {
		if err := validateAssignmentStartupMode(mode); err == nil {
			t.Errorf("startup mode %q was accepted", mode)
		}
	}
}
