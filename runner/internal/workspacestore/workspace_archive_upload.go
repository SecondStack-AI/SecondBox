package workspacestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// WorkspaceArchiveUpload keeps archive staging private to the WorkspaceStore.
// Its descriptor gives HTTP a bounded content length without buffering the disk.
type WorkspaceArchiveUpload struct {
	file       *os.File
	descriptor WorkspaceArchiveDescriptor
	receipt    Receipt
}

func (upload *WorkspaceArchiveUpload) Read(data []byte) (int, error) {
	return upload.file.Read(data)
}

func (upload *WorkspaceArchiveUpload) Descriptor() WorkspaceArchiveDescriptor {
	return upload.descriptor
}

func (upload *WorkspaceArchiveUpload) Receipt() Receipt {
	return upload.receipt
}

func (upload *WorkspaceArchiveUpload) Close() error {
	if upload.file == nil {
		return nil
	}
	file := upload.file
	upload.file = nil
	return errors.Join(file.Close(), removeExactFile(file.Name()))
}

type WorkspaceArchiveTransferStore interface {
	OpenWorkspaceArchiveUpload(context.Context, RelocationExportRequest) (*WorkspaceArchiveUpload, error)
	ImportWorkspaceArchive(context.Context, WorkspaceArchiveImportRequest, io.Reader) (Receipt, error)
}

func (store *Store) OpenWorkspaceArchiveUpload(
	ctx context.Context,
	request RelocationExportRequest,
) (_ *WorkspaceArchiveUpload, resultErr error) {
	staging := filepath.Join(store.root, "archive-staging")
	if err := os.MkdirAll(staging, privateDirectoryMode); err != nil {
		return nil, fmt.Errorf("SecondBox Workspace archive staging directory: %w", err)
	}
	if err := requirePrivateDirectory(staging); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(staging, ".archive-")
	if err != nil {
		return nil, fmt.Errorf("SecondBox Workspace archive staging file: %w", err)
	}
	upload := &WorkspaceArchiveUpload{file: file}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, upload.Close())
		}
	}()
	inspection, err := store.Inspect(ctx, request.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if inspection.CapacityBytes > math.MaxInt64-inspection.CapacityBytes/8-(1<<20) {
		return nil, ErrStorageIncompatible
	}
	stagingWriter := &workspaceArchiveStagingWriter{
		file: file, maximum: inspection.CapacityBytes + inspection.CapacityBytes/8 + (1 << 20),
	}
	descriptor, err := store.ExportWorkspaceArchive(ctx, request, stagingWriter)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("SecondBox Workspace archive staging rewind: %w", err)
	}
	export, err := store.OpenRelocationExport(ctx, request)
	if err != nil {
		return nil, err
	}
	upload.descriptor, upload.receipt = descriptor, export.Receipt()
	if err := export.Close(); err != nil {
		return nil, err
	}
	return upload, nil
}

type workspaceArchiveStagingWriter struct {
	file         *os.File
	maximum      int64
	written      int64
	unchecked    int64
	spaceChecked bool
}

func (writer *workspaceArchiveStagingWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.maximum-writer.written {
		return 0, errors.New("SecondBox Workspace archive exceeds its staging bound")
	}
	if !writer.spaceChecked || writer.unchecked >= 1<<20 {
		var filesystem unix.Statfs_t
		if err := unix.Fstatfs(int(writer.file.Fd()), &filesystem); err != nil {
			return 0, fmt.Errorf("SecondBox Workspace archive staging free space: %w", err)
		}
		if filesystem.Bsize <= 0 || uint64(filesystem.Blocks) > math.MaxUint64/uint64(filesystem.Bsize) || uint64(filesystem.Bavail) > uint64(filesystem.Blocks) {
			return 0, ErrStorageIncompatible
		}
		total := uint64(filesystem.Blocks) * uint64(filesystem.Bsize)
		available := uint64(filesystem.Bavail) * uint64(filesystem.Bsize)
		// Leave recovery room for other workspaces and the source's unseal receipt.
		if available <= max(total/50, 64<<20)+(1<<20) {
			return 0, fmt.Errorf("SecondBox Workspace archive staging preserves recovery headroom: %w", unix.ENOSPC)
		}
		writer.spaceChecked, writer.unchecked = true, 0
	}
	n, err := writer.file.Write(data)
	writer.written += int64(n)
	writer.unchecked += int64(n)
	return n, err
}

func (store *Store) cleanupWorkspaceArchiveStaging() (resultErr error) {
	root := filepath.Join(store.root, "archive-staging")
	if err := os.MkdirAll(root, privateDirectoryMode); err != nil {
		return fmt.Errorf("SecondBox Workspace archive staging startup: %w", err)
	}
	if err := requirePrivateDirectory(root); err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace archive staging inventory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("SecondBox Workspace archive staging inventory: %w", readErr)
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".archive-") {
				return fmt.Errorf("%w: unexpected Workspace archive staging entry", ErrCorruptState)
			}
			// Staging is never authoritative; its sealed source and command receipt survive restart.
			if err := removeExactFile(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

var _ WorkspaceArchiveTransferStore = (*Store)(nil)
