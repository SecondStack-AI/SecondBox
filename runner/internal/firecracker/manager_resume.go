package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
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
	workspacePath string,
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
	if strings.TrimSpace(workspacePath) == "" {
		return snapshotResumeLaunch{}, fmt.Errorf("resume Workspace image path is required")
	}

	if m.cfg.MicroVMAllowUnjailed {
		socket := filepath.Join(dir, firecrackerSockName)
		vsockUDS := filepath.Join(dir, vsockUDSName)
		if err := checkUnixSocketPath("firecracker api", socket, "SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR"); err != nil {
			return snapshotResumeLaunch{}, err
		}
		if err := checkUnixSocketPath("vsock", vsockUDS, "SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR"); err != nil {
			return snapshotResumeLaunch{}, err
		}
		// An unjailed Firecracker opens absolute host paths, so the template is
		// referenced where it lives. That is the strongest form of the sharing
		// the jailed path approximates with a hard link: one inode, one page
		// cache, no per-Instance file at all.
		if err := stageSnapshotResumeInstanceFiles(dir, template, workspacePath, sharedImagePath, 0, 0, false); err != nil {
			return snapshotResumeLaunch{}, err
		}
		return snapshotResumeLaunch{
			executable:          m.cfg.FirecrackerPath,
			args:                []string{"--id", instanceID, "--api-sock", socket},
			socketPath:          socket,
			vsockUDS:            vsockUDS,
			vmStateResolvedPath: template.VMStatePath,
			memoryResolvedPath:  template.MemoryPath,
			vsockResolvedPath:   vsockUDS,
		}, nil
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
	if policy == nil || policy.CPUMillis < 1 || policy.ProcessLimit < 1 || policy.VCPUs < 1 || policy.MemoryMiB < 1 {
		return snapshotResumeLaunch{}, fmt.Errorf("profile CPU, memory, and process limits are required for jailed resume")
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
		workspacePath,
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
	workspacePath string,
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
	if err := hardLinkFile(workspacePath, stagedWorkspace); err != nil {
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
