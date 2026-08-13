package workspacestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type relocationExport struct {
	file    *os.File
	lock    *os.File
	size    int64
	receipt Receipt
}

func (export *relocationExport) Read(buffer []byte) (int, error) {
	return export.file.Read(buffer)
}

func (export *relocationExport) Close() error {
	if export == nil {
		return nil
	}
	var result error
	if export.file != nil {
		result = export.file.Close()
		export.file = nil
	}
	if export.lock != nil {
		result = errors.Join(result, closeLockedFile(export.lock))
		export.lock = nil
	}
	return result
}

func (export *relocationExport) SizeBytes() int64 {
	return export.size
}

func (export *relocationExport) Receipt() Receipt {
	return export.receipt
}

// OpenRelocationExport durably seals the source before exposing a bounded reader.
func (store *Store) OpenRelocationExport(
	ctx context.Context,
	request RelocationExportRequest,
) (RelocationExport, error) {
	receipt, err := store.mutate(
		ctx,
		request,
		request.Mutation,
		ReceiptRelocationExport,
		func() (Receipt, error) {
			manifest, err := store.readCurrentManifest(request.WorkspaceID)
			if err != nil {
				return Receipt{}, err
			}
			if manifest.Generation != request.ExpectedGeneration {
				return Receipt{}, ErrStaleGeneration
			}
			if pending, err := store.restorePending(request.WorkspaceID); err != nil {
				return Receipt{}, err
			} else if pending {
				return Receipt{}, ErrRestorePending
			}
			manifest.RelocationSealed = true
			if err := store.publishCurrentManifest(request.WorkspaceID, manifest); err != nil {
				return Receipt{}, err
			}
			return Receipt{
				Generation: manifest.Generation, CapacityBytes: manifest.CapacityBytes,
			}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return nil, err
	}
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	if manifest.Generation != request.ExpectedGeneration || !manifest.RelocationSealed {
		closeLockedFile(lock)
		return nil, ErrStaleGeneration
	}
	imagePath, err := store.validatedVersionPath(request.WorkspaceID, manifest.Image)
	if err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	if err := validateExt4ImageUUID(
		imagePath,
		manifest.CapacityBytes,
		deterministicUUID(request.WorkspaceID),
	); err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	image, err := os.Open(imagePath)
	if err != nil {
		closeLockedFile(lock)
		return nil, fmt.Errorf("SecondBox WorkspaceStore open relocation source image: %w", err)
	}
	return &relocationExport{
		file: image, lock: lock, size: manifest.CapacityBytes, receipt: receipt,
	}, nil
}

// AbortRelocation makes the original source writable only after a durable receipt.
func (store *Store) AbortRelocation(
	ctx context.Context,
	request RelocationExportRequest,
) (Receipt, error) {
	return store.mutate(
		ctx,
		request,
		request.Mutation,
		ReceiptRelocationAbort,
		func() (Receipt, error) {
			manifest, err := store.readCurrentManifest(request.WorkspaceID)
			if err != nil {
				return Receipt{}, err
			}
			if manifest.Generation != request.ExpectedGeneration {
				return Receipt{}, ErrStaleGeneration
			}
			manifest.RelocationSealed = false
			if err := store.publishCurrentManifest(request.WorkspaceID, manifest); err != nil {
				return Receipt{}, err
			}
			return Receipt{
				Generation: manifest.Generation, CapacityBytes: manifest.CapacityBytes,
			}, nil
		},
	)
}

type relocationImport struct {
	store   *Store
	request RelocationImportRequest
	digest  string
	file    *os.File
	lock    *os.File
	hash    hash.Hash
	written uint64
	receipt Receipt
	created bool
}

func (store *Store) BeginRelocationImport(
	ctx context.Context,
	request RelocationImportRequest,
) (RelocationImport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Generation == 0 || request.CapacityBytes < minimumExt4Bytes {
		return nil, ErrStorageIncompatible
	}
	if err := validateMutation(request.Mutation); err != nil {
		return nil, err
	}
	digest, err := inputDigest(request, request.FencingToken)
	if err != nil {
		return nil, err
	}
	if receipt, found, err := store.loadReceipt(
		request.Mutation,
		digest,
		ReceiptRelocationImport,
	); found || err != nil {
		if err == nil {
			manifest, manifestErr := store.readCurrentManifest(request.WorkspaceID)
			if manifestErr != nil || manifest.Generation != request.Generation ||
				manifest.CapacityBytes != request.CapacityBytes || manifest.RelocationSealed {
				return nil, fmt.Errorf("%w: relocation import receipt lacks its image", ErrCorruptState)
			}
			if manifestErr = store.validateImage(
				request.WorkspaceID,
				manifest.Image,
				request.CapacityBytes,
			); manifestErr != nil {
				return nil, manifestErr
			}
		}
		return &relocationImport{store: store, request: request, digest: digest, receipt: receipt}, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if receipt, found, err := store.loadReceipt(
		request.Mutation,
		digest,
		ReceiptRelocationImport,
	); found || err != nil {
		if err == nil {
			manifest, manifestErr := store.readCurrentManifest(request.WorkspaceID)
			if manifestErr != nil || manifest.Generation != request.Generation ||
				manifest.CapacityBytes != request.CapacityBytes || manifest.RelocationSealed {
				closeLockedFile(lock)
				return nil, fmt.Errorf("%w: relocation import receipt lacks its image", ErrCorruptState)
			}
			if manifestErr = store.validateImage(
				request.WorkspaceID,
				manifest.Image,
				request.CapacityBytes,
			); manifestErr != nil {
				closeLockedFile(lock)
				return nil, manifestErr
			}
		}
		closeLockedFile(lock)
		return &relocationImport{store: store, request: request, digest: digest, receipt: receipt}, err
	}
	imageName := generationImageName(request.Generation, "relocation-"+request.OperationID)
	if manifest, err := store.readCurrentManifest(request.WorkspaceID); err == nil {
		if manifest.Image != imageName || manifest.Generation != request.Generation ||
			manifest.CapacityBytes != request.CapacityBytes {
			closeLockedFile(lock)
			return nil, ErrConflictingReplay
		}
		if err := store.removeWorkspaceTree(request.WorkspaceID); err != nil {
			closeLockedFile(lock)
			return nil, err
		}
	} else if !errors.Is(err, ErrWorkspaceNotFound) {
		closeLockedFile(lock)
		return nil, err
	}
	if err := store.prepareReceiptDirectory(request.Mutation); err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	stagingDir := store.relocationDir(request.WorkspaceID, request.OperationID)
	if err := os.MkdirAll(stagingDir, privateDirectoryMode); err != nil {
		closeLockedFile(lock)
		return nil, fmt.Errorf("SecondBox WorkspaceStore create relocation staging: %w", err)
	}
	if err := requirePrivateDirectory(stagingDir); err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	imagePath := store.relocationImagePath(request.WorkspaceID, request.OperationID)
	if err := removeExactFile(imagePath); err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	templatePath, err := store.ensureTemplate(ctx, request.CapacityBytes)
	if err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	template, err := os.Open(templatePath)
	if err != nil {
		closeLockedFile(lock)
		return nil, fmt.Errorf("SecondBox WorkspaceStore open relocation capacity template: %w", err)
	}
	image, err := os.OpenFile(imagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, writableImageMode)
	if err != nil {
		_ = template.Close()
		closeLockedFile(lock)
		return nil, fmt.Errorf("SecondBox WorkspaceStore open relocation target staging: %w", err)
	}
	if err := store.driver.Clone(image, template); err != nil {
		_ = template.Close()
		_ = image.Close()
		_ = removeExactFile(imagePath)
		closeLockedFile(lock)
		return nil, fmt.Errorf("%w: SecondBox WorkspaceStore relocation FICLONE failed: %w", ErrStorageIncompatible, err)
	}
	if err := template.Close(); err != nil {
		_ = image.Close()
		_ = removeExactFile(imagePath)
		closeLockedFile(lock)
		return nil, err
	}
	if err := store.driver.ResetSparse(image, request.CapacityBytes); err != nil {
		_ = image.Close()
		_ = removeExactFile(imagePath)
		closeLockedFile(lock)
		return nil, err
	}
	return &relocationImport{
		store: store, request: request, digest: digest, file: image, lock: lock,
		hash: sha256.New(),
	}, nil
}

func (relocation *relocationImport) CompletedReceipt() (Receipt, bool) {
	return relocation.receipt, !relocation.receipt.RecordedAt.IsZero()
}

func (relocation *relocationImport) WriteChunk(offset uint64, data []byte) error {
	if relocation.file == nil || offset != relocation.written || len(data) == 0 {
		return ErrConflictingReplay
	}
	if relocation.written+uint64(len(data)) > uint64(relocation.request.CapacityBytes) {
		return ErrStorageIncompatible
	}
	if allZero(data) {
		if _, err := relocation.file.Seek(int64(len(data)), io.SeekCurrent); err != nil {
			return fmt.Errorf("SecondBox WorkspaceStore seek sparse relocation target: %w", err)
		}
	} else if _, err := relocation.file.Write(data); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore write relocation target: %w", err)
	}
	if _, err := relocation.hash.Write(data); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore hash relocation target: %w", err)
	}
	relocation.written += uint64(len(data))
	return nil
}

func (relocation *relocationImport) Complete(size uint64, checksum string) (Receipt, error) {
	if receipt, complete := relocation.CompletedReceipt(); complete {
		return receipt, nil
	}
	if relocation.file == nil || size != relocation.written ||
		size != uint64(relocation.request.CapacityBytes) {
		return Receipt{}, ErrStorageIncompatible
	}
	actualChecksum := "sha256:" + hex.EncodeToString(relocation.hash.Sum(nil))
	if checksum != actualChecksum {
		return Receipt{}, fmt.Errorf("%w: relocation checksum mismatch", ErrCorruptState)
	}
	if err := relocation.file.Truncate(relocation.request.CapacityBytes); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore size sparse relocation target: %w", err)
	}
	if err := relocation.file.Sync(); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore fsync relocation target: %w", err)
	}
	if err := relocation.file.Close(); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore close relocation target: %w", err)
	}
	relocation.file = nil
	stagedPath := relocation.store.relocationImagePath(
		relocation.request.WorkspaceID,
		relocation.request.OperationID,
	)
	if err := validateExt4ImageUUID(
		stagedPath,
		relocation.request.CapacityBytes,
		deterministicUUID(relocation.request.WorkspaceID),
	); err != nil {
		return Receipt{}, err
	}
	relocation.created = true
	if err := relocation.store.ensureWorkspaceLayout(relocation.request.WorkspaceID); err != nil {
		return Receipt{}, err
	}
	imageName := generationImageName(
		relocation.request.Generation,
		"relocation-"+relocation.request.OperationID,
	)
	imagePath := relocation.store.versionPath(relocation.request.WorkspaceID, imageName)
	if err := os.Rename(stagedPath, imagePath); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore publish relocation image: %w", err)
	}
	if err := relocation.store.driver.SyncDirectory(relocation.store.versionsDir(relocation.request.WorkspaceID)); err != nil {
		return Receipt{}, err
	}
	if err := relocation.store.publishCurrentManifest(
		relocation.request.WorkspaceID,
		currentManifest{
			FormatVersion: currentManifestFormatVersion,
			WorkspaceID:   relocation.request.WorkspaceID,
			Generation:    relocation.request.Generation,
			Image:         imageName,
			CapacityBytes: relocation.request.CapacityBytes,
		},
	); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{
		FormatVersion: receiptFormatVersion,
		Kind:          ReceiptRelocationImport,
		OperationID:   relocation.request.OperationID,
		WorkspaceID:   relocation.request.WorkspaceID,
		InputDigest:   relocation.digest,
		Generation:    relocation.request.Generation,
		CapacityBytes: relocation.request.CapacityBytes,
		Checksum:      actualChecksum,
	}
	receipt, err := relocation.store.recordReceipt(receipt, true)
	if err != nil {
		return Receipt{}, err
	}
	relocation.receipt = receipt
	if err := relocation.cleanupStaging(); err != nil {
		return Receipt{}, err
	}
	if err := closeLockedFile(relocation.lock); err != nil {
		return Receipt{}, err
	}
	relocation.lock = nil
	return receipt, nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func (relocation *relocationImport) Abort() error {
	if relocation == nil || !relocation.receipt.RecordedAt.IsZero() {
		return nil
	}
	var result error
	if relocation.file != nil {
		result = relocation.file.Close()
		relocation.file = nil
	}
	if relocation.created {
		result = errors.Join(
			result,
			relocation.store.removeWorkspaceTree(relocation.request.WorkspaceID),
			relocation.store.driver.SyncDirectory(relocation.store.workspacesRoot()),
		)
		relocation.created = false
	}
	result = errors.Join(result, relocation.cleanupStaging())
	if relocation.lock != nil {
		result = errors.Join(result, closeLockedFile(relocation.lock))
		relocation.lock = nil
	}
	return result
}

func (relocation *relocationImport) cleanupStaging() error {
	imagePath := relocation.store.relocationImagePath(
		relocation.request.WorkspaceID,
		relocation.request.OperationID,
	)
	if err := removeExactFile(imagePath); err != nil {
		return err
	}
	operationDir := relocation.store.relocationDir(
		relocation.request.WorkspaceID,
		relocation.request.OperationID,
	)
	if err := os.Remove(operationDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox WorkspaceStore remove relocation staging: %w", err)
	}
	workspaceDir := filepath.Dir(operationDir)
	if err := os.Remove(workspaceDir); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("SecondBox WorkspaceStore remove relocation workspace staging: %w", err)
	}
	return relocation.store.driver.SyncDirectory(relocation.store.relocationsRoot())
}
