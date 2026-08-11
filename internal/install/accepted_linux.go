//go:build linux

package install

import (
	"bytes"
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

const (
	operationPlanStageName    = ".install-plan-update"
	operationReceiptStageName = ".install-receipt-update"
	operationCommitMarkerName = ".install-update-commit"
)

type operationCommitMarker struct {
	SchemaVersion         string `json:"schemaVersion"`
	OperationID           string `json:"operationId"`
	PlanDocumentDigest    string `json:"planDocumentDigest"`
	ReceiptDocumentDigest string `json:"receiptDocumentDigest"`
}

func ReadAccepted(directory, expectedPlanDigest string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	plan, receipt, err := readOperationWithDigest(directory, expectedPlanDigest, ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	if len(receipt.CompletedStages) != 2 || receipt.CompletedStages[0].Stage != StagePreflight || receipt.CompletedStages[1].Stage != StagePlanAccepted || receipt.Status != OperationRunning {
		return InstallPlan{}, InstallReceipt{}, installerError("accepted receipt is not at the immutable host-apply boundary", nil)
	}
	return plan, receipt, nil
}

// ReadHostApply securely loads either the immutable pre-apply boundary or a
// receipt whose host apply has completed. The latter authorizes only the
// privileged read-only replay implemented by ApplyHost.
func ReadHostApply(directory, expectedPlanDigest string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	plan, receipt, err := readOperationWithDigest(directory, expectedPlanDigest, ownerUID)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	accepted := len(receipt.CompletedStages) == 2 && receipt.CompletedStages[0].Stage == StagePreflight && receipt.CompletedStages[1].Stage == StagePlanAccepted && receipt.Status == OperationRunning
	_, completed := completedStage(receipt, StageHostApply)
	if !accepted && !completed {
		return InstallPlan{}, InstallReceipt{}, installerError("receipt is not at or beyond the immutable host-apply boundary", nil)
	}
	return plan, receipt, nil
}

func readOperationWithDigest(directory, expectedPlanDigest string, ownerUID int) (InstallPlan, InstallReceipt, error) {
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
	return plan, receipt, nil
}

// ReadOperation securely reloads a durable installer operation at any stage.
func ReadOperation(directory string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	if err := validateSafePath(directory); err != nil || ownerUID < 0 {
		return InstallPlan{}, InstallReceipt{}, installerError("operation directory or owner is invalid", err)
	}
	if err := recoverOperationCommit(directory, ownerUID); err != nil {
		return InstallPlan{}, InstallReceipt{}, err
	}
	return readOperationDocuments(directory, ownerUID)
}

// ReadOperationReadOnly securely reloads an installer operation without
// completing a pending plan/receipt commit. Callers that promise not to mutate
// deployment state must direct the operator to resume recovery instead.
func ReadOperationReadOnly(directory string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	if err := validateSafePath(directory); err != nil || ownerUID < 0 {
		return InstallPlan{}, InstallReceipt{}, installerError("operation directory or owner is invalid", err)
	}
	directoryFD, err := openDirectoryNoSymlinks(directory)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, installerError("open operation directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	for _, name := range []string{operationPlanStageName, operationReceiptStageName, operationCommitMarkerName} {
		if err := unix.Fstatat(directoryFD, name, &unix.Stat_t{}, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return InstallPlan{}, InstallReceipt{}, installerError("operation has pending commit recovery; run update --resume", nil)
		} else if err != unix.ENOENT {
			return InstallPlan{}, InstallReceipt{}, installerError("inspect operation commit state", err)
		}
	}
	return readOperationDocumentsFromFD(directoryFD, ownerUID)
}

func readOperationDocuments(directory string, ownerUID int) (InstallPlan, InstallReceipt, error) {
	directoryFD, err := openDirectoryNoSymlinks(directory)
	if err != nil {
		return InstallPlan{}, InstallReceipt{}, installerError("open operation directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	return readOperationDocumentsFromFD(directoryFD, ownerUID)
}

func readOperationDocumentsFromFD(directoryFD, ownerUID int) (InstallPlan, InstallReceipt, error) {
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

// SaveOperation atomically advances the plan and receipt as one recoverable
// logical commit. A protected marker lets ReadOperation finish either rename
// after process or host failure; it never accepts a mixed plan/receipt pair.
func SaveOperation(directory string, plan InstallPlan, receipt InstallReceipt, ownerUID int) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	planDigest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	if err := receipt.Validate(planDigest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return err
	}
	if err := validateUpdateHistory(plan, receipt); err != nil {
		return err
	}
	planBytes, err := Canonical(plan)
	if err != nil {
		return err
	}
	receiptBytes, err := Canonical(receipt)
	if err != nil {
		return err
	}
	marker := operationCommitMarker{SchemaVersion: "secondbox.install.operation-commit/v1", OperationID: plan.OperationID, PlanDocumentDigest: Digest(planBytes), ReceiptDocumentDigest: Digest(receiptBytes)}
	markerBytes, err := Canonical(marker)
	if err != nil {
		return err
	}
	directoryFD, err := openDirectoryReadOnlyNoSymlinks(directory)
	if err != nil {
		return installerError("open operation commit directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	for _, name := range []string{operationPlanStageName, operationReceiptStageName, operationCommitMarkerName} {
		if err := unix.Fstatat(directoryFD, name, &unix.Stat_t{}, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return installerError("operation commit already has pending state", nil)
		} else if err != unix.ENOENT {
			return installerError("inspect operation commit state", err)
		}
	}
	if err := writeOperationDocument(directoryFD, operationPlanStageName, planBytes, ownerUID); err != nil {
		return err
	}
	cleanupPlan := true
	defer func() {
		if cleanupPlan {
			_ = unix.Unlinkat(directoryFD, operationPlanStageName, 0)
		}
	}()
	if err := writeOperationDocument(directoryFD, operationReceiptStageName, receiptBytes, ownerUID); err != nil {
		return err
	}
	cleanupReceipt := true
	defer func() {
		if cleanupReceipt {
			_ = unix.Unlinkat(directoryFD, operationReceiptStageName, 0)
		}
	}()
	if err := writeOperationDocument(directoryFD, operationCommitMarkerName, markerBytes, ownerUID); err != nil {
		return err
	}
	if err := unix.Fsync(directoryFD); err != nil {
		return installerError("sync operation commit intent", err)
	}
	cleanupPlan, cleanupReceipt = false, false
	return finishOperationCommit(directoryFD, ownerUID, marker)
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

func recoverOperationCommit(directory string, ownerUID int) error {
	directoryFD, err := openDirectoryReadOnlyNoSymlinks(directory)
	if err != nil {
		return installerError("open operation recovery directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	var stat unix.Stat_t
	if err := unix.Fstatat(directoryFD, operationCommitMarkerName, &stat, unix.AT_SYMLINK_NOFOLLOW); err == unix.ENOENT {
		for _, name := range []string{operationPlanStageName, operationReceiptStageName} {
			if stageErr := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); stageErr == nil {
				return installerError("operation contains uncommitted staged document without commit intent", nil)
			} else if stageErr != unix.ENOENT {
				return installerError("inspect staged operation document", stageErr)
			}
		}
		return nil
	} else if err != nil {
		return installerError("inspect operation commit marker", err)
	}
	content, err := readAcceptedFile(directoryFD, operationCommitMarkerName, ownerUID)
	if err != nil {
		return err
	}
	var marker operationCommitMarker
	if err := decodeStrict(content, &marker); err != nil || marker.SchemaVersion != "secondbox.install.operation-commit/v1" || !operationPattern.MatchString(marker.OperationID) || !digestPattern.MatchString(marker.PlanDocumentDigest) || !digestPattern.MatchString(marker.ReceiptDocumentDigest) {
		return installerError("operation commit marker is invalid", err)
	}
	return finishOperationCommit(directoryFD, ownerUID, marker)
}

func finishOperationCommit(directoryFD, ownerUID int, marker operationCommitMarker) error {
	for _, document := range []struct {
		current string
		staged  string
		digest  string
	}{{"install-plan.json", operationPlanStageName, marker.PlanDocumentDigest}, {"install-receipt.json", operationReceiptStageName, marker.ReceiptDocumentDigest}} {
		current, err := readAcceptedFile(directoryFD, document.current, ownerUID)
		if err != nil {
			return err
		}
		if Digest(bytes.TrimSuffix(current, []byte{'\n'})) == document.digest {
			if err := unix.Unlinkat(directoryFD, document.staged, 0); err != nil && err != unix.ENOENT {
				return installerError("remove adopted staged operation document", err)
			}
			continue
		}
		staged, err := readAcceptedFile(directoryFD, document.staged, ownerUID)
		if err != nil || Digest(bytes.TrimSuffix(staged, []byte{'\n'})) != document.digest {
			return installerError("staged operation document differs from commit intent", err)
		}
		if err := unix.Renameat(directoryFD, document.staged, directoryFD, document.current); err != nil {
			return installerError("publish staged operation document", err)
		}
		if err := unix.Fsync(directoryFD); err != nil {
			return installerError("sync published operation document", err)
		}
	}
	planBytes, err := readAcceptedFile(directoryFD, "install-plan.json", ownerUID)
	if err != nil {
		return err
	}
	plan, err := DecodePlan(planBytes)
	if err != nil || plan.OperationID != marker.OperationID {
		return installerError("committed operation plan is invalid", err)
	}
	receiptBytes, err := readAcceptedFile(directoryFD, "install-receipt.json", ownerUID)
	if err != nil {
		return err
	}
	if _, err := DecodeReceipt(receiptBytes, plan); err != nil {
		return installerError("committed operation receipt is invalid", err)
	}
	if err := unix.Unlinkat(directoryFD, operationCommitMarkerName, 0); err != nil {
		return installerError("remove completed operation commit marker", err)
	}
	if err := unix.Fsync(directoryFD); err != nil {
		return installerError("sync completed operation commit", err)
	}
	return nil
}

func writeOperationDocument(directoryFD int, name string, content []byte, ownerUID int) error {
	fd, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return installerError("create staged operation document", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return installerError("adopt staged operation document", nil)
	}
	if err := errors.Join(unix.Fchmod(fd, 0o600), unix.Fchown(fd, ownerUID, -1)); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(directoryFD, name, 0)
		return installerError("secure staged operation document", err)
	}
	if _, err := file.Write(append(content, '\n')); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(directoryFD, name, 0)
		return installerError("write staged operation document", err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		_ = unix.Unlinkat(directoryFD, name, 0)
		return installerError("sync staged operation document", err)
	}
	return nil
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
