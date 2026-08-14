package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// The names a restored Instance opens are part of the template contract. A
// jailed Firecracker runs with the chroot as its working directory, so the VM
// state records chroot-relative drive and vsock paths and a restored Instance
// opens its own jail's file at the same name. Staging, not PATCH /drives, is
// therefore how a per-Sandbox Workspace reaches a resumed guest.
const (
	snapshotResumeVMStateName = snapshotTemplateVMStateName
	snapshotResumeMemoryName  = snapshotTemplateMemoryName

	snapshotResumeNetworkInterfaceID = "eth0"

	// snapshotTemplateGuestMAC is the MAC a template's interface is captured
	// with. It is a compatibility-keyed constant for the same reason the guest
	// CID and the vsock port numbers are: Firecracker's snapshot load overrides
	// only an interface's host TAP, never its guest MAC, so the captured value
	// reaches every resumed Instance and must be reproducible rather than derived
	// from whichever TAP the template build happened to allocate. Every Instance
	// replaces it with a unique per-Sandbox MAC at its assignment bind, before
	// the link comes up.
	snapshotTemplateGuestMAC = "02:00:00:5b:7e:00"

	// snapshotTemplateGuestCID is the guest context ID every VM is configured
	// with. Like the vsock port numbers it is a compatibility-keyed constant:
	// only the UDS path changes per Instance.
	snapshotTemplateGuestCID = 3
)

// ErrSnapshotTemplateUnavailable marks a resume that cannot proceed because the
// exact compatible template is not materialized on this runner. It is retryable
// and it is never a reason to cold boot: a snapshot_resume Profile has no
// fallback execution path.
var ErrSnapshotTemplateUnavailable = errors.New("SecondBox snapshot resume template is unavailable")

// reflinkRootfsChild is a package variable for the same reason hardLinkFile is:
// staging behaviour must be exercisable on a filesystem the unit suite can
// create. Production always uses reflinkOnlyFile and there is no byte-copy
// fallback.
var reflinkRootfsChild = reflinkOnlyFile

// snapshotResumeLaunch is a prepared, staged Firecracker process that has not
// been started yet. Every path it exposes is the one the restored VM will use,
// expressed the way Firecracker will resolve it.
type snapshotResumeLaunch struct {
	executable  string
	args        []string
	environment []string
	socketPath  string
	vsockUDS    string
	jailRoot    string
	// vmStateResolvedPath and memoryResolvedPath are what the load request
	// carries: chroot-relative under the jailer, absolute when unjailed.
	vmStateResolvedPath string
	memoryResolvedPath  string
	vsockResolvedPath   string
}

// stageSharedTemplateFile links an immutable template file into an Instance's
// jail. It is a hard link on purpose. A reflink clone is a distinct inode and
// distinct inodes share no page cache, which is exactly the per-inode page-in
// that makes 32 concurrent cold boots read the same rootfs bytes 32 times. One
// inode for the golden memory file means the first resume's resident set is
// every later resume's cache hit.
//
// The link is never chowned. Template files are world-readable and owned by the
// runner; chowning would hand one Instance's jailer UID ownership of an
// artifact every other Instance depends on.
func stageSharedTemplateFile(destination, source string) error {
	_ = os.Remove(destination)
	if err := hardLinkFile(source, destination); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf(
				"link snapshot template file into jail: %w (the snapshot template cache root must be on the same filesystem as SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT so the golden memory file keeps one inode and one page cache)",
				err,
			)
		}
		return fmt.Errorf("link snapshot template file into jail: %w", err)
	}
	return nil
}

// sharesInode reports whether two paths are the same file. The resume path
// depends on it for the memory backing file, so it is checked rather than
// assumed.
func sharesInode(first, second string) (bool, error) {
	firstIdentity, err := trustedMicroVMArtifactIdentityFor(first)
	if err != nil {
		return false, err
	}
	secondIdentity, err := trustedMicroVMArtifactIdentityFor(second)
	if err != nil {
		return false, err
	}
	return firstIdentity.dev == secondIdentity.dev && firstIdentity.ino == secondIdentity.ino, nil
}

// prepareSnapshotResumeLaunch stages every file a restored Instance opens and
// returns the process it will run. It does not start the process and it does
// not touch the Firecracker API.
func (m *Manager) prepareSnapshotResumeLaunch(
	instanceID string,
	dir string,
	template *AdmittedSnapshotTemplate,
	workspace workspacestore.ComputeAttachment,
	sharedImagePath string,
	jailerUID int,
	policy *runtimemanager.SandboxRuntimePolicy,
) (snapshotResumeLaunch, error) {
	if template == nil {
		return snapshotResumeLaunch{}, fmt.Errorf("%w: no template was resolved", ErrSnapshotTemplateUnavailable)
	}
	if err := template.VerifyStableIdentity(); err != nil {
		return snapshotResumeLaunch{}, err
	}
	if workspace == nil || workspace.Descriptor() == nil {
		return snapshotResumeLaunch{}, fmt.Errorf("resume Workspace attachment is required")
	}

	// Snapshot resume requires the jailer, and this is a property of the VMM,
	// not a policy choice. Firecracker opens every block device at the path the
	// VM state recorded, during the load itself:
	//
	//   Failed to restore devices: Error restoring MMIO devices: Block: Virtio
	//   backend error: Error manipulating the backing file: No such file or
	//   directory (os error 2) /.../rootfs.ext4
	//
	// so PATCH /drives cannot repair it — the load has already failed by then.
	// Under the jailer the recorded paths are chroot-relative names that each
	// Instance resolves inside its own jail. Unjailed they are the template
	// source's absolute paths, which one Instance at most could ever own.
	if m.cfg.MicroVMAllowUnjailed {
		return snapshotResumeLaunch{}, fmt.Errorf(
			"snapshot resume requires the jailer: restored VM state records absolute drive paths that cannot be made per-Instance, so SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED must be false",
		)
	}

	jailRoot := m.jailerRoot(instanceID)
	socket := filepath.Join(jailRoot, firecrackerSockName)
	vsockUDS := filepath.Join(jailRoot, vsockUDSName)
	if err := checkUnixSocketPath("jailed firecracker api", socket, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"); err != nil {
		return snapshotResumeLaunch{}, err
	}
	if err := checkUnixSocketPath("jailed vsock", vsockUDS, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"); err != nil {
		return snapshotResumeLaunch{}, err
	}
	if jailerUID < 1 {
		return snapshotResumeLaunch{}, fmt.Errorf("per-instance jailer UID is required")
	}
	if policy == nil || policy.VCPUs < 1 || policy.MemoryMiB < 1 {
		return snapshotResumeLaunch{}, fmt.Errorf("profile vCPU and memory limits are required for jailed resume")
	}
	if err := os.MkdirAll(jailRoot, 0o700); err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("create jail root: %w", err)
	}
	if err := stageSharedTemplateFile(filepath.Join(jailRoot, snapshotResumeVMStateName), template.VMStatePath); err != nil {
		_ = os.RemoveAll(jailRoot)
		return snapshotResumeLaunch{}, err
	}
	if err := stageSharedTemplateFile(filepath.Join(jailRoot, snapshotResumeMemoryName), template.MemoryPath); err != nil {
		_ = os.RemoveAll(jailRoot)
		return snapshotResumeLaunch{}, err
	}
	if err := stageSnapshotResumeInstanceFiles(
		jailRoot,
		template,
		workspace,
		sharedImagePath,
		jailerUID,
		m.cfg.MicroVMJailerGID,
		true,
	); err != nil {
		_ = os.RemoveAll(jailRoot)
		return snapshotResumeLaunch{}, err
	}
	args := m.jailerArgsWithPolicy(instanceID, jailerUID, policy)
	args = append(args, "--", "--api-sock", firecrackerSockName)
	supervisorEnvironment, err := jailersupervisor.CommandEnvironment(m.cfg.JailerPath, args)
	if err != nil {
		_ = os.RemoveAll(jailRoot)
		return snapshotResumeLaunch{}, err
	}
	return snapshotResumeLaunch{
		executable:  "/proc/self/exe",
		args:        []string{jailersupervisor.InvocationArgument},
		environment: []string{supervisorEnvironment},
		socketPath:  socket,
		vsockUDS:    vsockUDS,
		jailRoot:    jailRoot,
		// Firecracker resolves these relative to the chroot it is running in.
		vmStateResolvedPath: snapshotResumeVMStateName,
		memoryResolvedPath:  snapshotResumeMemoryName,
		vsockResolvedPath:   vsockUDSName,
	}, nil
}

// stageSnapshotResumeInstanceFiles places the per-Instance files a restored VM
// opens under the exact names the template recorded.
func stageSnapshotResumeInstanceFiles(
	root string,
	template *AdmittedSnapshotTemplate,
	workspace workspacestore.ComputeAttachment,
	sharedImagePath string,
	uid int,
	gid int,
	chownStagedFiles bool,
) error {
	// The sealed post-boot rootfs is immutable; the restored guest writes to it,
	// so each Instance gets its own copy-on-write child. There is no byte-copy
	// fallback: a run directory on a filesystem without reflink support is a
	// misconfiguration, not a slow path.
	stagedRootfs := filepath.Join(root, snapshotTemplateRootfsName)
	if err := reflinkRootfsChild(stagedRootfs, template.RootfsPath, 0o600); err != nil {
		return fmt.Errorf("prepare resume rootfs child: %w", err)
	}
	stagedWorkspace := filepath.Join(root, workspaceName)
	_ = os.Remove(stagedWorkspace)
	if err := workspace.LinkInto(stagedWorkspace); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf(
				"link Workspace image for resume: %w (the jailer chroot base dir must be on the same filesystem as SECONDBOX_RUNNER_WORKSPACE_ROOT)",
				err,
			)
		}
		return fmt.Errorf("link Workspace image for resume: %w", err)
	}
	stagedShared := ""
	if strings.TrimSpace(sharedImagePath) != "" {
		stagedShared = filepath.Join(root, sharedImageName)
		if err := copyFile(stagedShared, sharedImagePath, 0o600); err != nil {
			return fmt.Errorf("stage shared image for resume: %w", err)
		}
	}
	if !chownStagedFiles {
		return nil
	}
	for _, path := range []string{stagedRootfs, stagedWorkspace, stagedShared} {
		if path == "" {
			continue
		}
		if err := chownIfDifferent(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// waitForUnixSocket blocks until a unix socket accepts a connection. A restored
// Instance's API socket appears only after the VMM process is up, and the
// resume path has no configuration file to fall back on.
func waitForUnixSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		connection, err := net.DialTimeout("unix", path, 10*time.Millisecond)
		if err == nil {
			return connection.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Firecracker API socket: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// snapshotResumeLoadRequest is the exact request that restores an Instance. The
// template's guest CID and vsock port numbers are compatibility-keyed
// constants; only the socket path, the TAP device, and the wall clock change
// per Instance.
func snapshotResumeLoadRequest(launch snapshotResumeLaunch, tapName string) snapshotLoadRequest {
	request := snapshotLoadRequest{
		SnapshotPath: launch.vmStateResolvedPath,
		MemBackend: &memoryBackend{
			BackendPath: launch.memoryResolvedPath,
			BackendType: "File",
		},
		ResumeVM:      true,
		VsockOverride: &vsockOverride{UDSPath: launch.vsockResolvedPath},
		ClockRealtime: true,
	}
	if tapName = strings.TrimSpace(tapName); tapName != "" {
		request.NetworkOverrides = []networkOverride{{
			IfaceID:     snapshotResumeNetworkInterfaceID,
			HostDevName: tapName,
		}}
	}
	return request
}

// resumeSnapshotTemplate waits for the restored process's API socket and loads
// the template into it. It performs no retry and no fallback: a failed load is
// a failed start.
//
// The load opens every block device at the chroot-relative name the VM state
// recorded, which resolves inside this Instance's own jail, so one call both
// attaches this Instance's disks and resumes it.
func resumeSnapshotTemplate(
	ctx context.Context,
	launch snapshotResumeLaunch,
	tapName string,
	apiReadyTimeout time.Duration,
	loadTimeout time.Duration,
) error {
	if err := waitForUnixSocket(ctx, launch.socketPath, apiReadyTimeout); err != nil {
		return fmt.Errorf("wait for restored Firecracker API socket: %w", err)
	}
	client := FirecrackerAPIClient{SocketPath: launch.socketPath, Timeout: loadTimeout}
	if err := client.LoadSnapshotWithOptions(ctx, snapshotResumeLoadRequest(launch, tapName)); err != nil {
		return fmt.Errorf("load snapshot template: %w", err)
	}
	return nil
}

// snapshotResumeAPIReadyTimeout bounds the jailed VMM's chroot, device-node,
// exec-file, cgroup, and UID-drop work before its API socket answers. The
// qualified jailed gate measured 32 ms at one Instance and 179 ms at sixteen.
const snapshotResumeAPIReadyTimeout = 30 * time.Second

// snapshotResumeLoadTimeout bounds one file-backed snapshot load. The qualified
// floor is 3-4 ms warm at every memory shape and 5 ms cache-evicted.
const snapshotResumeLoadTimeout = 120 * time.Second

// snapshotResumeTemplateKey derives the exact compatibility identity this start
// needs. Every field comes from configuration the runner already verified or
// from the Profile shape the assignment carries; nothing is hashed here. The
// kernel, rootfs, and shared-image digests are read from the signed manifest,
// whose signature Config.ValidateMicroVMTrustAnchor verified against the local
// files before the Manager existed, so a start never rehashes an 11 GiB bundle.
//
// A key this runner cannot state is a template it cannot resolve, so every
// failure here is the same typed unavailability a cache miss raises.
func (m *Manager) snapshotResumeTemplateKey(
	opts runtimemanager.StartOpts,
	hasNetworkDevice bool,
	hasSharedImage bool,
) (SnapshotTemplateKey, error) {
	policy := opts.SandboxPolicy
	if policy == nil {
		return SnapshotTemplateKey{}, fmt.Errorf(
			"%w: a snapshot-resume start requires the Profile CPU, memory, Workspace, and process shape",
			ErrSnapshotTemplateUnavailable,
		)
	}
	artifactDir := filepath.Dir(m.cfg.MicroVMKernelPath)
	manifestPath := filepath.Join(artifactDir, "manifest.json")
	manifest, err := loadSignedArtifactManifest(manifestPath)
	if err != nil {
		return SnapshotTemplateKey{}, fmt.Errorf(
			"%w: read signed bundle manifest: %w", ErrSnapshotTemplateUnavailable, err,
		)
	}
	manifestDigest, err := fileSHA256(manifestPath)
	if err != nil {
		return SnapshotTemplateKey{}, fmt.Errorf(
			"%w: digest signed bundle manifest: %w", ErrSnapshotTemplateUnavailable, err,
		)
	}
	cpuFingerprint, err := hostCPUCompatibilityFingerprint()
	if err != nil {
		return SnapshotTemplateKey{}, fmt.Errorf(
			"%w: read host CPU compatibility fingerprint: %w", ErrSnapshotTemplateUnavailable, err,
		)
	}
	templateBootArgs, err := m.templateGuestBootArgs()
	if err != nil {
		return SnapshotTemplateKey{}, fmt.Errorf(
			"%w: %w", ErrSnapshotTemplateUnavailable, err,
		)
	}
	networkInterfaceID, templateGuestMAC := "", ""
	if hasNetworkDevice {
		networkInterfaceID, templateGuestMAC = snapshotResumeNetworkInterfaceID, snapshotTemplateGuestMAC
	}
	sharedImageSHA256 := ""
	if hasSharedImage {
		sharedImageSHA256 = manifest.Shared.SHA256
	}
	key := SnapshotTemplateKey{
		ArtifactVersion:       manifest.ArtifactVersion,
		Architecture:          manifest.Architecture,
		SigningKeyFingerprint: m.cfg.MicroVMPublicKeySHA256,
		SignedManifestDigest:  "sha256:" + manifestDigest,
		KernelSHA256:          manifest.Kernel.SHA256,
		// The template's own boot arguments, which carry no Sandbox identity and
		// no guest address. A resumed guest's kernel finished booting before the
		// Sandbox existed, so its identity arrives at the assignment bind.
		KernelArgs:              strings.TrimSpace(effectiveKernelArgs(m.cfg, "") + " " + templateBootArgs),
		SourceRootfsSHA256:      manifest.Rootfs.SHA256,
		SharedImageSHA256:       sharedImageSHA256,
		RuntimeBundleDigest:     opts.ImageManifestDigest,
		ToolchainBundleDigest:   opts.ToolchainManifestDigest,
		GuestBuildID:            opts.GuestBuildID,
		GuestProtocolGeneration: currentGuestProtocolGeneration,
		GuestFeatures:           append([]string(nil), requestedGuestProtocolFeatureNames...),
		ComputeBackendVersion:   expectedComputeBackendVersionString(),
		HostCPUFingerprint:      cpuFingerprint,
		CPUTemplate:             m.cfg.MicroVMCPUTemplate,
		VCPUCount:               policy.VCPUs,
		MemorySizeMiB:           policy.MemoryMiB,
		WorkspaceSizeMiB:        policy.WorkspaceSizeMiB,

		RuntimeClass:           string(opts.RuntimeClass),
		NetworkInterfaceID:     networkInterfaceID,
		TemplateGuestMAC:       templateGuestMAC,
		GuestControlVsockPort:  m.cfg.MicroVMGuestControlVsockPort,
		GuestProtocolVsockPort: m.cfg.MicroVMGuestProtocolVsockPort,
		GuestCID:               snapshotTemplateGuestCID,
	}
	if err := key.Validate(); err != nil {
		return SnapshotTemplateKey{}, fmt.Errorf("%w: %w", ErrSnapshotTemplateUnavailable, err)
	}
	return key, nil
}

// resolveSnapshotResumeTemplate turns this start's compatibility key into an
// admitted template. A miss, an incompatible shape, a corrupted generation, or
// a template that changed since admission is the same typed retryable failure,
// and never a cold boot.
func (m *Manager) resolveSnapshotResumeTemplate(
	opts runtimemanager.StartOpts,
	hasNetworkDevice bool,
	hasSharedImage bool,
) (*AdmittedSnapshotTemplate, error) {
	if m.snapshotTemplates == nil {
		return nil, fmt.Errorf(
			"%w: this Runner has no snapshot template cache; set SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT",
			ErrSnapshotTemplateUnavailable,
		)
	}
	key, err := m.snapshotResumeTemplateKey(opts, hasNetworkDevice, hasSharedImage)
	if err != nil {
		return nil, err
	}
	template, err := m.snapshotTemplates.Resolve(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSnapshotTemplateUnavailable, err)
	}
	return template, nil
}

// createAndStartResume is the start path a snapshot_resume Profile revision
// takes. It forks below WorkspaceStore.Open exactly where cold start does, so
// the exclusive writer lock and the generation fence are already held, and it
// shares the prologue and epilogue with cold start rather than reproducing
// them: a divergence between the two paths' isolation properties is the failure
// this design cannot tolerate.
//
// There is no retry inside the load, no second attempt with different
// parameters, and no cold boot. Every failure tears the Instance down with its
// TAP, host policy, guest address, staged files, jail, and Workspace attachment
// and reports the same typed retryable unavailability.
func (m *Manager) createAndStartResume(
	ctx context.Context,
	sandboxID string,
	compartmentID string,
	opts runtimemanager.StartOpts,
	onRegisteredLocked func(),
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if opts.WorkspaceAttachment == nil {
		return "", fmt.Errorf("SecondBox Firecracker Workspace attachment is required")
	}
	if opts.TemplateMode {
		return "", fmt.Errorf("a snapshot-resume template is captured from a cold boot, never resumed from one")
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	opts.CompartmentID = compartmentID
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelSetup()

	host, err := m.reserveInstanceHost(ctx, setupCtx, sandboxID, compartmentID, opts)
	if err != nil {
		return "", err
	}
	defer host.release()
	id, dir, timer := host.id, host.dir, host.timer

	workspace, err := m.resolveWorkspaceAttachment(opts)
	if err != nil {
		return "", host.joinNetworkCleanup(setupCtx, err)
	}
	timer.mark("workspace_ready", "workspaceId", workspace.attachment.WorkspaceID())

	// The shared image is the runner's own verified artifact, staged per
	// Instance exactly as cold start stages it.
	sharedImagePath := ""
	if workspace.sharedReadOnly {
		image, imageErr := m.microVMImageForStart(opts)
		if imageErr != nil {
			return "", host.joinNetworkCleanup(setupCtx, imageErr)
		}
		sharedImagePath = image.SharedImagePath
	}
	template, err := m.resolveSnapshotResumeTemplate(opts, host.tapName != "", sharedImagePath != "")
	if err != nil {
		return "", host.joinNetworkCleanup(setupCtx, err)
	}
	timer.mark("snapshot_template_resolved", "template", template.TemplateID)

	startupFingerprint, err := m.startupFingerprint(sandboxID, compartmentID, opts)
	if err != nil {
		return "", host.joinNetworkCleanup(setupCtx, fmt.Errorf("build startup fingerprint: %w", err))
	}
	if err := m.writeIdentityFile(dir, id, sandboxID, opts); err != nil {
		return "", host.joinNetworkCleanup(setupCtx, err)
	}

	launch, err := m.prepareSnapshotResumeLaunch(
		id,
		dir,
		template,
		workspace.attachment,
		sharedImagePath,
		host.jailerUID,
		opts.SandboxPolicy,
	)
	if err != nil {
		return "", host.joinNetworkCleanup(setupCtx, fmt.Errorf("%w: %w", ErrSnapshotTemplateUnavailable, err))
	}
	timer.mark("snapshot_jail_staged", "jailRoot", launch.jailRoot)

	cmd := exec.Command(launch.executable, launch.args...)
	cmd.Dir = dir
	cmd.Stdout = host.logFile
	cmd.Stderr = host.logFile
	if len(launch.environment) != 0 {
		cmd.Env = append(os.Environ(), launch.environment...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if startErr := cmd.Start(); startErr != nil {
		_ = os.RemoveAll(launch.jailRoot)
		return "", host.joinNetworkCleanup(setupCtx, fmt.Errorf(
			"%w: start jailed Firecracker for resume: %w", ErrSnapshotTemplateUnavailable, startErr,
		))
	}
	timer.mark("firecracker_process_started", "pid", cmd.Process.Pid)

	// Register before the load so the reaper owns the process from its first
	// instant and every later failure tears down through one path.
	inst, err := m.registerLaunchedInstance(
		setupCtx,
		host,
		sandboxID,
		compartmentID,
		opts,
		launchedInstanceFiles{
			socketPath:      launch.socketPath,
			vsockUDS:        launch.vsockUDS,
			jailRoot:        launch.jailRoot,
			rootfsPath:      filepath.Join(launch.jailRoot, snapshotTemplateRootfsName),
			rootfsImagePath: template.RootfsPath,
			workspacePath:   filepath.Join(launch.jailRoot, workspaceName),
			sharedImagePath: sharedImagePath,
		},
		startupFingerprint,
		cmd,
		onRegisteredLocked,
	)
	if err != nil {
		return "", err
	}

	if err := m.resumeInstanceGuest(setupCtx, inst, opts, launch, timer); err != nil {
		diagnostics := inst.logTailDiagnostics(120)
		cleanupErr := m.stopInstance(setupCtx, inst, true)
		return "", errors.Join(
			fmt.Errorf("%w: %w%s", ErrSnapshotTemplateUnavailable, err, diagnostics),
			cleanupErr,
		)
	}
	if err := m.completeInstanceStartup(setupCtx, inst, sandboxID, opts, timer); err != nil {
		return "", err
	}
	host.transferOwnership() // ownership transfers to the running instance
	slog.Info(
		"resumed firecracker microVM from snapshot template",
		"sandbox", sandboxID,
		"compartment", compartmentID,
		"instance", id,
		"template", template.TemplateID,
		"elapsedMs", timer.elapsedMs(),
		"log", host.logPath,
	)
	return id, nil
}

// resumeInstanceGuest loads the template into the started process and brings the
// guest from restored to bound: first control response, post-resume hardening,
// then the one permitted assignment bind carrying this Instance's identity,
// Workspace expectation, and network identity. The caller tears the Instance
// down on any failure.
func (m *Manager) resumeInstanceGuest(
	ctx context.Context,
	inst *instance,
	opts runtimemanager.StartOpts,
	launch snapshotResumeLaunch,
	timer *coldStartStageTimer,
) error {
	if err := resumeSnapshotTemplate(
		ctx,
		launch,
		inst.tapName,
		snapshotResumeAPIReadyTimeout,
		snapshotResumeLoadTimeout,
	); err != nil {
		return err
	}
	timer.mark("snapshot_loaded")
	controlCtx, cancelControl := context.WithTimeout(ctx, controlPlaneReadyTimeout)
	controlErr := m.waitForControlPlane(controlCtx, inst, controlPlaneReadyTimeout)
	cancelControl()
	if controlErr != nil {
		return fmt.Errorf("wait for resumed guest control plane: %w", controlErr)
	}
	timer.mark("resumed_control_plane_ready")
	controlClient := inst.controlClient(controlPlaneReadyTimeout)
	// Hardening is the first permitted post-resume action and the precondition
	// of the bind: the guest refuses to install an identity it could have
	// generated template-era randomness under.
	if err := controlClient.HardenPostRestore(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("harden resumed guest: %w", err)
	}
	m.mu.Lock()
	inst.postResumeHardened = true
	m.mu.Unlock()
	timer.mark("post_resume_hardened")
	bind := AssignmentBindRequest{
		InstanceID:              inst.compartmentID,
		SandboxID:               inst.sandboxID,
		SandboxGeneration:       inst.sandboxGeneration,
		GuestBuildID:            opts.GuestBuildID,
		ImageManifestDigest:     opts.ImageManifestDigest,
		ToolchainManifestDigest: opts.ToolchainManifestDigest,
		HeartbeatIntervalMs:     uint64(m.cfg.MicroVMGuestHeartbeatInterval / time.Millisecond),
		WorkspaceWritable:       opts.SandboxPolicy != nil && opts.SandboxPolicy.WorkspaceWritable,
	}
	// The network identity is present exactly when this Instance has a TAP. A
	// resumed guest's kernel consumed no ip= argument, so its address, route,
	// and unique MAC arrive here or not at all.
	if strings.TrimSpace(inst.tapName) != "" {
		bind.Network = &AssignmentNetworkIdentity{
			Interface:   snapshotResumeNetworkInterfaceID,
			MACAddress:  guestMACForInstance(inst.tapName),
			AddressCIDR: guestAddressCIDR(inst.guestIP, m.cfg.MicroVMBridgeCIDR),
			Gateway:     bridgeAddress(m.cfg.MicroVMBridgeCIDR).String(),
			Nameserver:  bridgeAddress(m.cfg.MicroVMBridgeCIDR).String(),
		}
	}
	if err := controlClient.BindAssignment(ctx, bind); err != nil {
		return fmt.Errorf("bind resumed guest assignment: %w", err)
	}
	timer.mark("assignment_bound")
	return nil
}
