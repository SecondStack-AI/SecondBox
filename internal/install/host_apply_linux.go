//go:build linux

package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type SystemHostApplyExecutor struct{ CallerUID int }

func (executor SystemHostApplyExecutor) EffectiveUID() int { return os.Geteuid() }

func (executor SystemHostApplyExecutor) Revalidate(ctx context.Context, plan InstallPlan, receipt InstallReceipt) error {
	if executor.CallerUID < 0 || plan.HostFacts.InvokingUID != int64(executor.CallerUID) {
		return installerError("SUDO_UID differs from the accepted invoking user", nil)
	}
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil || "machine-id:"+strings.TrimSpace(string(machineID)) != plan.HostFacts.HostIdentity {
		return installerError("host identity changed after preflight", err)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return installerError("cgroup v2 is unavailable", err)
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(" "+string(controllers)+" ", " "+required+" ") {
			return installerError("required cgroup v2 controller "+required+" is unavailable", nil)
		}
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		filesystems, err := os.ReadFile("/proc/filesystems")
		if err != nil || !strings.Contains(string(filesystems), "btrfs") {
			return installerError("Btrfs kernel filesystem support is unavailable", err)
		}
	}
	for _, path := range []string{"/dev/kvm", "/dev/net/tun"} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			return installerError(path+" is not a device", err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return installerError(path+" is not accessible as root", err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	userProbe := systemUserProbe{filesystem: systemFilesystemProbe{}}
	assigned, err := userProbe.AssignedUIDs()
	if err != nil {
		return err
	}
	reserved, err := userProbe.ReservedIDRanges()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(reserved, func(candidate UIDRange) bool { return rangesOverlap(plan.Network.JailerUIDRange, candidate) }) {
		return installerError("accepted jailer UID/GID range now overlaps a subordinate ID allocation", nil)
	}
	for uid := plan.Network.JailerUIDRange.Start; uid < plan.Network.JailerUIDRange.Start+plan.Network.JailerUIDRange.Count; uid++ {
		if assigned[uid] {
			return installerError("accepted jailer UID range is now assigned", nil)
		}
	}
	caller, err := user.LookupId(strconv.Itoa(executor.CallerUID))
	if err != nil {
		return installerError("resolve SUDO_UID account", err)
	}
	for _, planned := range plan.Paths {
		if !planned.RequiresSudo || !planned.Create {
			continue
		}
		if planned.Path == caller.HomeDir || planned.Path == "/home" || planned.Path == "/root" {
			return installerError("privileged target is a home-directory root", nil)
		}
		if resource, recorded := receiptResource(receipt, planned.Name); recorded {
			if err := validateRecordedHostApplyPath(plan, planned, resource); err != nil {
				return err
			}
			continue
		}
		if slices.Contains(receipt.PendingResourceIDs, planned.Name) {
			if _, statErr := os.Lstat(planned.Path); statErr == nil {
				if err := validateRecordedHostApplyPath(plan, planned, resourceFromPath(planned, StageHostApply)); err != nil {
					return installerError("pending privileged target "+planned.Name, err)
				}
				continue
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return installerError("inspect pending privileged target "+planned.Name, statErr)
			}
		}
		if err := verifyCreateOnlyPath(planned.Path); err != nil {
			return installerError("privileged target "+planned.Name, err)
		}
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		if filepath.Base(plan.Storage.MountUnitPath) != systemdMountUnitName(plan.Storage.WorkspacePath) {
			return installerError("mount unit name does not encode the exact workspace mountpoint", nil)
		}
		var stat unix.Statfs_t
		if err := unix.Statfs(filepath.Dir(plan.Storage.FilesystemImagePath), &stat); err != nil {
			return installerError("inspect filesystem-image backing capacity", err)
		}
		available := int64(stat.Bavail) * int64(stat.Bsize)
		imageExists := false
		if _, err := os.Lstat(plan.Storage.FilesystemImagePath); err == nil {
			imageExists = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return installerError("inspect filesystem image before backing-capacity revalidation", err)
		}
		required := filesystemImageBackingRequired(plan, receipt, imageExists)
		if available < required {
			return installerError("filesystem-image backing capacity became stale", nil)
		}
	} else if err := verifyExistingWorkspaceMount(plan); err != nil {
		return err
	}
	deployment, found := plannedPathByName(plan.Paths, "deployment")
	if !found {
		return installerError("deployment directory is absent from accepted plan", nil)
	}
	var deploymentStat unix.Statfs_t
	if err := unix.Statfs(deployment.Path, &deploymentStat); err != nil {
		return installerError("inspect deployment filesystem capacity", err)
	}
	if int64(deploymentStat.Bavail)*int64(deploymentStat.Bsize) < MinimumDeploymentBytes {
		return installerError("deployment filesystem capacity became insufficient", nil)
	}
	return ctx.Err()
}

func filesystemImageBackingRequired(plan InstallPlan, receipt InstallReceipt, imageExists bool) int64 {
	required := MinimumBackingReserveBytes + plan.Release.ExpectedDownloadBytes + MinimumObjectStoreBytes
	_, recorded := receiptResource(receipt, "filesystem-image")
	if !imageExists || (!recorded && !slices.Contains(receipt.PendingResourceIDs, "filesystem-image")) {
		required += plan.Storage.ImageSizeBytes
	}
	return required
}

func validateRecordedHostApplyPath(plan InstallPlan, planned PlannedPath, resource CreatedResource) error {
	if resource.Path != planned.Path || resource.Kind != planned.Kind || resource.Class != planned.Class || resource.Mode != planned.Mode || resource.OwnerUID != planned.OwnerUID || resource.OwnerGID != planned.OwnerGID || resource.Stage != StageHostApply {
		return installerError("recorded host resource differs from accepted plan: "+planned.Name, nil)
	}
	if planned.Name == "workspace" && plan.Storage.Choice == StorageBtrfsImage {
		info, err := os.Lstat(planned.Path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return installerError("recorded workspace is missing or unsafe", err)
		}
	} else if err := ValidatePlannedPath(planned); err != nil {
		return err
	}
	if planned.Kind == ResourceFilesystemImage {
		info, err := os.Lstat(planned.Path)
		if err != nil || info.Size() != plan.Storage.ImageSizeBytes {
			return installerError("recorded filesystem image size changed", err)
		}
	}
	if planned.Kind == ResourceMountUnit {
		content, err := os.ReadFile(planned.Path)
		if err != nil || string(content) != MountUnit(plan.Storage.FilesystemImagePath, plan.Storage.WorkspacePath) {
			return installerError("recorded workspace mount unit changed", err)
		}
	}
	return nil
}

func verifyCreateOnlyPath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return errors.New("target already exists")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	ancestor := filepath.Dir(path)
	for {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("existing ancestor is not a real directory")
			}
			fd, err := openDirectoryNoSymlinks(ancestor)
			if err == nil {
				err = unix.Close(fd)
			}
			return err
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return errors.New("no existing safe ancestor")
		}
		ancestor = next
	}
}

func verifyExistingWorkspaceMount(plan InstallPlan) error {
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	return verifyExistingWorkspaceMountInfo(plan, content)
}

func verifyExistingWorkspaceMountInfo(plan InstallPlan, content []byte) error {
	parent := filepath.Dir(plan.Storage.WorkspacePath)
	rootDeviceIdentity := ""
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && decodeMount(fields[4]) == "/" {
			rootDeviceIdentity = fields[2]
			break
		}
	}
	if rootDeviceIdentity == "" {
		return installerError("root mount identity is unavailable", nil)
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		separator := slices.Index(fields, "-")
		if separator < 0 || separator+2 >= len(fields) || len(fields) < 6 || decodeMount(fields[4]) != parent {
			continue
		}
		if fields[2] != plan.Storage.ExistingDeviceIdentity || fields[2] == rootDeviceIdentity || (fields[separator+1] != "xfs" && fields[separator+1] != "btrfs") || parent == "/" {
			return installerError("existing workspace mount identity or filesystem changed", nil)
		}
		return nil
	}
	return installerError("accepted existing workspace mount is no longer mounted", nil)
}

func (SystemHostApplyExecutor) CreateDirectory(path PlannedPath) error {
	if path.Kind != ResourceDirectory {
		return installerError("directory action received a non-directory resource", nil)
	}
	if err := os.Mkdir(path.Path, fs.FileMode(path.Mode)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ValidatePlannedPath(path)
		}
		return err
	}
	if err := errors.Join(os.Chown(path.Path, int(path.OwnerUID), int(path.OwnerGID)), os.Chmod(path.Path, fs.FileMode(path.Mode))); err != nil {
		return errors.Join(err, os.Remove(path.Path))
	}
	return nil
}

func (SystemHostApplyExecutor) AllocateFilesystemImage(path PlannedPath, size int64) error {
	if path.Kind != ResourceFilesystemImage || size < MinimumFilesystemImageBytes {
		return installerError("filesystem-image allocation is invalid", nil)
	}
	file, err := os.OpenFile(path.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(path.Mode))
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			if validateErr := ValidatePlannedPath(path); validateErr != nil {
				return validateErr
			}
			info, statErr := os.Lstat(path.Path)
			if statErr != nil || info.Size() != size {
				return installerError("pending filesystem image size changed", statErr)
			}
			return nil
		}
		return err
	}
	fd := int(file.Fd())
	secureErr := errors.Join(file.Chown(int(path.OwnerUID), int(path.OwnerGID)), file.Chmod(fs.FileMode(path.Mode)))
	allocateErr := unix.Fallocate(fd, 0, 0, size)
	closeErr := file.Close()
	if err := errors.Join(secureErr, allocateErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(path.Path))
	}
	return nil
}

func (SystemHostApplyExecutor) FormatBtrfs(ctx context.Context, imagePath, installerTools string) error {
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "0:0", "--mount", "type=bind,source="+imagePath+",target=/workspace.img", installerTools, "--force", "/workspace.img")
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("SecondBox installer-tools container: %w", err)
	}
	return nil
}

func (SystemHostApplyExecutor) WriteMountUnit(path PlannedPath, content string) (resultErr error) {
	if path.Kind != ResourceMountUnit || content == "" {
		return installerError("mount-unit action is invalid", nil)
	}
	temporaryDirectory, err := os.MkdirTemp("/run", ".secondbox-mount-verify-*")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporaryDirectory)) }()
	temporaryPath := filepath.Join(temporaryDirectory, filepath.Base(path.Path))
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	verify := exec.Command("systemd-analyze", "verify", temporaryPath)
	verify.Stdout, verify.Stderr = os.Stderr, os.Stderr
	if err := verify.Run(); err != nil {
		return fmt.Errorf("systemd-analyze verify: %w", err)
	}
	if _, statErr := os.Lstat(path.Path); statErr == nil {
		if err := ValidatePlannedPath(path); err != nil {
			return err
		}
		existing, err := os.ReadFile(path.Path)
		if err != nil || string(existing) != content {
			return installerError("pending workspace mount unit changed", err)
		}
		return nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	unit, err := os.OpenFile(path.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(path.Mode))
	if err != nil {
		return err
	}
	_, writeErr := unit.WriteString(content)
	secureErr := errors.Join(unit.Chown(int(path.OwnerUID), int(path.OwnerGID)), unit.Chmod(fs.FileMode(path.Mode)))
	closeErr := errors.Join(unit.Sync(), unit.Close())
	if err := errors.Join(writeErr, secureErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(path.Path))
	}
	return nil
}

func (SystemHostApplyExecutor) EnableMountUnit(ctx context.Context, unitPath string) error {
	for _, arguments := range [][]string{{"daemon-reload"}, {"enable", "--now", filepath.Base(unitPath)}} {
		command := exec.CommandContext(ctx, "systemctl", arguments...)
		command.Stdout, command.Stderr = os.Stderr, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(arguments, " "), err)
		}
	}
	return nil
}

func (SystemHostApplyExecutor) SecureMountedWorkspace(path PlannedPath) error {
	info, err := os.Lstat(path.Path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return installerError("mounted Workspace is not a real directory", err)
	}
	return errors.Join(os.Chown(path.Path, int(path.OwnerUID), int(path.OwnerGID)), os.Chmod(path.Path, fs.FileMode(path.Mode)))
}

func (SystemHostApplyExecutor) ProveReflinkIsolation(workspace string) (identity string, resultErr error) {
	source, err := os.CreateTemp(workspace, ".secondbox-reflink-source-*")
	if err != nil {
		return "", err
	}
	sourcePath := source.Name()
	defer func() { resultErr = errors.Join(resultErr, os.Remove(sourcePath)) }()
	destination, err := os.CreateTemp(workspace, ".secondbox-reflink-destination-*")
	if err != nil {
		_ = source.Close()
		return "", err
	}
	destinationPath := destination.Name()
	defer func() { resultErr = errors.Join(resultErr, os.Remove(destinationPath)) }()
	if _, err := source.WriteString("source-proof"); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if err := source.Sync(); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if err := unix.IoctlFileClone(int(destination.Fd()), int(source.Fd())); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if _, err := destination.WriteAt([]byte("mutated"), 0); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if err := destination.Sync(); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	content := make([]byte, len("source-proof"))
	if _, err := source.ReadAt(content, 0); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if string(content) != "source-proof" {
		return "", errors.Join(errors.New("reflink mutation changed the source"), source.Close(), destination.Close())
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &stat); err != nil {
		return "", errors.Join(err, source.Close(), destination.Close())
	}
	if err := errors.Join(source.Close(), destination.Close()); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev))), nil
}

func (SystemHostApplyExecutor) RemoveEmpty(resource CreatedResource) (bool, error) {
	switch resource.Kind {
	case ResourceDirectory:
		if err := os.Remove(resource.Path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return true, nil
			}
			if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EBUSY) {
				return false, nil // A non-empty or mounted directory is intentionally retained.
			}
			return false, err
		}
		return true, nil
	case ResourceFile, ResourceFilesystemImage, ResourceMountUnit:
		info, err := os.Lstat(resource.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode().IsRegular() && info.Size() == 0 {
			return true, os.Remove(resource.Path)
		}
	}
	return false, nil
}
