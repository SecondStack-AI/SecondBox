//go:build linux

package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"golang.org/x/sys/unix"
)

// PurgeAcceptedHost removes only receipt-backed privileged resources. Missing
// targets are accepted as the postcondition of an interrupted purge, while a
// replacement, symlinked component, mount crossing, or changed regular file is
// refused.
func PurgeAcceptedHost(ctx context.Context, directory, expectedDigest string, ownerUID int, now func() time.Time) (result InstallReceipt, resultErr error) {
	if os.Geteuid() != 0 || ownerUID < 0 || now == nil {
		return InstallReceipt{}, installerError("private host purge requires root, SUDO_UID, and a clock", nil)
	}
	lock, err := AcquireLock(directory)
	if err != nil {
		return InstallReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err := RecoverOperation(directory, ownerUID, lock)
	if err != nil {
		return InstallReceipt{}, err
	}
	if err := validateAcceptedHostPurge(plan, receipt, expectedDigest); err != nil {
		return receipt, err
	}
	persist := func() error { return SaveReceipt(directory, plan, receipt, ownerUID) }
	if plan.Storage.Choice == StorageBtrfsImage {
		runnerStorage, found := plannedPathByName(plan.Paths, "runner-storage")
		if !found {
			return receipt, installerError("private host purge Runner storage is absent from plan", nil)
		}
		unitResource, err := requirePrivilegedPurgeResource(plan, receipt, "workspace-mount-unit")
		if err != nil {
			return receipt, err
		}
		if err := validatePurgeTargetMetadata(unitResource); err != nil {
			return receipt, err
		}
		unit := plan.Storage.MountUnitPath
		expectedUnitDigest := Digest([]byte(MountUnit(plan.Storage.FilesystemImagePath, runnerStorage.Path)))
		unitPresent, err := validateRegularPathIfExists(unit, expectedUnitDigest)
		if err != nil {
			return receipt, err
		}
		if unitPresent {
			if err := runPurgeCommand(ctx, "systemctl", "disable", "--now", filepath.Base(unit)); err != nil {
				return receipt, err
			}
		}
		if err := unix.Unmount(runnerStorage.Path, 0); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return receipt, installerError("unmount purged Runner storage filesystem", err)
		}
		if _, err := removeRegularPathIfExists(unit, expectedUnitDigest); err != nil {
			return receipt, err
		}
		if err := runPurgeCommand(ctx, "systemctl", "daemon-reload"); err != nil {
			return receipt, err
		}
	}
	runnerRoot, found := plannedPathByName(plan.Paths, "runner-root")
	if !found {
		return receipt, installerError("private host purge Runner root is absent from plan", nil)
	}
	if _, err := removeTreeIfExists(runnerRoot.Path); err != nil {
		return receipt, installerError("purge Runner root", err)
	}
	for _, resource := range receipt.CreatedResources {
		planned, found := plannedPathByName(plan.Paths, resource.ID)
		if !found || !planned.RequiresSudo {
			continue
		}
		if resource.Path != planned.Path || resource.Kind != planned.Kind || resource.OwnerUID != planned.OwnerUID || resource.OwnerGID != planned.OwnerGID {
			return receipt, installerError("privileged purge resource differs from accepted plan: "+resource.ID, nil)
		}
		if err := receipt.MarkResourceRemoved(resource.ID, now()); err != nil {
			return receipt, err
		}
	}
	if err := persist(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// ValidateAcceptedHostPurge proves that the privileged purge boundary is still
// exactly the one accepted by the plan and receipt without mutating it. The
// public purge command runs this before removing Compose volumes or artifacts;
// PurgeAcceptedHost repeats it immediately before privileged deletion.
func ValidateAcceptedHostPurge(directory, expectedDigest string, ownerUID int) (resultErr error) {
	if os.Geteuid() != 0 || ownerUID < 0 {
		return installerError("private host purge validation requires root and SUDO_UID", nil)
	}
	lock, err := AcquireLock(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err := RecoverOperation(directory, ownerUID, lock)
	if err != nil {
		return err
	}
	return validateAcceptedHostPurge(plan, receipt, expectedDigest)
}

func validateAcceptedHostPurge(plan InstallPlan, receipt InstallReceipt, expectedDigest string) error {
	digest, err := PlanDigest(plan)
	if err != nil || digest != expectedDigest {
		return installerError("private host purge plan digest differs from the accepted plan", err)
	}
	if receipt.Status != OperationPurging {
		return installerError("private host purge requires ordinary uninstall first", nil)
	}
	runnerRoot, err := requirePrivilegedPurgeResource(plan, receipt, "runner-root")
	if err != nil {
		return err
	}
	runnerStorage, err := requirePrivilegedPurgeResource(plan, receipt, "runner-storage")
	if err != nil {
		return err
	}
	workspace, err := requirePrivilegedPurgeResource(plan, receipt, "workspace")
	if err != nil {
		return err
	}
	for _, resource := range []CreatedResource{runnerRoot, runnerStorage, workspace} {
		if err := validatePurgeTargetMetadata(resource); err != nil {
			return err
		}
	}
	if err := validatePurgeWorkspaceIdentity(plan, receipt, workspace); err != nil {
		return err
	}
	if err := validateNoNestedMounts(runnerStorage.Path); err != nil {
		return err
	}
	if plan.Storage.Choice != StorageBtrfsImage {
		return nil
	}
	unit, err := requirePrivilegedPurgeResource(plan, receipt, "workspace-mount-unit")
	if err != nil {
		return err
	}
	if err := validatePurgeTargetMetadata(unit); err != nil {
		return err
	}
	plannedStorage, found := plannedPathByName(plan.Paths, "runner-storage")
	if !found {
		return installerError("private host purge Runner storage is absent from plan", nil)
	}
	expectedUnitDigest := Digest([]byte(MountUnit(plan.Storage.FilesystemImagePath, plannedStorage.Path)))
	_, err = validateRegularPathIfExists(plan.Storage.MountUnitPath, expectedUnitDigest)
	return err
}

func validateNoNestedMounts(root string) error {
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return installerError("purge cannot inspect the host mount table", err)
	}
	return validateNoNestedMountsInfo(root, content)
}

func validateNoNestedMountsInfo(root string, content []byte) error {
	root = filepath.Clean(root)
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		mountpoint := filepath.Clean(decodeMount(fields[4]))
		relative, err := filepath.Rel(root, mountpoint)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return installerError("recursive removal refuses a nested mount beneath "+root+": "+mountpoint, nil)
	}
	return nil
}

func requirePrivilegedPurgeResource(plan InstallPlan, receipt InstallReceipt, id string) (CreatedResource, error) {
	planned, found := plannedPathByName(plan.Paths, id)
	if !found || !planned.RequiresSudo {
		return CreatedResource{}, installerError("privileged purge target is absent from the accepted plan: "+id, nil)
	}
	resource, found := receiptResource(receipt, id)
	if !found || resource.Path != planned.Path || resource.Kind != planned.Kind || resource.Mode != planned.Mode || resource.OwnerUID != planned.OwnerUID || resource.OwnerGID != planned.OwnerGID {
		return CreatedResource{}, installerError("privileged purge target lacks exact plan-and-receipt authority: "+id, nil)
	}
	return resource, nil
}

func validatePurgeTargetMetadata(resource CreatedResource) error {
	info, err := os.Lstat(resource.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !resourceKindMatches(resource.Kind, info.Mode()) || info.Mode().Perm() != os.FileMode(resource.Mode) {
		return installerError("privileged purge target kind or mode changed: "+resource.ID, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != resource.OwnerUID || int64(stat.Gid) != resource.OwnerGID {
		return installerError("privileged purge target ownership changed: "+resource.ID, nil)
	}
	return nil
}

func validatePurgeWorkspaceIdentity(plan InstallPlan, receipt InstallReceipt, resource CreatedResource) error {
	return validatePurgeWorkspaceIdentityWith(plan, receipt, resource, workspaceFilesystemIdentity)
}

func validatePurgeWorkspaceIdentityWith(plan InstallPlan, receipt InstallReceipt, resource CreatedResource, identify func(string) (string, error)) error {
	if plan.Storage.Choice == StorageExistingMount {
		if err := verifyExistingWorkspaceMount(plan, false); err != nil {
			return installerError("purge requires the accepted existing Workspace filesystem to remain mounted", err)
		}
	}
	_, err := os.Lstat(resource.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	hostApply, found := completedStage(receipt, StageHostApply)
	identity, identityErr := identify(resource.Path)
	if identityErr != nil || !found || hostApply.Evidence["workspaceDeviceIdentity"] == "" || identity != hostApply.Evidence["workspaceDeviceIdentity"] {
		return installerError("purge Workspace device identity differs from the completed host apply", nil)
	}
	return nil
}

// PurgeVerifiedArtifacts removes the independently verified execution assets
// before the privileged Runner storage root may be removed. The release
// manifest remains outside that root as the verification authority.
func PurgeVerifiedArtifacts(plan InstallPlan, receipt InstallReceipt, now func() time.Time, persist func(InstallReceipt) error) (InstallReceipt, error) {
	if receipt.Status != OperationPurging || now == nil || persist == nil {
		return receipt, installerError("verified artifact purge requires a purge-in-progress receipt, clock, and persistence", nil)
	}
	if slices.Contains(receipt.RemovedResourceIDs, "artifacts") {
		return receipt, nil
	}
	artifactPath, artifactPresent, err := validatePurgeVerifiedArtifacts(plan, receipt)
	if err != nil {
		return receipt, err
	}
	if artifactPresent {
		if _, err := removeTreeIfExists(artifactPath.Path); err != nil {
			return receipt, installerError("purge verified artifacts", err)
		}
	}
	if err := receipt.MarkResourceRemoved("artifacts", now()); err != nil {
		return receipt, err
	}
	return receipt, persist(receipt)
}

// ValidatePurgeVerifiedArtifacts proves the complete release-owned artifact
// directory against the still-present signed release manifest without removing
// either one.
func ValidatePurgeVerifiedArtifacts(plan InstallPlan, receipt InstallReceipt) error {
	_, _, err := validatePurgeVerifiedArtifacts(plan, receipt)
	return err
}

func validatePurgeVerifiedArtifacts(plan InstallPlan, receipt InstallReceipt) (PlannedPath, bool, error) {
	if receipt.Status != OperationPurging {
		return PlannedPath{}, false, installerError("verified artifact purge validation requires a purge-in-progress receipt", nil)
	}
	if slices.Contains(receipt.RemovedResourceIDs, "artifacts") {
		return PlannedPath{}, false, nil
	}
	releasePath, found := plannedPathByName(plan.Paths, "release-artifact-manifest")
	if !found || slices.Contains(receipt.RemovedResourceIDs, "release-artifact-manifest") {
		return PlannedPath{}, false, installerError("purge release manifest must remain before verified artifacts", nil)
	}
	artifactPath, found := plannedPathByName(plan.Paths, "artifacts")
	if !found || artifactPath.RequiresSudo {
		return PlannedPath{}, false, installerError("purge artifact directory is absent from the user-owned plan boundary", nil)
	}
	resource, recorded := receiptResource(receipt, "artifacts")
	if !recorded || resource.Path != artifactPath.Path || resource.Kind != artifactPath.Kind || resource.OwnerUID != artifactPath.OwnerUID || resource.OwnerGID != artifactPath.OwnerGID {
		return PlannedPath{}, false, installerError("purge artifacts lack exact plan-and-receipt authority", nil)
	}
	releaseInfo, err := os.Lstat(releasePath.Path)
	if err != nil || releaseInfo.Mode()&os.ModeSymlink != 0 || !releaseInfo.Mode().IsRegular() {
		return PlannedPath{}, false, installerError("purge release manifest must remain a regular file", err)
	}
	releaseBytes, err := os.ReadFile(releasePath.Path)
	if err != nil {
		return PlannedPath{}, false, err
	}
	release, err := releasecontract.DecodeArtifactManifest(releaseBytes)
	if err != nil || releasecontract.Digest(releaseBytes) != plan.Release.ArtifactManifestDigest {
		return PlannedPath{}, false, installerError("purge release manifest differs from accepted release", err)
	}
	artifactInfo, err := os.Lstat(artifactPath.Path)
	if errors.Is(err, os.ErrNotExist) {
		return artifactPath, false, nil
	}
	if err != nil || artifactInfo.Mode()&os.ModeSymlink != 0 || !artifactInfo.IsDir() {
		return PlannedPath{}, false, installerError("purge artifact target must remain a directory", err)
	}
	if _, err := VerifyArtifactDirectory(artifactPath.Path, release); err != nil {
		return PlannedPath{}, false, err
	}
	return artifactPath, true, nil
}

// ValidatePurgeUserResources checks every remaining user-owned target without
// deleting it. It catches changed files and closed directory allowlists before
// the public purge crosses any destructive boundary.
func ValidatePurgeUserResources(plan InstallPlan, receipt InstallReceipt) error {
	if receipt.Status != OperationPurging {
		return installerError("user resource purge validation requires a purge-in-progress receipt", nil)
	}
	for _, resource := range receipt.CreatedResources {
		if resource.ID == "operation-directory" || resource.ID == "compose-project" || slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
			continue
		}
		planned, found := plannedPathByName(plan.Paths, resource.ID)
		if !found {
			return installerError("user purge validation resource is absent from the accepted plan: "+resource.ID, nil)
		}
		if planned.RequiresSudo {
			continue
		}
		if resource.Path != planned.Path || resource.Kind != planned.Kind || resource.Mode != planned.Mode || resource.OwnerUID != planned.OwnerUID || resource.OwnerGID != planned.OwnerGID {
			return installerError("user purge validation resource differs from accepted plan: "+resource.ID, nil)
		}
		if err := validatePurgeTargetMetadata(resource); err != nil {
			return err
		}
		switch resource.ID {
		case "runner-identity":
			if err := validateRunnerIdentityDirectory(resource.Path); err != nil {
				return err
			}
		case "compose-assets":
			if err := validateComposeAssetDirectory(resource.Path); err != nil {
				return err
			}
		default:
			if resource.Kind != ResourceDirectory {
				if resource.Digest == "" {
					return installerError("purge regular resource lacks recorded content identity: "+resource.ID, nil)
				}
				if _, err := validateRegularPathIfExists(resource.Path, resource.Digest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// PurgeUserResources removes remaining create-only user-owned resources
// deepest-first and persists the removal ledger after every target.
func PurgeUserResources(plan InstallPlan, receipt InstallReceipt, now func() time.Time, persist func(InstallReceipt) error) (InstallReceipt, error) {
	if receipt.Status != OperationPurging || now == nil || persist == nil {
		return receipt, installerError("user resource purge requires a purge-in-progress receipt, clock, and persistence", nil)
	}
	if !slices.Contains(receipt.RemovedResourceIDs, "artifacts") {
		return receipt, installerError("verified artifacts remain before the sudo purge boundary", nil)
	}
	if err := ValidatePurgeUserResources(plan, receipt); err != nil {
		return receipt, err
	}
	resources := slices.Clone(receipt.CreatedResources)
	slices.SortFunc(resources, func(a, b CreatedResource) int {
		if a.ID == "release-artifact-manifest" && b.ID != a.ID {
			return 1
		}
		if b.ID == "release-artifact-manifest" && a.ID != b.ID {
			return -1
		}
		return strings.Count(b.Path, string(filepath.Separator)) - strings.Count(a.Path, string(filepath.Separator))
	})
	for _, resource := range resources {
		if resource.ID == "operation-directory" || resource.ID == "compose-project" || slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
			continue
		}
		planned, found := plannedPathByName(plan.Paths, resource.ID)
		if !found || planned.RequiresSudo || resource.Path != planned.Path || resource.Kind != planned.Kind || resource.OwnerUID != planned.OwnerUID || resource.OwnerGID != planned.OwnerGID {
			if found && planned.RequiresSudo {
				return receipt, installerError("privileged purge resource remains after sudo boundary: "+resource.ID, nil)
			}
			return receipt, installerError("user purge resource differs from accepted plan: "+resource.ID, nil)
		}
		var err error
		switch resource.ID {
		case "artifacts":
			_, err = removeTreeIfExists(resource.Path)
		case "runner-identity":
			err = validateRunnerIdentityDirectory(resource.Path)
			if err == nil {
				_, err = removeTreeIfExists(resource.Path)
			}
		case "compose-assets":
			err = validateComposeAssetDirectory(resource.Path)
			if err == nil {
				_, err = removeTreeIfExists(resource.Path)
			}
		case "secondbox-binary", "secondbox-deploy-binary":
			if resource.Digest == "" {
				return receipt, installerError("purge regular resource lacks recorded content identity: "+resource.ID, nil)
			}
			_, err = removeRegularPathIfExists(resource.Path, resource.Digest)
		default:
			if resource.Kind == ResourceDirectory {
				_, err = removeEmptyDirectoryIfExists(resource.Path)
			} else {
				if resource.Digest == "" {
					return receipt, installerError("purge regular resource lacks recorded content identity: "+resource.ID, nil)
				}
				_, err = removeRegularPathIfExists(resource.Path, resource.Digest)
			}
		}
		if err != nil {
			return receipt, installerError("purge user resource "+resource.ID, err)
		}
		if err := receipt.MarkResourceRemoved(resource.ID, now()); err != nil {
			return receipt, err
		}
		if err := persist(receipt); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

func runPurgeCommand(ctx context.Context, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if len(output) > 64<<10 {
		output = output[:64<<10]
	}
	if err != nil {
		return fmt.Errorf("SecondBox installer purge %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeTreeIfExists(path string) (bool, error) {
	parentFD, name, err := openParentForRemoval(path)
	if err != nil {
		return false, err
	}
	defer unix.Close(parentFD)
	fd, err := unix.Openat2(parentFD, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var rootStat unix.Stat_t
	if err := unix.Fstat(fd, &rootStat); err != nil {
		_ = unix.Close(fd)
		return false, err
	}
	if err := removeDirectoryContents(fd, uint64(rootStat.Dev)); err != nil {
		_ = unix.Close(fd)
		return false, err
	}
	if err := unix.Close(fd); err != nil {
		return false, err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != rootStat.Dev || current.Ino != rootStat.Ino || current.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, installerError("purge directory changed during removal", err)
	}
	return true, unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func removeDirectoryContents(fd int, rootDevice uint64) error {
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(duplicate), "purge-directory")
	if directory == nil {
		_ = unix.Close(duplicate)
		return errors.New("adopt purge directory")
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err := errors.Join(readErr, closeErr); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, name := range names {
		childFD, openErr := unix.Openat2(fd, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
		if openErr == nil {
			var stat unix.Stat_t
			if err := unix.Fstat(childFD, &stat); err != nil || uint64(stat.Dev) != rootDevice {
				_ = unix.Close(childFD)
				return installerError("purge refuses a nested mount or changed directory", err)
			}
			if err := removeDirectoryContents(childFD, rootDevice); err != nil {
				_ = unix.Close(childFD)
				return err
			}
			if err := unix.Close(childFD); err != nil {
				return err
			}
			if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(openErr, unix.ENOTDIR) && !errors.Is(openErr, unix.ELOOP) {
			return openErr
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			return err
		}
	}
	return nil
}

func removeRegularPathIfExists(path, expectedDigest string) (bool, error) {
	parentFD, name, err := openParentForRemoval(path)
	if err != nil {
		return false, err
	}
	defer unix.Close(parentFD)
	fd, err := unix.Openat2(parentFD, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return false, errors.New("adopt purge file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return false, installerError("purge target must remain a regular file", err)
	}
	openedStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		_ = file.Close()
		return false, installerError("inspect purge file identity", nil)
	}
	if expectedDigest != "" {
		hash := sha256.New()
		_, hashErr := io.Copy(hash, file)
		actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if hashErr != nil || actualDigest != expectedDigest {
			_ = file.Close()
			return false, installerError("purge file digest differs from receipt", hashErr)
		}
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != openedStat.Dev || current.Ino != openedStat.Ino || current.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, installerError("purge file changed during removal", err)
	}
	return true, unix.Unlinkat(parentFD, name, 0)
}

func validateRegularPathIfExists(path, expectedDigest string) (bool, error) {
	parentFD, name, err := openParentForRemoval(path)
	if err != nil {
		return false, err
	}
	defer unix.Close(parentFD)
	fd, err := unix.Openat2(parentFD, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return false, errors.New("adopt purge file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return false, installerError("purge target must remain a regular file", err)
	}
	if expectedDigest != "" {
		hash := sha256.New()
		_, hashErr := io.Copy(hash, file)
		actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if hashErr != nil || actualDigest != expectedDigest {
			_ = file.Close()
			return false, installerError("purge file digest differs from receipt", hashErr)
		}
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func removeEmptyDirectoryIfExists(path string) (bool, error) {
	parentFD, name, err := openParentForRemoval(path)
	if err != nil {
		return false, err
	}
	defer unix.Close(parentFD)
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func openParentForRemoval(path string) (int, string, error) {
	if err := validateSafePath(path); err != nil {
		return -1, "", err
	}
	parent := filepath.Dir(path)
	fd, err := openDirectoryReadOnlyNoSymlinks(parent)
	return fd, filepath.Base(path), err
}

func validateRunnerIdentityDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	want := map[string]os.FileMode{"egress-contexts.json": 0o600, "runner-ca.crt": 0o644, "runner.crt": 0o600, "runner.env": 0o600, "runner.key": 0o600}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(path, entry.Name()))
		expectedMode, allowed := want[entry.Name()]
		if err != nil || !allowed || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != expectedMode {
			return installerError("Runner identity purge found an unexpected or exposed entry", err)
		}
		actual = append(actual, entry.Name())
	}
	slices.Sort(actual)
	return boolError(slices.Equal(actual, []string{"egress-contexts.json", "runner-ca.crt", "runner.crt", "runner.env", "runner.key"}), "Runner identity purge allowlist differs")
}

func validateComposeAssetDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	allow := map[string]bool{"compose.yml": true, "compose.development.yml": true, "compose.explicit-network.yml": true, "compose.same-host-runner.yml": true}
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(path, entry.Name()))
		if err != nil || !allow[entry.Name()] || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			return installerError("Compose asset purge found an unexpected entry", err)
		}
	}
	return nil
}

func boolError(condition bool, message string) error {
	if condition {
		return nil
	}
	return installerError(message, nil)
}
