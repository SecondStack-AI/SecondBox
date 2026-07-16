package microvm

import (
	"agentcy/internal/config"
	"agentcy/internal/registry"
	"agentcy/internal/runtimecontext"
	"agentcy/internal/runtimemanager"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func buildPrivilegedLaunchRequest(instanceID, agentID, compartmentID string, launchImage, sourceImage microVMImageSelection, workspacePath, tapName, guestIP string) privilegedLaunchRequest {
	return privilegedLaunchRequest{
		InstanceID:    instanceID,
		AgentID:       agentID,
		CompartmentID: compartmentID,
		RootfsPath:    launchImage.RootfsPath,
		RootfsImage:   sourceImage.RootfsPath,
		WorkspacePath: workspacePath,
		SharedImage:   sourceImage.SharedImagePath,
		TapName:       tapName,
		GuestIP:       guestIP,
	}
}

// relocateRunDirForUnixSockets returns a shorter run dir (and true) when runDir is
// deep enough that appending the per-instance socket suffix would exceed the unix
// socket path limit. The run dir also holds large rootfs copies, so avoid
// XDG_RUNTIME_DIR: it is commonly a small tmpfs and cannot hold an 8G VM image.
// Returns ("", false) when runDir already fits.
func relocateRunDirForUnixSockets(runDir string) (string, bool) {
	abs := runDir
	if a, err := filepath.Abs(runDir); err == nil {
		abs = a
	}
	if len(abs)+reservedRunDirBudget < maxUnixSocketPathLen {
		return "", false
	}
	for _, candidate := range shortMicroVMRunDirCandidates() {
		if len(candidate)+reservedRunDirBudget < maxUnixSocketPathLen {
			return candidate, true
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agentcy-%d", os.Getuid()), "run"), true
}

func shortMicroVMRunDirCandidates() []string {
	var candidates []string
	if cacheDir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cacheDir) != "" {
		candidates = append(candidates, filepath.Join(cacheDir, "agentcy", "microvm", "run"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), fmt.Sprintf("agentcy-%d", os.Getuid()), "run"))
	return candidates
}

// ensureShortRunDirAlias keeps the run directory on its configured filesystem
// (so multi-gigabyte rootfs images can be reflinked) while exposing it through a
// path short enough for Firecracker's unix sockets.
func ensureShortRunDirAlias(alias, target string) error {
	alias, err := filepath.Abs(alias)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	if alias == target {
		return os.MkdirAll(target, 0o700)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create target %q: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		return fmt.Errorf("create alias parent %q: %w", filepath.Dir(alias), err)
	}
	info, err := os.Lstat(alias)
	if err == nil {
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			destination, readErr := os.Readlink(alias)
			if readErr == nil && !filepath.IsAbs(destination) {
				destination = filepath.Join(filepath.Dir(alias), destination)
			}
			if readErr == nil && filepath.Clean(destination) == target {
				return nil
			}
			if removeErr := os.Remove(alias); removeErr != nil {
				return fmt.Errorf("replace alias %q: %w", alias, removeErr)
			}
		case info.IsDir():
			if removeErr := os.Remove(alias); removeErr != nil {
				return fmt.Errorf("replace existing run directory %q (it must be empty): %w", alias, removeErr)
			}
		default:
			return fmt.Errorf("alias path %q exists and is not a directory or symlink", alias)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect alias %q: %w", alias, err)
	}
	if err := os.Symlink(target, alias); err != nil {
		return fmt.Errorf("link %q to %q: %w", alias, target, err)
	}
	return nil
}

func (m *Manager) validateTrustAnchorForLaunch() error {
	if m == nil || m.cfg == nil || strings.TrimSpace(m.cfg.MicroVMPublicKeyPath) == "" {
		return nil
	}
	m.mu.Lock()
	unchanged, err := trustedMicroVMArtifactsUnchanged(m.trustedArtifacts)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	trustedArtifacts, err := verifyAndCaptureTrustedMicroVMArtifacts(m.cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.trustedArtifacts = trustedArtifacts
	m.mu.Unlock()
	return nil
}

func (m *Manager) prepareLaunchImage(dir string, image microVMImageSelection) (microVMImageSelection, error) {
	if m == nil || m.cfg == nil || strings.TrimSpace(m.cfg.MicroVMPublicKeyPath) == "" {
		sourceRootfs := m.microVMImageSourceRootfs(image)
		image.RootfsPath = filepath.Join(dir, rootfsName)
		if err := reflinkOnlyFile(image.RootfsPath, sourceRootfs, 0o600); err != nil {
			return microVMImageSelection{}, fmt.Errorf("prepare rootfs: %w", err)
		}
		return image, nil
	}
	if err := m.validateTrustAnchorForLaunch(); err != nil {
		return microVMImageSelection{}, fmt.Errorf("verify microVM trust anchor: %w", err)
	}
	return m.stageTrustedLaunchImage(dir, image)
}

func (m *Manager) microVMImageSourceRootfs(image microVMImageSelection) string {
	if strings.TrimSpace(image.RootfsPath) != "" {
		return image.RootfsPath
	}
	if m == nil || m.cfg == nil {
		return ""
	}
	return m.cfg.MicroVMRootfsPath
}

func (m *Manager) stageTrustedLaunchImage(dir string, image microVMImageSelection) (microVMImageSelection, error) {
	m.mu.Lock()
	artifacts := cloneTrustedMicroVMArtifacts(m.trustedArtifacts)
	m.mu.Unlock()
	if artifacts == nil {
		return microVMImageSelection{}, fmt.Errorf("trusted microVM artifact identities are not recorded")
	}
	staged, err := stageTrustedLaunchImageFiles(dir, image, artifacts)
	if err != nil {
		return microVMImageSelection{}, err
	}
	return staged, nil
}

func stageTrustedLaunchImageFiles(dir string, image microVMImageSelection, artifacts *trustedMicroVMArtifacts) (microVMImageSelection, error) {
	staged := image
	staged.KernelPath = filepath.Join(dir, kernelName)
	staged.RootfsPath = filepath.Join(dir, rootfsName)
	if strings.TrimSpace(image.SharedImagePath) != "" {
		staged.SharedImagePath = filepath.Join(dir, sharedImageName)
	}
	if err := copyFile(staged.KernelPath, image.KernelPath, 0o600); err != nil {
		return microVMImageSelection{}, fmt.Errorf("stage trusted kernel: %w", err)
	}
	if err := reflinkOnlyFile(staged.RootfsPath, image.RootfsPath, 0o600); err != nil {
		return microVMImageSelection{}, fmt.Errorf("prepare rootfs: %w", err)
	}
	if strings.TrimSpace(image.SharedImagePath) != "" {
		if err := copyFile(staged.SharedImagePath, image.SharedImagePath, 0o600); err != nil {
			return microVMImageSelection{}, fmt.Errorf("stage trusted shared image: %w", err)
		}
	}
	unchanged, err := trustedMicroVMArtifactsUnchanged(artifacts)
	if err != nil {
		return microVMImageSelection{}, err
	}
	if !unchanged {
		return microVMImageSelection{}, fmt.Errorf("trusted microVM artifacts changed while staging launch image")
	}
	return staged, nil
}

func verifyAndCaptureTrustedMicroVMArtifacts(cfg *config.Config) (*trustedMicroVMArtifacts, error) {
	if cfg == nil || strings.TrimSpace(cfg.MicroVMPublicKeyPath) == "" {
		return nil, nil
	}
	before, err := captureTrustedMicroVMArtifacts(cfg)
	if err != nil {
		return nil, fmt.Errorf("record microVM trust anchor identities: %w", err)
	}
	if err := cfg.ValidateMicroVMTrustAnchor(); err != nil {
		return nil, err
	}
	unchanged, err := trustedMicroVMArtifactsUnchanged(before)
	if err != nil {
		return nil, err
	}
	if !unchanged {
		return nil, fmt.Errorf("trusted microVM artifacts changed during verification")
	}
	return before, nil
}

// VerifyArtifactHealth proves that the artifact set verified during manager
// startup has not been replaced or modified in place.
func (m *Manager) VerifyArtifactHealth() error {
	if m == nil || m.trustedArtifacts == nil {
		return fmt.Errorf("trusted microVM artifacts are not configured")
	}
	unchanged, err := trustedMicroVMArtifactsUnchanged(m.trustedArtifacts)
	if err != nil {
		return err
	}
	if !unchanged {
		return fmt.Errorf("trusted microVM artifacts changed after startup verification")
	}
	return nil
}

func captureTrustedMicroVMArtifacts(cfg *config.Config) (*trustedMicroVMArtifacts, error) {
	if cfg == nil || strings.TrimSpace(cfg.MicroVMPublicKeyPath) == "" {
		return nil, nil
	}
	artifactDir := filepath.Dir(cfg.MicroVMKernelPath)
	paths := []trustedMicroVMArtifactFile{
		{label: "public key", path: cfg.MicroVMPublicKeyPath},
		{label: "kernel", path: cfg.MicroVMKernelPath},
		{label: "rootfs", path: cfg.MicroVMRootfsPath},
		{label: "shared image", path: cfg.MicroVMSharedImagePath},
		{label: "kernel provenance", path: filepath.Join(artifactDir, "kernel-provenance.json")},
		{label: "rootfs source manifest", path: filepath.Join(artifactDir, "rootfs-source-manifest.json")},
		{label: "manifest", path: filepath.Join(artifactDir, "manifest.json")},
		{label: "manifest signature", path: filepath.Join(artifactDir, "manifest.sig")},
		{label: "checksums", path: filepath.Join(artifactDir, "SHA256SUMS")},
	}
	for i := range paths {
		identity, err := trustedMicroVMArtifactIdentityFor(paths[i].path)
		if err != nil {
			return nil, fmt.Errorf("stat trusted microVM %s %q: %w", paths[i].label, paths[i].path, err)
		}
		paths[i].identity = identity
	}
	return &trustedMicroVMArtifacts{files: paths}, nil
}

func cloneTrustedMicroVMArtifacts(artifacts *trustedMicroVMArtifacts) *trustedMicroVMArtifacts {
	if artifacts == nil {
		return nil
	}
	files := make([]trustedMicroVMArtifactFile, len(artifacts.files))
	copy(files, artifacts.files)
	return &trustedMicroVMArtifacts{files: files}
}

func trustedMicroVMArtifactsUnchanged(artifacts *trustedMicroVMArtifacts) (bool, error) {
	if artifacts == nil {
		return false, nil
	}
	for _, file := range artifacts.files {
		identity, err := trustedMicroVMArtifactIdentityFor(file.path)
		if err != nil {
			return false, fmt.Errorf("stat trusted microVM %s %q: %w", file.label, file.path, err)
		}
		if file.identity != identity {
			return false, nil
		}
	}
	return true, nil
}

func trustedMicroVMArtifactIdentityFor(path string) (trustedMicroVMArtifactIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return trustedMicroVMArtifactIdentity{}, err
	}
	dev, ino, ctimeUnixNano, ok := fileStatIdentity(info)
	if !ok {
		return trustedMicroVMArtifactIdentity{}, fmt.Errorf("unsupported stat metadata")
	}
	return trustedMicroVMArtifactIdentity{
		dev:             dev,
		ino:             ino,
		size:            info.Size(),
		modTimeUnixNano: info.ModTime().UnixNano(),
		ctimeUnixNano:   ctimeUnixNano,
	}, nil
}

func fileStatIdentity(info os.FileInfo) (dev uint64, ino uint64, ctimeUnixNano int64, ok bool) {
	if info == nil || info.Sys() == nil {
		return 0, 0, 0, false
	}
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() {
		return 0, 0, 0, false
	}
	devField, inoField := stat.FieldByName("Dev"), stat.FieldByName("Ino")
	dev, ok = uint64FromStatField(devField)
	if !ok {
		return 0, 0, 0, false
	}
	ino, ok = uint64FromStatField(inoField)
	if !ok {
		return 0, 0, 0, false
	}
	for _, fieldName := range []string{"Ctim", "Ctimespec"} {
		field := stat.FieldByName(fieldName)
		if !field.IsValid() {
			continue
		}
		sec, secOK := int64FromStatField(field.FieldByName("Sec"))
		nsec, nsecOK := int64FromStatField(field.FieldByName("Nsec"))
		if secOK && nsecOK {
			return dev, ino, sec*int64(time.Second) + nsec, true
		}
	}
	return 0, 0, 0, false
}

func uint64FromStatField(field reflect.Value) (uint64, bool) {
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		if value < 0 {
			return 0, false
		}
		return uint64(value), true
	default:
		return 0, false
	}
}

func int64FromStatField(field reflect.Value) (int64, bool) {
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value := field.Uint()
		if value > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

func (m *Manager) microVMImageForStart(opts runtimemanager.StartOpts) (microVMImageSelection, error) {
	runtimeClass := opts.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	switch runtimeClass {
	case runtimemanager.RuntimeClassToolExecutor:
		rootfsPath := firstNonEmpty(m.cfg.MicroVMToolRootfsPath, m.cfg.MicroVMRootfsPath)
		sharedImagePath := firstNonEmpty(m.cfg.MicroVMToolSharedImagePath, m.cfg.MicroVMSharedImagePath)
		return microVMImageSelection{
			RuntimeClass:    runtimeClass,
			KernelPath:      m.cfg.MicroVMKernelPath,
			RootfsPath:      rootfsPath,
			SharedImagePath: sharedImagePath,
		}, nil
	default:
		return microVMImageSelection{}, fmt.Errorf("unsupported microVM runtime class %q", runtimeClass)
	}
}

// checkUnixSocketPath fails fast with an actionable message when a socket path would
// exceed the kernel limit, instead of letting Firecracker exit with a cryptic
// "path must be shorter than SUN_LEN".
func checkUnixSocketPath(label, path string) error {
	if len(path) >= maxUnixSocketPathLen {
		return fmt.Errorf("%s socket path %q is %d bytes, exceeding the unix socket limit of %d; set AG_MICROVM_RUN_DIR to a shorter path", label, path, len(path), maxUnixSocketPathLen)
	}
	return nil
}

func (m *Manager) prepareLaunch(ctx context.Context, instanceID, dir, kernelPath, rootfsPath, workspacePath, sharedImagePath, tapName, guestIP string) (firecrackerLaunch, error) {
	return m.prepareLaunchWithPolicy(ctx, instanceID, dir, kernelPath, rootfsPath, workspacePath, sharedImagePath, tapName, guestIP, nil)
}

func (m *Manager) prepareLaunchWithPolicy(ctx context.Context, instanceID, dir, kernelPath, rootfsPath, workspacePath, sharedImagePath, tapName, guestIP string, policy *runtimemanager.SandboxRuntimePolicy) (firecrackerLaunch, error) {
	if m.cfg.MicroVMAllowUnjailed {
		socket := filepath.Join(dir, firecrackerSockName)
		vsockUDS := filepath.Join(dir, vsockUDSName)
		if err := checkUnixSocketPath("firecracker api", socket); err != nil {
			return firecrackerLaunch{}, err
		}
		if err := checkUnixSocketPath("vsock", vsockUDS); err != nil {
			return firecrackerLaunch{}, err
		}
		configPath := filepath.Join(dir, configName)
		return firecrackerLaunch{
			executable: m.cfg.FirecrackerPath,
			args:       []string{"--id", instanceID, "--api-sock", socket, "--config-file", configPath},
			config:     buildFirecrackerConfigWithPolicy(m.cfg, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP, policy),
			configPath: configPath,
			socketPath: socket,
			vsockUDS:   vsockUDS,
		}, nil
	}

	jailRoot := m.jailerRoot(instanceID)
	if err := os.MkdirAll(jailRoot, 0o700); err != nil {
		return firecrackerLaunch{}, fmt.Errorf("create jail root: %w", err)
	}
	stagedRootfs := filepath.Join(jailRoot, rootfsName)
	stagedWorkspace := filepath.Join(jailRoot, workspaceName)
	if err := stageLinkedJailFile(stagedRootfs, rootfsPath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage rootfs in jail: %w", err)
	}
	if err := stageWorkspaceJailFile(stagedWorkspace, workspacePath, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage workspace in jail: %w", err)
	}
	if err := stageCopiedJailFile(filepath.Join(jailRoot, kernelName), kernelPath, 0o600, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
		_ = os.RemoveAll(jailRoot)
		return firecrackerLaunch{}, fmt.Errorf("stage kernel in jail: %w", err)
	}
	drivesSharedPath := ""
	if strings.TrimSpace(sharedImagePath) != "" {
		drivesSharedPath = sharedImageName
		if err := stageCopiedJailFile(filepath.Join(jailRoot, sharedImageName), sharedImagePath, 0o600, m.cfg.MicroVMJailerUID, m.cfg.MicroVMJailerGID); err != nil {
			_ = os.RemoveAll(jailRoot)
			return firecrackerLaunch{}, fmt.Errorf("stage shared image in jail: %w", err)
		}
	}

	socket := filepath.Join(jailRoot, firecrackerSockName)
	vsockUDS := filepath.Join(jailRoot, vsockUDSName)
	configPath := filepath.Join(jailRoot, configName)
	fcConfig := buildFirecrackerConfigWithPolicy(m.cfg, kernelName, rootfsName, workspaceName, drivesSharedPath, vsockUDSName, tapName, guestIP, policy)
	fcConfig.BootSource.KernelImagePath = kernelName

	memoryMiB := m.cfg.MicroVMMemoryMiB
	if policy != nil {
		memoryMiB = policy.MemoryMiB
	}
	args := m.jailerArgsWithMemory(instanceID, memoryMiB)
	args = append(args, "--", "--api-sock", firecrackerSockName, "--config-file", configName)
	return firecrackerLaunch{
		executable: m.cfg.JailerPath,
		args:       args,
		config:     fcConfig,
		configPath: configPath,
		socketPath: socket,
		vsockUDS:   vsockUDS,
		jailRoot:   jailRoot,
	}, nil
}

func (m *Manager) jailerArgs(instanceID string) []string {
	return m.jailerArgsWithMemory(instanceID, m.cfg.MicroVMMemoryMiB)
}

func (m *Manager) jailerArgsWithMemory(instanceID string, memoryMiB int) []string {
	args := []string{
		"--id", instanceID,
		"--exec-file", m.cfg.FirecrackerPath,
		"--uid", strconv.Itoa(m.cfg.MicroVMJailerUID),
		"--gid", strconv.Itoa(m.cfg.MicroVMJailerGID),
		"--chroot-base-dir", m.cfg.MicroVMJailerChrootBaseDir,
		"--new-pid-ns",
		"--resource-limit", "no-file=4096",
	}
	if m.cfg.MicroVMJailerCgroupVersion > 0 {
		args = append(args, "--cgroup-version", strconv.Itoa(m.cfg.MicroVMJailerCgroupVersion))
		if parent := strings.TrimSpace(m.cfg.MicroVMJailerParentCgroup); parent != "" {
			args = append(args, "--parent-cgroup", parent)
		}
		args = append(args, "--cgroup", jailerMemoryCgroup(m.cfg.MicroVMJailerCgroupVersion, memoryMiB))
	}
	return args
}

func (m *Manager) jailerRoot(instanceID string) string {
	execName := filepath.Base(m.cfg.FirecrackerPath)
	if execName == "." || execName == string(filepath.Separator) || execName == "" {
		execName = "firecracker"
	}
	return filepath.Join(m.cfg.MicroVMJailerChrootBaseDir, execName, instanceID, "root")
}

func (m *Manager) cleanupLaunch(launch firecrackerLaunch) {
	if launch.jailRoot != "" {
		_ = os.RemoveAll(launch.jailRoot)
	}
}

func jailerMemoryCgroup(version, memMiB int) string {
	overheadMiB := memMiB / 10
	if overheadMiB < 256 {
		overheadMiB = 256
	}
	bytes := int64(memMiB+overheadMiB) * 1024 * 1024
	if version == 1 {
		return fmt.Sprintf("memory.limit_in_bytes=%d", bytes)
	}
	return fmt.Sprintf("memory.max=%d", bytes)
}

func newInstanceID(agentID, compartmentID string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	return instancePrefix + "-" + instanceAgentIDSegment(agentID) + "-" + compartmentIDSegment(compartmentID) + "-" + hex.EncodeToString(b[:]), nil
}

func instanceAgentIDSegment(agentID string) string {
	agentID = strings.TrimSpace(agentID)
	const maxBytes = 35
	if len(agentID) <= maxBytes {
		return agentID
	}
	digest := sha256.Sum256([]byte(agentID))
	return agentID[:22] + "-" + hex.EncodeToString(digest[:6])
}

func compartmentIDSegment(compartmentID string) string {
	compartmentID = strings.TrimSpace(compartmentID)
	var b strings.Builder
	for _, r := range compartmentID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
		if b.Len() >= 16 {
			break
		}
	}
	segment := strings.Trim(b.String(), "-")
	if segment == "" {
		segment = "compartment"
	}
	if len(segment) <= 16 {
		return segment
	}
	return segment[:16]
}

func copyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := reflinkFile(out, in); err == nil {
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chmod(dst, mode)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// reflinkOnlyFile clones src into dst as a copy-on-write reflink and never falls
// back to a full byte copy. The tool rootfs image is multiple gigabytes, so a
// silent io.Copy per ephemeral VM adds tens of seconds of latency to every
// workspace tool call. When the clone cannot be made — dst on a different
// filesystem than src, or a filesystem without reflink support — it returns an
// actionable error instead of copying, so the misconfiguration surfaces loudly
// rather than as a mysterious stall.
func reflinkOnlyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := reflinkFile(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		hint := "the run dir must be on the same copy-on-write filesystem (btrfs/xfs) as the rootfs image"
		if errors.Is(err, syscall.EXDEV) {
			hint = "dst and src are on different filesystems; set AG_MICROVM_RUN_DIR to a path on the same filesystem as AG_MICROVM_ROOTFS_PATH"
		}
		return fmt.Errorf("reflink rootfs %s -> %s: %w (%s)", src, dst, err, hint)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func reflinkFile(dst, src *os.File) error {
	const ficlone = 0x40049409
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}

func stageCopiedJailFile(dst, src string, mode os.FileMode, uid, gid int) error {
	if err := copyFile(dst, src, mode); err != nil {
		return err
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		return err
	}
	return nil
}

func stageLinkedJailFile(dst, src string, uid, gid int) error {
	_ = os.Remove(dst)
	if err := hardLinkFile(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(dst, src, 0o600); err != nil {
			return err
		}
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func stageWorkspaceJailFile(dst, src string, uid, gid int) error {
	_ = os.Remove(dst)
	if err := hardLinkFile(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("link workspace image into jail: %w (jailer chroot base dir must be on the same filesystem as AG_MICROVM_WORKSPACE_DIR)", err)
		}
		return err
	}
	if err := chownIfDifferent(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func chownIfDifferent(path string, uid, gid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}

func ensureWorkspaceImage(ctx context.Context, path string, sizeMiB int, seedDir string) error {
	if info, err := os.Stat(path); err == nil {
		want := int64(sizeMiB) * 1024 * 1024
		if info.Size() != want {
			return fmt.Errorf("existing workspace image is %d bytes, requested policy requires %d bytes", info.Size(), want)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return createWorkspaceImage(ctx, path, sizeMiB, seedDir)
}

func createWorkspaceImage(ctx context.Context, path string, sizeMiB int, seedDir string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sizeBytes := int64(sizeMiB) * 1024 * 1024
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	args := []string{
		"-F",
		"-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard",
	}
	if seedDir != "" {
		if info, err := os.Stat(seedDir); err == nil && info.IsDir() {
			args = append(args, "-d", seedDir)
		} else if err != nil && !os.IsNotExist(err) {
			_ = os.Remove(path)
			return fmt.Errorf("stat workspace seed dir: %w", err)
		}
	}
	args = append(args, path)
	cmd := exec.CommandContext(ctx, "mkfs.ext4", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func debugfsReadFile(ctx context.Context, imagePath, guestPath string) ([]byte, bool, error) {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, imagePath).CombinedOutput()
	text := string(out)
	if strings.Contains(text, "File not found") || strings.Contains(text, "File not found by ext2_lookup") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("debugfs cat %s: %w: %s", guestPath, err, strings.TrimSpace(text))
	}
	lines := strings.SplitN(text, "\n", 2)
	if len(lines) == 2 && strings.HasPrefix(lines[0], "debugfs ") {
		text = lines[1]
	}
	return []byte(text), true, nil
}

func (m *Manager) prepareWorkspace(ctx context.Context, agentID, compartmentID string) (string, error) {
	return m.prepareWorkspaceSized(ctx, agentID, compartmentID, m.cfg.MicroVMWorkspaceSizeMiB)
}

func (m *Manager) prepareWorkspaceSized(ctx context.Context, agentID, compartmentID string, sizeMiB int) (string, error) {
	if strings.EqualFold(m.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		if sizeMiB != m.cfg.MicroVMWorkspaceSizeMiB {
			return "", fmt.Errorf("dm-thin workspace size %d MiB cannot enforce requested %d MiB", m.cfg.MicroVMWorkspaceSizeMiB, sizeMiB)
		}
		return m.ensureThinWorkspace(ctx, agentID, compartmentID)
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	workspacePath := filepath.Join(m.cfg.MicroVMWorkspaceDir, agentID, compartmentID+"."+workspaceName)
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0o700); err != nil {
		return "", fmt.Errorf("create compartment workspace dir: %w", err)
	}
	seedDir := m.workspaceSeedDir(agentID, compartmentID)
	if err := ensureWorkspaceImage(ctx, workspacePath, sizeMiB, seedDir); err != nil {
		return "", err
	}
	return workspacePath, nil
}

type firecrackerConfig struct {
	BootSource    bootSource     `json:"boot-source"`
	Drives        []drive        `json:"drives"`
	Machine       machineConfig  `json:"machine-config"`
	Vsock         vsockConfig    `json:"vsock"`
	Logger        loggerConfig   `json:"logger,omitempty"`
	NetworkIfaces []networkIface `json:"network-interfaces,omitempty"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machineConfig struct {
	VCPUCount   int    `json:"vcpu_count"`
	MemSizeMiB  int    `json:"mem_size_mib"`
	SMT         bool   `json:"smt"`
	CPUTemplate string `json:"cpu_template,omitempty"`
}

type vsockConfig struct {
	VsockID  string `json:"vsock_id"`
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type loggerConfig struct {
	LogPath string `json:"log_path,omitempty"`
	Level   string `json:"level,omitempty"`
}

type networkIface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name,omitempty"`
}

func buildFirecrackerConfig(cfg *config.Config, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP string) firecrackerConfig {
	return buildFirecrackerConfigWithPolicy(cfg, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP, nil)
}

func buildFirecrackerConfigWithPolicy(cfg *config.Config, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP string, policy *runtimemanager.SandboxRuntimePolicy) firecrackerConfig {
	workspaceWritable := true
	vcpus := cfg.MicroVMVCPUs
	memoryMiB := cfg.MicroVMMemoryMiB
	processLimit := 0
	if policy != nil {
		workspaceWritable = policy.WorkspaceWritable
		vcpus = policy.VCPUs
		memoryMiB = policy.MemoryMiB
		processLimit = policy.ProcessLimit
	}
	drives := []drive{
		{DriveID: "rootfs", PathOnHost: rootfsPath, IsRootDevice: true, IsReadOnly: false},
		{DriveID: "workspace", PathOnHost: workspacePath, IsRootDevice: false, IsReadOnly: !workspaceWritable},
	}
	if strings.TrimSpace(sharedImagePath) != "" {
		drives = append(drives, drive{DriveID: "shared", PathOnHost: sharedImagePath, IsRootDevice: false, IsReadOnly: true})
	}
	fc := firecrackerConfig{
		BootSource: bootSource{KernelImagePath: kernelPath, BootArgs: effectiveKernelArgsWithProcessLimit(cfg, guestIP, processLimit)},
		Drives:     drives,
		Machine:    machineConfig{VCPUCount: vcpus, MemSizeMiB: memoryMiB, SMT: false, CPUTemplate: cfg.MicroVMCPUTemplate},
		Vsock:      vsockConfig{VsockID: "agentcy-vsock", GuestCID: 3, UDSPath: vsockUDS},
	}
	if strings.TrimSpace(tapName) != "" {
		fc.NetworkIfaces = []networkIface{{
			IfaceID:     "eth0",
			GuestMAC:    guestMACForInstance(tapName),
			HostDevName: strings.TrimSpace(tapName),
		}}
	}
	return fc
}

func effectiveKernelArgsWithProcessLimit(cfg *config.Config, guestIP string, processLimit int) string {
	args := effectiveKernelArgs(cfg, guestIP)
	if processLimit > 0 {
		args += " agentcy.process_limit=" + strconv.Itoa(processLimit)
	}
	return args
}

func effectiveKernelArgs(cfg *config.Config, guestIP string) string {
	args := strings.TrimSpace(cfg.MicroVMKernelArgs)
	if args == "" {
		args = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"
	}
	// Firecracker snapshot restore on this kernel/CPU combination faults in the
	// guest FPU restore path when XSAVE state is enabled. Disable guest XSAVE
	// until the image/kernel snapshot path can safely support it.
	if !hasKernelArg(args, "noxsave") {
		args += " noxsave"
	}
	if ipArg := guestIPBootArg(cfg, guestIP); ipArg != "" && !strings.Contains(args, "ip=") {
		args += " " + ipArg
	}
	return strings.TrimSpace(args)
}

func hasKernelArg(args, name string) bool {
	for _, field := range strings.Fields(args) {
		if field == name || strings.HasPrefix(field, name+"=") {
			return true
		}
	}
	return false
}

type identityFile struct {
	InstanceID    string `json:"instanceId"`
	AgentID       string `json:"agentId"`
	CompartmentID string `json:"compartmentId"`
	Timezone      string `json:"timezone"`
	PlatformURL   string `json:"platformApiUrl"`
	FlueStoreURL  string `json:"flueStoreUrl"`
	CreatedAt     string `json:"createdAt"`
}

func (m *Manager) flueStoreURL(agentID string) string {
	return strings.TrimRight(m.cfg.PlatformAPIURL, "/") + "/api/agents/" + agentID + "/flue-store"
}

func (m *Manager) writeIdentityFile(dir, instanceID, agentID string, opts runtimemanager.StartOpts) error {
	identity := identityFile{
		InstanceID:    instanceID,
		AgentID:       agentID,
		CompartmentID: normalizeRuntimeCompartmentID(opts.CompartmentID),
		Timezone:      strings.TrimSpace(opts.Timezone),
		PlatformURL:   m.cfg.PlatformAPIURL,
		FlueStoreURL:  m.flueStoreURL(agentID),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), data, 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}

// controlPlaneReadyTimeout bounds how long CreateAndStart waits for the in-guest
// agent to come up so startup secrets can be delivered before the runtime starts.
const controlPlaneReadyTimeout = 60 * time.Second

// buildStartupSecretBundle assembles the static-tier tool environment (and any
// proxy CA material) delivered to the guest over vsock after boot. The guest
// needs its scoped identity and proxy settings, but it must not receive
// control-plane runtime or Flue credentials; durable agent execution runs in host
// harness cells with a separately scoped AGENTCY_HARNESS_TOKEN.
func (m *Manager) buildStartupSecretBundle(agentID, instanceID string, opts runtimemanager.StartOpts) (SecretBundle, error) {
	env := map[string]string{
		"PLATFORM_API_URL": m.cfg.PlatformAPIURL,
		// sudo is a preserved capability and safe under the microVM
		// hypervisor boundary.
		"AGENT_ENABLE_SUDO":      "true",
		"AGENT_ID":               agentID,
		"AGENT_HOST_GID":         strconv.Itoa(os.Getgid()),
		"AGENTCY_COMPARTMENT_ID": normalizeRuntimeCompartmentID(opts.CompartmentID),
		"TZ":                     registry.NormalizeTimezone(opts.Timezone),
	}
	if strings.TrimSpace(instanceID) != "" {
		env["AGENTCY_RUNTIME_CREDENTIAL_ID"] = agentID + ":" + normalizeRuntimeCompartmentID(opts.CompartmentID) + ":" + strings.TrimSpace(instanceID)
	}
	files := map[string]string{}
	if opts.ProxyEgress != nil && opts.ProxyEgress.Enabled {
		proxyURL := m.proxyURLForGuest(opts.ProxyEgress)
		const gitHubAskpassPath = "/runtime-private/github-askpass"
		const gitConfigPath = "/runtime-private/gitconfig"
		env["AGENTCY_PROXY_EGRESS_ENABLED"] = "true"
		env["GH_TOKEN"] = "agentcy-proxy:github"
		env["GITHUB_TOKEN"] = "agentcy-proxy:github"
		env["GIT_ASKPASS"] = gitHubAskpassPath
		env["GIT_CONFIG_GLOBAL"] = gitConfigPath
		env["GIT_TERMINAL_PROMPT"] = "0"
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["NO_PROXY"] = opts.ProxyEgress.NoProxy
		env["no_proxy"] = opts.ProxyEgress.NoProxy
		env["npm_config_proxy"] = proxyURL
		env["npm_config_https_proxy"] = proxyURL
		if strings.TrimSpace(opts.ProxyEgress.NoProxy) != "" {
			env["npm_config_noproxy"] = opts.ProxyEgress.NoProxy
		}
		files["github-askpass"] = gitHubAskpassScript()
		files["gitconfig"] = gitHubProxyGitConfig()
		if token := strings.TrimSpace(opts.ProxyEgress.ContextToken); token != "" {
			env["AGENTCY_EGRESS_CONTEXT_TOKEN"] = token
		}
		if strings.TrimSpace(opts.ProxyEgress.CACertPath) != "" {
			data, err := os.ReadFile(opts.ProxyEgress.CACertPath)
			if err != nil {
				return SecretBundle{}, fmt.Errorf("read egress proxy CA cert: %w", err)
			}
			const guestCAPath = "/runtime-private/proxy-ca.crt"
			files["proxy-ca.crt"] = string(data)
			env["NODE_EXTRA_CA_CERTS"] = guestCAPath
			env["REQUESTS_CA_BUNDLE"] = guestCAPath
			env["SSL_CERT_FILE"] = guestCAPath
			env["GIT_SSL_CAINFO"] = guestCAPath
			env["CURL_CA_BUNDLE"] = guestCAPath
		}
	}
	bundle := SecretBundle{Env: env}
	if len(files) > 0 {
		bundle.Files = files
	}
	if !opts.RuntimeContextProjection.IsZero() {
		projection, err := runtimecontext.Canonicalize(opts.RuntimeContextProjection)
		if err != nil {
			return SecretBundle{}, fmt.Errorf("canonicalize runtime context projection: %w", err)
		}
		bundle.RuntimeContextProjection = projection
	}
	return bundle, nil
}

func gitHubAskpassScript() string {
	return `#!/bin/sh
case "$1" in
  *Username*|*username*)
    printf '%s\n' 'x-access-token'
    ;;
  *Password*|*password*)
    printf '%s\n' "${GITHUB_TOKEN:-${GH_TOKEN:-}}"
    ;;
  *)
    printf '\n'
    ;;
esac
`
}

func gitHubProxyGitConfig() string {
	return `[credential "https://github.com"]
	username = x-access-token
	helper =
	useHttpPath = true
[url "https://github.com/"]
	insteadOf = git@github.com:
	insteadOf = ssh://git@github.com/
`
}

func (m *Manager) proxyURLForGuest(proxy *runtimemanager.ProxyEgressConfig) string {
	if proxy == nil {
		return ""
	}
	rawURL := strings.TrimSpace(proxy.ProxyURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return rawURL
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	if host == "agentcy-egress-proxy" {
		gateway, _, err := net.ParseCIDR(strings.TrimSpace(m.cfg.MicroVMBridgeCIDR))
		if err == nil && gateway != nil {
			port := parsed.Port()
			if port != "" {
				parsed.Host = net.JoinHostPort(gateway.String(), port)
			} else {
				parsed.Host = gateway.String()
			}
		}
	}
	if token := strings.TrimSpace(proxy.ContextToken); token != "" {
		parsed.User = url.UserPassword("AgentcyContext", token)
	}
	return parsed.String()
}

func (m *Manager) startupFingerprint(agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
	return m.startupFingerprintWithEffectiveProfileHash(agentID, compartmentID, opts, strings.TrimSpace(opts.ShapeFingerprint))
}

func (m *Manager) startupFingerprintWithEffectiveProfileHash(agentID, compartmentID string, opts runtimemanager.StartOpts, effectiveProfileHash string) (string, error) {
	bundle, err := m.buildStartupSecretBundle(agentID, "", opts)
	if err != nil {
		return "", err
	}
	image, err := m.microVMImageForStart(opts)
	if err != nil {
		return "", err
	}
	rootfsIdentity, err := fileArtifactIdentity(image.RootfsPath)
	if err != nil {
		return "", fmt.Errorf("stat startup rootfs image: %w", err)
	}
	sharedIdentity, err := fileArtifactIdentity(image.SharedImagePath)
	if err != nil {
		return "", fmt.Errorf("stat startup shared image: %w", err)
	}
	fingerprintInput := struct {
		SecretBundle            SecretBundle                        `json:"secretBundle"`
		RuntimeClass            runtimemanager.RuntimeClass         `json:"runtimeClass"`
		RootfsPath              string                              `json:"rootfsPath"`
		RootfsIdentity          *ArtifactIdentity                   `json:"rootfsIdentity,omitempty"`
		SharedPath              string                              `json:"sharedPath,omitempty"`
		SharedIdentity          *ArtifactIdentity                   `json:"sharedIdentity,omitempty"`
		EffectiveProfileHash    string                              `json:"effectiveProfileHash,omitempty"`
		ActorPrincipal          string                              `json:"actorPrincipal,omitempty"`
		RuntimeActorContext     runtimecontext.VerifiedActorContext `json:"runtimeActorContext,omitempty"`
		ExecutorContractVersion int                                 `json:"executorContractVersion,omitempty"`
		ExecutorCapabilities    []string                            `json:"executorCapabilities,omitempty"`
	}{
		SecretBundle:            bundle,
		RuntimeClass:            image.RuntimeClass,
		RootfsPath:              image.RootfsPath,
		RootfsIdentity:          rootfsIdentity,
		SharedPath:              image.SharedImagePath,
		SharedIdentity:          sharedIdentity,
		EffectiveProfileHash:    effectiveProfileHash,
		ActorPrincipal:          strings.TrimSpace(opts.ActorPrincipal),
		RuntimeActorContext:     canonicalRuntimeActorContext(opts.RuntimeActorContext),
		ExecutorContractVersion: executorContractVersionForFingerprint(image.RuntimeClass),
		ExecutorCapabilities:    executorCapabilitiesForFingerprint(image.RuntimeClass),
	}
	data, err := json.Marshal(fingerprintInput)
	if err != nil {
		return "", fmt.Errorf("marshal startup fingerprint bundle: %w", err)
	}
	sum := sha256.New()
	sum.Write(data)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func canonicalRuntimeActorContext(actor runtimecontext.VerifiedActorContext) runtimecontext.VerifiedActorContext {
	actor.Principal = strings.TrimSpace(actor.Principal)
	actor.PlatformUserID = strings.TrimSpace(actor.PlatformUserID)
	actor.SessionID = strings.TrimSpace(actor.SessionID)
	actor.Source = strings.TrimSpace(actor.Source)
	actor.TurnContextID = strings.TrimSpace(actor.TurnContextID)
	actor.RequestID = strings.TrimSpace(actor.RequestID)
	return actor
}

func executorContractVersionForFingerprint(runtimeClass runtimemanager.RuntimeClass) int {
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	if runtimeClass != runtimemanager.RuntimeClassToolExecutor {
		return 0
	}
	return toolExecutorFingerprintContractVersion
}

func executorCapabilitiesForFingerprint(runtimeClass runtimemanager.RuntimeClass) []string {
	if runtimeClass == "" {
		runtimeClass = runtimemanager.RuntimeClassToolExecutor
	}
	if runtimeClass != runtimemanager.RuntimeClassToolExecutor {
		return nil
	}
	caps := append([]string(nil), toolExecutorFingerprintCapabilities...)
	sort.Strings(caps)
	return caps
}

func (m *Manager) workspaceSeedDir(agentID, compartmentID string) string {
	defaultSeed := filepath.Join(m.cfg.AgentsDir(), agentID, "workspace")
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if compartmentID == "" {
		return defaultSeed
	}
	compartmentSeed := filepath.Join(m.cfg.AgentsDir(), agentID, "compartments", compartmentID, "workspace")
	if info, err := os.Stat(compartmentSeed); err == nil && info.IsDir() {
		return compartmentSeed
	}
	return defaultSeed
}

// waitForControlPlane blocks until the in-guest agent answers a heartbeat over
// vsock, the VM exits, or the timeout/ctx elapses.
func (m *Manager) waitForControlPlane(ctx context.Context, inst *instance, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := inst.controlClient(time.Second).Heartbeat(ctx); err == nil {
			return nil
		}
		if inst.launcherOnly {
			if time.Since(inst.startedAt) > 2*time.Second {
				running, err := m.firecrackerRunning(ctx, inst)
				if err == nil && !running {
					return fmt.Errorf("jailed firecracker process exited before its control plane became ready%s%s", inst.jailDiagnostics(), inst.logTailDiagnostics(120))
				}
			}
		} else {
			select {
			case <-inst.done:
				return fmt.Errorf("microVM exited before its control plane became ready")
			default:
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for microVM control plane after %s%s%s", timeout, inst.jailDiagnostics(), inst.logTailDiagnostics(80))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) firecrackerRunning(ctx context.Context, inst *instance) (bool, error) {
	if inst == nil {
		return false, nil
	}
	if inst.launcherOnly && m.launcher != nil {
		return m.launcher.Running(ctx, inst.id)
	}
	return firecrackerProcessRunning(inst.id)
}

// deliverStartupSecrets waits for the guest control plane, then applies the
// runtime environment bundle over vsock so the runtime can reach the platform
// API and the Flue durable store before its first turn.
func (m *Manager) deliverStartupSecrets(ctx context.Context, inst *instance, agentID string, opts runtimemanager.StartOpts, timer *coldStartStageTimer) error {
	bundle, err := m.buildStartupSecretBundle(agentID, inst.id, opts)
	if err != nil {
		return err
	}
	timer.mark("startup_secret_bundle_ready")
	if err := m.waitForControlPlane(ctx, inst, controlPlaneReadyTimeout); err != nil {
		return err
	}
	timer.mark("control_plane_ready")
	if err := inst.controlClient(15*time.Second).ApplySecrets(ctx, bundle); err != nil {
		return fmt.Errorf("apply runtime secrets: %w", err)
	}
	timer.mark("startup_secrets_applied")
	return nil
}

func (inst *instance) jailDiagnostics() string {
	if inst == nil || inst.jailRoot == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\njail diagnostics:")
	for _, rel := range []string{"dev", "dev/net", "dev/net/tun", "dev/kvm", firecrackerSockName, configName} {
		path := filepath.Join(inst.jailRoot, rel)
		info, err := os.Lstat(path)
		if err != nil {
			b.WriteString("\n  ")
			b.WriteString(rel)
			b.WriteString(": ")
			b.WriteString(err.Error())
			continue
		}
		b.WriteString("\n  ")
		b.WriteString(rel)
		b.WriteString(": mode=")
		b.WriteString(info.Mode().String())
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			b.WriteString(" uid=")
			b.WriteString(strconv.FormatUint(uint64(stat.Uid), 10))
			b.WriteString(" gid=")
			b.WriteString(strconv.FormatUint(uint64(stat.Gid), 10))
			if info.Mode()&os.ModeDevice != 0 {
				b.WriteString(" rdev=")
				b.WriteString(strconv.FormatUint(uint64(stat.Rdev), 10))
			}
		}
	}
	for _, path := range []string{filepath.Join(inst.jailRoot, "firecracker.pid"), filepath.Join(filepath.Dir(inst.jailRoot), "firecracker.pid")} {
		if pidData, err := os.ReadFile(path); err == nil {
			b.WriteString("\n  ")
			b.WriteString(path)
			b.WriteString("=")
			b.WriteString(strings.TrimSpace(string(pidData)))
		}
	}
	if inst.tapName != "" {
		for _, args := range [][]string{
			{"tuntap", "show", inst.tapName},
			{"-details", "link", "show", "dev", inst.tapName},
		} {
			out, err := exec.Command("ip", args...).CombinedOutput()
			b.WriteString("\n  ip ")
			b.WriteString(strings.Join(args, " "))
			b.WriteString(": ")
			if err != nil {
				b.WriteString(err.Error())
				if len(out) > 0 {
					b.WriteString(": ")
				}
			}
			b.WriteString(strings.TrimSpace(string(out)))
		}
	}
	return b.String()
}

func (inst *instance) logTailDiagnostics(lines int) string {
	if inst == nil || strings.TrimSpace(inst.logPath) == "" {
		return ""
	}
	data, err := os.ReadFile(inst.logPath)
	if err != nil {
		return "\nfirecracker log tail: " + err.Error()
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "\nfirecracker log tail: empty"
	}
	all := strings.Split(trimmed, "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return "\nfirecracker log tail:\n" + strings.Join(all, "\n")
}
