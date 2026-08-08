//go:build linux

package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maximumAcceptedDocumentBytes = 8 << 20

func ReadAccepted(directory, expectedPlanDigest string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	if err := validateSafePath(directory); err != nil {
		return InstallPlan{}, InstallReceipt{}, installerError("accepted operation directory", err)
	}
	if !digestPattern.MatchString(expectedPlanDigest) || ownerUID < 0 {
		return InstallPlan{}, InstallReceipt{}, installerError("accepted plan digest or owner is invalid", nil)
	}
	directoryFD, err := openDirectoryNoSymlinks(directory)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, installerError("open accepted operation directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	planBytes, err := readAcceptedFile(directoryFD, "install-plan.json", ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	plan, err := DecodePlan(planBytes)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	actualDigest, err := PlanDigest(plan)
	if err != nil || actualDigest != expectedPlanDigest {
		return InstallPlan{}, InstallReceipt{}, installerError("caller-supplied plan digest does not match the accepted plan", err)
	}
	receiptBytes, err := readAcceptedFile(directoryFD, "install-receipt.json", ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	receipt, err := DecodeReceipt(receiptBytes, plan)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	if len(receipt.CompletedStages) != 2 || receipt.CompletedStages[0].Stage != StagePreflight || receipt.CompletedStages[1].Stage != StagePlanAccepted || receipt.Status != OperationRunning {
		return InstallPlan{}, InstallReceipt{}, installerError("accepted receipt is not at the immutable host-apply boundary", nil)
	}
	return plan, receipt, nil
}

// ReadOperation securely reloads a durable installer operation at any stage.
func ReadOperation(directory string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	if err := validateSafePath(directory); err != nil || ownerUID < 0 {
		return InstallPlan{}, InstallReceipt{}, installerError("operation directory or owner is invalid", err)
	}
	directoryFD, err := openDirectoryNoSymlinks(directory)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, installerError("open operation directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	planBytes, err := readAcceptedFile(directoryFD, "install-plan.json", ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	plan, err := DecodePlan(planBytes)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	receiptBytes, err := readAcceptedFile(directoryFD, "install-receipt.json", ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	receipt, err := DecodeReceipt(receiptBytes, plan)
	return plan, receipt, err
}

// SaveReceipt atomically persists a validated receipt inside its operation.
func SaveReceipt(directory string, plan InstallPlan, receipt InstallReceipt, ownerUID int) error {
	digest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return err
	}
	return writeReceiptAtomic(directory, receipt, ownerUID)
}

func openDirectoryNoSymlinks(path string) (int, error) {
	return openDirectoryNoSymlinksWithFlags(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC)
}

func openDirectoryReadOnlyNoSymlinks(path string) (int, error) {
	return openDirectoryNoSymlinksWithFlags(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
}

func openDirectoryNoSymlinksWithFlags(path string, flags uint64) (int, error) {
	root, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	relative := strings.TrimPrefix(filepath.Clean(path), "/")
	return unix.Openat2(root, relative, &unix.OpenHow{Flags: flags, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
}

func readAcceptedFile(directoryFD int, name string, ownerUID int) ([]byte, error) {
	fd, err := unix.Openat2(directoryFD, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, installerError("open accepted "+name+" without symbolic links", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, installerError("adopt accepted "+name, nil)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, installerError("inspect accepted "+name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Uid != uint32(ownerUID) || stat.Nlink != 1 || stat.Size < 1 || stat.Size > maximumAcceptedDocumentBytes {
		return nil, installerError("accepted "+name+" must be a singly-linked mode-0600 regular file owned by SUDO_UID", nil)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumAcceptedDocumentBytes+1))
	if err != nil {
		return nil, installerError("read accepted "+name, err)
	}
	if len(content) > maximumAcceptedDocumentBytes {
		return nil, installerError("accepted "+name+" exceeds the size limit", nil)
	}
	return content, nil
}

func writeReceiptAtomic(directory string, receipt InstallReceipt, ownerUID int) error {
	encoded, err := Canonical(receipt)
	if err != nil {
		return err
	}
	directoryFD, err := openDirectoryReadOnlyNoSymlinks(directory)
	if err != nil {
		return installerError("open receipt directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return installerError("generate receipt update name", err)
	}
	temporaryName := ".install-receipt-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(directoryFD, temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return installerError("create receipt update", err)
	}
	temporary := os.NewFile(uintptr(fd), temporaryName)
	if temporary == nil {
		_ = unix.Close(fd)
		return installerError("adopt receipt update", nil)
	}
	cleanup := func() { _ = unix.Unlinkat(directoryFD, temporaryName, 0) }
	defer cleanup()
	if err := errors.Join(unix.Fchmod(fd, 0o600), unix.Fchown(fd, ownerUID, -1)); err != nil {
		_ = temporary.Close()
		return installerError("secure receipt update", err)
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		return installerError("write receipt update", err)
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return installerError("sync receipt update", err)
	}
	if err := unix.Renameat(directoryFD, temporaryName, directoryFD, "install-receipt.json"); err != nil {
		return installerError("publish receipt update", err)
	}
	if err := unix.Fsync(directoryFD); err != nil {
		return installerError("sync receipt directory", err)
	}
	return nil
}
