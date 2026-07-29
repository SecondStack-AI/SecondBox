package workspacestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	privateDirectoryMode = 0o700
	writableImageMode    = 0o600
	snapshotImageMode    = 0o400
	ext4MagicOffset      = 1024 + 0x38
	minimumExt4Bytes     = 16 << 20
)

var logicalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Config contains the one explicit production setting for runner-local
// Workspace durability.
type Config struct {
	Root string
}

type imageCloner interface {
	Clone(destination *os.File, source *os.File) error
}

type imageFormatter interface {
	Format(context.Context, string, string) error
}

type linuxFICLONER struct{}

func (linuxFICLONER) Clone(destination *os.File, source *os.File) error {
	return unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
}

type ext4Formatter struct {
	executable string
}

func (formatter ext4Formatter) Format(ctx context.Context, path string, uuid string) error {
	command := exec.CommandContext(
		ctx,
		formatter.executable,
		"-t", "ext4",
		"-F",
		"-q",
		"-L", workspaceFilesystemLabel,
		"-U", uuid,
		"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard",
		path,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"SecondBox WorkspaceStore ext4 format failed: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return nil
}

// Store is the reflink-only production WorkspaceStore implementation.
type Store struct {
	root      string
	cloner    imageCloner
	formatter imageFormatter
	now       func() time.Time
}

// New validates the absolute root, creates the deterministic layout, and proves
// the mandatory reflink semantics with a real mutation-isolation probe.
func New(ctx context.Context, config Config) (*Store, error) {
	formatterPath, err := exec.LookPath("mke2fs")
	if err != nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore mke2fs is required: %w", err)
	}
	store, err := newStore(config, linuxFICLONER{}, ext4Formatter{executable: formatterPath})
	if err != nil {
		return nil, err
	}
	if err := store.initialize(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func newStore(config Config, cloner imageCloner, formatter imageFormatter) (*Store, error) {
	if !filepath.IsAbs(config.Root) || filepath.Clean(config.Root) != config.Root {
		return nil, fmt.Errorf(
			"SecondBox WorkspaceStore SECONDBOX_RUNNER_WORKSPACE_ROOT must be a clean absolute path",
		)
	}
	if config.Root == string(filepath.Separator) {
		return nil, fmt.Errorf(
			"SecondBox WorkspaceStore SECONDBOX_RUNNER_WORKSPACE_ROOT cannot be the filesystem root",
		)
	}
	if cloner == nil || formatter == nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore filesystem dependencies are required")
	}
	return &Store{
		root:      config.Root,
		cloner:    cloner,
		formatter: formatter,
		now:       time.Now,
	}, nil
}

func (store *Store) initialize(ctx context.Context) error {
	for _, path := range []string{
		store.root,
		store.workspacesRoot(),
		store.snapshotsRoot(),
		store.locksRoot(),
		store.receiptsRoot(),
	} {
		if err := os.MkdirAll(path, privateDirectoryMode); err != nil {
			return fmt.Errorf("SecondBox WorkspaceStore create directory %q: %w", path, err)
		}
		if err := requirePrivateDirectory(path); err != nil {
			return err
		}
	}
	if err := store.probeReflink(ctx); err != nil {
		return err
	}
	return syncDir(store.root)
}

func (store *Store) probeReflink(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := os.CreateTemp(store.workspacesRoot(), ".secondbox-reflink-source-")
	if err != nil {
		return fmt.Errorf("%w: create active-workspace probe: %v", ErrStorageIncompatible, err)
	}
	sourcePath := source.Name()
	defer os.Remove(sourcePath)
	defer source.Close()
	if err := source.Chmod(writableImageMode); err != nil {
		return fmt.Errorf("%w: chmod active-workspace probe: %v", ErrStorageIncompatible, err)
	}
	sourceBytes := []byte("secondbox-reflink-source")
	if _, err := source.Write(sourceBytes); err != nil {
		return fmt.Errorf("%w: write active-workspace probe: %v", ErrStorageIncompatible, err)
	}
	if err := source.Sync(); err != nil {
		return fmt.Errorf("%w: fsync active-workspace probe: %v", ErrStorageIncompatible, err)
	}

	destination, err := os.CreateTemp(store.snapshotsRoot(), ".secondbox-reflink-destination-")
	if err != nil {
		return fmt.Errorf("%w: create Snapshot probe: %v", ErrStorageIncompatible, err)
	}
	destinationPath := destination.Name()
	defer os.Remove(destinationPath)
	defer destination.Close()
	if err := destination.Chmod(writableImageMode); err != nil {
		return fmt.Errorf("%w: chmod Snapshot probe: %v", ErrStorageIncompatible, err)
	}
	var sourceStat, destinationStat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &sourceStat); err != nil {
		return fmt.Errorf("%w: stat active-workspace probe: %v", ErrStorageIncompatible, err)
	}
	if err := unix.Fstat(int(destination.Fd()), &destinationStat); err != nil {
		return fmt.Errorf("%w: stat Snapshot probe: %v", ErrStorageIncompatible, err)
	}
	if sourceStat.Dev != destinationStat.Dev {
		return fmt.Errorf(
			"%w: active Workspace and Snapshot roots are not on the same filesystem",
			ErrStorageIncompatible,
		)
	}
	if err := store.cloner.Clone(destination, source); err != nil {
		return fmt.Errorf("%w: FICLONE probe failed: %v", ErrStorageIncompatible, err)
	}
	if _, err := destination.WriteAt([]byte("D"), 0); err != nil {
		return fmt.Errorf("%w: mutate Snapshot probe: %v", ErrStorageIncompatible, err)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("%w: fsync Snapshot probe: %v", ErrStorageIncompatible, err)
	}
	actual := make([]byte, len(sourceBytes))
	if _, err := source.ReadAt(actual, 0); err != nil {
		return fmt.Errorf("%w: read active-workspace probe: %v", ErrStorageIncompatible, err)
	}
	if string(actual) != string(sourceBytes) {
		return fmt.Errorf("%w: FICLONE mutation isolation failed", ErrStorageIncompatible)
	}
	return nil
}

type currentManifest struct {
	FormatVersion int    `json:"formatVersion"`
	WorkspaceID   string `json:"workspaceId"`
	Generation    uint64 `json:"generation"`
	Image         string `json:"image"`
	CapacityBytes int64  `json:"capacityBytes"`
}

type snapshotManifest struct {
	FormatVersion int    `json:"formatVersion"`
	SnapshotID    string `json:"snapshotId"`
	WorkspaceID   string `json:"workspaceId"`
	Generation    uint64 `json:"generation"`
	CapacityBytes int64  `json:"capacityBytes"`
}

type stagedRestore struct {
	FormatVersion      int    `json:"formatVersion"`
	OperationID        string `json:"operationId"`
	WorkspaceID        string `json:"workspaceId"`
	SnapshotID         string `json:"snapshotId"`
	ExpectedGeneration uint64 `json:"expectedGeneration"`
	NextGeneration     uint64 `json:"nextGeneration"`
	Image              string `json:"image"`
	CapacityBytes      int64  `json:"capacityBytes"`
}

type rollbackManifest struct {
	FormatVersion      int    `json:"formatVersion"`
	OperationID        string `json:"operationId"`
	WorkspaceID        string `json:"workspaceId"`
	SnapshotID         string `json:"snapshotId"`
	PreviousGeneration uint64 `json:"previousGeneration"`
	PreviousImage      string `json:"previousImage"`
	NextGeneration     uint64 `json:"nextGeneration"`
	NextImage          string `json:"nextImage"`
	CapacityBytes      int64  `json:"capacityBytes"`
}

// Create creates and formats one sparse raw ext4 image, then atomically
// publishes generation one and its durable operation receipt.
func (store *Store) Create(
	ctx context.Context,
	request CreateWorkspaceRequest,
) (Receipt, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	if request.CapacityBytes < minimumExt4Bytes {
		return Receipt{}, fmt.Errorf(
			"SecondBox WorkspaceStore logical capacity must be at least %d bytes",
			minimumExt4Bytes,
		)
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptWorkspaceCreate); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptWorkspaceCreate); found || err != nil {
		return receipt, err
	}

	if err := store.ensureWorkspaceLayout(request.WorkspaceID); err != nil {
		return Receipt{}, err
	}
	manifest, manifestErr := store.readCurrentManifest(request.WorkspaceID)
	if manifestErr == nil {
		if manifest.Generation != 1 || manifest.CapacityBytes != request.CapacityBytes {
			return Receipt{}, ErrConflictingReplay
		}
		if err := store.validateImage(request.WorkspaceID, manifest.Image, request.CapacityBytes); err != nil {
			return Receipt{}, err
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptWorkspaceCreate, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, InputDigest: digest,
			Generation: 1, CapacityBytes: request.CapacityBytes,
		})
	}
	if !errors.Is(manifestErr, ErrWorkspaceNotFound) {
		return Receipt{}, manifestErr
	}

	imageName := generationImageName(1, "create")
	imagePath := store.versionPath(request.WorkspaceID, imageName)
	if err := store.createFormattedImage(
		ctx,
		request.WorkspaceID,
		request.OperationID,
		imagePath,
		request.CapacityBytes,
	); err != nil {
		return Receipt{}, err
	}
	manifest = currentManifest{
		FormatVersion: currentManifestFormatVersion,
		WorkspaceID:   request.WorkspaceID, Generation: 1,
		Image: imageName, CapacityBytes: request.CapacityBytes,
	}
	if err := store.publishCurrentManifest(request.WorkspaceID, manifest); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptWorkspaceCreate, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, InputDigest: digest,
		Generation: 1, CapacityBytes: request.CapacityBytes,
	})
}

// Open resolves the current manifest and holds the cross-process exclusive
// writer lock until Attachment.Close.
func (store *Store) Open(
	ctx context.Context,
	workspaceID string,
	expectedGeneration uint64,
) (ComputeAttachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateID(workspaceID); err != nil {
		return nil, err
	}
	lock, err := store.lockWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	manifest, err := store.readCurrentManifest(workspaceID)
	if err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	if manifest.Generation != expectedGeneration {
		closeLockedFile(lock)
		return nil, ErrStaleGeneration
	}
	if pending, err := store.stagedRestorePending(workspaceID); err != nil {
		closeLockedFile(lock)
		return nil, err
	} else if pending {
		closeLockedFile(lock)
		return nil, ErrRestorePending
	}
	imagePath, err := store.validatedVersionPath(workspaceID, manifest.Image)
	if err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	if err := validateExt4Image(imagePath, manifest.CapacityBytes); err != nil {
		closeLockedFile(lock)
		return nil, err
	}
	image, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		closeLockedFile(lock)
		return nil, fmt.Errorf("SecondBox WorkspaceStore open current image: %w", err)
	}
	return &Attachment{
		handle: WorkspaceHandle{
			workspaceID: workspaceID,
			image:       manifest.Image, generation: manifest.Generation,
			nonce: requestNonce(workspaceID, manifest.Image, strconv.FormatUint(manifest.Generation, 10)),
		},
		file: image,
		lock: lock,
	}, nil
}

// AdvanceGeneration atomically republishes the same current image under the
// next generation after compute has detached.
func (store *Store) AdvanceGeneration(
	ctx context.Context,
	request AdvanceGenerationRequest,
) (Receipt, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	if request.NextGeneration != request.ExpectedGeneration+1 {
		return Receipt{}, ErrStaleGeneration
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptGenerationAdvance); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	if manifest.Generation == request.NextGeneration {
		return store.recordReceipt(Receipt{
			Kind: ReceiptGenerationAdvance, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, InputDigest: digest,
			PreviousGeneration: request.ExpectedGeneration,
			Generation:         request.NextGeneration, CapacityBytes: manifest.CapacityBytes,
		})
	}
	if manifest.Generation != request.ExpectedGeneration {
		return Receipt{}, ErrStaleGeneration
	}
	if pending, err := store.stagedRestorePending(request.WorkspaceID); err != nil {
		return Receipt{}, err
	} else if pending {
		return Receipt{}, ErrRestorePending
	}
	manifest.Generation = request.NextGeneration
	if err := store.publishCurrentManifest(request.WorkspaceID, manifest); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptGenerationAdvance, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, InputDigest: digest,
		PreviousGeneration: request.ExpectedGeneration,
		Generation:         request.NextGeneration, CapacityBytes: manifest.CapacityBytes,
	})
}

// CreateSnapshot creates an immutable-by-policy FICLONE of the stopped current
// image. It has no byte-copy or digest fallback.
func (store *Store) CreateSnapshot(
	ctx context.Context,
	request CreateSnapshotRequest,
) (Receipt, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	if err := validateID(request.SnapshotID); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptSnapshotCreate); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	if manifest.Generation != request.ExpectedGeneration {
		return Receipt{}, ErrStaleGeneration
	}
	if pending, err := store.stagedRestorePending(request.WorkspaceID); err != nil {
		return Receipt{}, err
	} else if pending {
		return Receipt{}, ErrRestorePending
	}
	if existing, err := store.readSnapshotManifest(request.SnapshotID); err == nil {
		if existing.WorkspaceID != request.WorkspaceID ||
			existing.Generation != request.ExpectedGeneration ||
			existing.CapacityBytes != manifest.CapacityBytes {
			return Receipt{}, ErrConflictingReplay
		}
		if err := validateExt4Image(store.snapshotImagePath(request.SnapshotID), manifest.CapacityBytes); err != nil {
			return Receipt{}, err
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptSnapshotCreate, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
			InputDigest: digest, Generation: manifest.Generation,
			CapacityBytes: manifest.CapacityBytes,
		})
	} else if !errors.Is(err, ErrSnapshotNotFound) {
		return Receipt{}, err
	}

	if err := os.Mkdir(store.snapshotDir(request.SnapshotID), privateDirectoryMode); err != nil &&
		!errors.Is(err, os.ErrExist) {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore create Snapshot directory: %w", err)
	}
	sourcePath, err := store.validatedVersionPath(request.WorkspaceID, manifest.Image)
	if err != nil {
		return Receipt{}, err
	}
	if err := store.cloneImage(
		sourcePath,
		store.snapshotImagePath(request.SnapshotID),
		snapshotImageMode,
		manifest.CapacityBytes,
		request.OperationID,
	); err != nil {
		return Receipt{}, err
	}
	snapshot := snapshotManifest{
		FormatVersion: currentManifestFormatVersion,
		SnapshotID:    request.SnapshotID, WorkspaceID: request.WorkspaceID,
		Generation: manifest.Generation, CapacityBytes: manifest.CapacityBytes,
	}
	if err := atomicJSON(store.snapshotManifestPath(request.SnapshotID), snapshot); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore publish Snapshot manifest: %w", err)
	}
	if err := syncDir(store.snapshotDir(request.SnapshotID)); err != nil {
		return Receipt{}, err
	}
	if err := syncDir(store.snapshotsRoot()); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptSnapshotCreate, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
		InputDigest: digest, Generation: manifest.Generation,
		CapacityBytes: manifest.CapacityBytes,
	})
}

// DeleteSnapshot idempotently removes one immutable Snapshot after proving no
// staged restore refers to it.
func (store *Store) DeleteSnapshot(
	ctx context.Context,
	request DeleteSnapshotRequest,
) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	if err := validateID(request.SnapshotID); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptSnapshotDelete); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	snapshot, err := store.readSnapshotManifest(request.SnapshotID)
	if errors.Is(err, ErrSnapshotNotFound) {
		return store.recordReceipt(Receipt{
			Kind: ReceiptSnapshotDelete, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
			InputDigest: digest,
		})
	}
	if err != nil {
		return Receipt{}, err
	}
	if snapshot.WorkspaceID != request.WorkspaceID {
		return Receipt{}, ErrConflictingReplay
	}
	inUse, err := store.snapshotReferencedByRestore(request.WorkspaceID, request.SnapshotID)
	if err != nil {
		return Receipt{}, err
	}
	if inUse {
		return Receipt{}, ErrSnapshotInUse
	}
	if err := os.Chmod(store.snapshotImagePath(request.SnapshotID), writableImageMode); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore make Snapshot deletable: %w", err)
	}
	if err := removeExactFile(store.snapshotImagePath(request.SnapshotID)); err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.snapshotManifestPath(request.SnapshotID)); err != nil {
		return Receipt{}, err
	}
	if err := os.Remove(store.snapshotDir(request.SnapshotID)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore remove Snapshot directory: %w", err)
	}
	if err := syncDir(store.snapshotsRoot()); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptSnapshotDelete, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
		InputDigest: digest, Generation: snapshot.Generation,
		CapacityBytes: snapshot.CapacityBytes,
	})
}

// PrepareRestore durably publishes a writable staged reflink child while
// leaving the current manifest untouched.
func (store *Store) PrepareRestore(
	ctx context.Context,
	request PrepareRestoreRequest,
) (Receipt, error) {
	if err := validateRestoreRequest(
		request.Mutation,
		request.SnapshotID,
		request.ExpectedGeneration,
		request.NextGeneration,
	); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptRestorePrepare); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	if manifest.Generation != request.ExpectedGeneration {
		return Receipt{}, ErrStaleGeneration
	}
	snapshot, err := store.readSnapshotManifest(request.SnapshotID)
	if err != nil {
		return Receipt{}, err
	}
	if snapshot.WorkspaceID != request.WorkspaceID ||
		snapshot.CapacityBytes != manifest.CapacityBytes {
		return Receipt{}, ErrConflictingReplay
	}
	if err := validateExt4Image(
		store.snapshotImagePath(request.SnapshotID),
		snapshot.CapacityBytes,
	); errors.Is(err, os.ErrNotExist) {
		return Receipt{}, ErrSnapshotNotFound
	} else if err != nil {
		return Receipt{}, err
	}
	if pending, err := store.pendingRestoreOperation(request.WorkspaceID); err != nil {
		return Receipt{}, err
	} else if pending != "" && pending != request.OperationID {
		return Receipt{}, ErrRestorePending
	}
	if staged, err := store.readStagedRestore(request.WorkspaceID, request.OperationID); err == nil {
		if !stagedMatches(staged, request) {
			return Receipt{}, ErrConflictingReplay
		}
		if err := validateExt4Image(
			store.stagedImagePath(request.WorkspaceID, request.OperationID),
			manifest.CapacityBytes,
		); err != nil {
			return Receipt{}, err
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptRestorePrepare, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
			InputDigest: digest, PreviousGeneration: request.ExpectedGeneration,
			Generation: request.NextGeneration, CapacityBytes: manifest.CapacityBytes,
		})
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, err
	}
	if err := store.cloneImage(
		store.snapshotImagePath(request.SnapshotID),
		store.stagedImagePath(request.WorkspaceID, request.OperationID),
		writableImageMode,
		manifest.CapacityBytes,
		request.OperationID,
	); err != nil {
		return Receipt{}, err
	}
	staged := stagedRestore{
		FormatVersion: currentManifestFormatVersion,
		OperationID:   request.OperationID, WorkspaceID: request.WorkspaceID,
		SnapshotID: request.SnapshotID, ExpectedGeneration: request.ExpectedGeneration,
		NextGeneration: request.NextGeneration,
		Image:          generationImageName(request.NextGeneration, "restore-"+request.OperationID),
		CapacityBytes:  manifest.CapacityBytes,
	}
	if err := atomicJSON(store.stagedManifestPath(request.WorkspaceID, request.OperationID), staged); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore publish staged restore: %w", err)
	}
	if err := syncDir(store.stagedDir(request.WorkspaceID)); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptRestorePrepare, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
		InputDigest: digest, PreviousGeneration: request.ExpectedGeneration,
		Generation: request.NextGeneration, CapacityBytes: manifest.CapacityBytes,
	})
}

// SwapRestore preserves operation-scoped rollback state, moves the staged image
// into versioned storage, and atomically selects it in the current manifest.
func (store *Store) SwapRestore(
	ctx context.Context,
	request SwapRestoreRequest,
) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := validateRestoreRequest(
		request.Mutation,
		request.SnapshotID,
		request.ExpectedGeneration,
		request.NextGeneration,
	); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptRestoreSwap); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	staged, err := store.readStagedRestore(request.WorkspaceID, request.OperationID)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: staged restore: %v", ErrCorruptState, err)
	}
	swapRequest := PrepareRestoreRequest{
		Mutation: request.Mutation, SnapshotID: request.SnapshotID,
		ExpectedGeneration: request.ExpectedGeneration,
		NextGeneration:     request.NextGeneration,
	}
	if !stagedMatches(staged, swapRequest) {
		return Receipt{}, ErrConflictingReplay
	}
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	if manifest.Generation == request.NextGeneration && manifest.Image == staged.Image {
		rollback, rollbackErr := store.readRollback(request.WorkspaceID, request.OperationID)
		if rollbackErr != nil {
			return Receipt{}, fmt.Errorf("%w: swapped restore lacks rollback evidence", ErrCorruptState)
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptRestoreSwap, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
			InputDigest: digest, PreviousGeneration: rollback.PreviousGeneration,
			Generation: rollback.NextGeneration, CapacityBytes: rollback.CapacityBytes,
		})
	}
	if manifest.Generation != request.ExpectedGeneration {
		return Receipt{}, ErrStaleGeneration
	}
	targetPath := store.versionPath(request.WorkspaceID, staged.Image)
	if _, err := os.Stat(targetPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(
			store.stagedImagePath(request.WorkspaceID, request.OperationID),
			targetPath,
		); err != nil {
			return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore publish restore version: %w", err)
		}
		if err := syncDir(store.stagedDir(request.WorkspaceID)); err != nil {
			return Receipt{}, err
		}
		if err := syncDir(store.versionsDir(request.WorkspaceID)); err != nil {
			return Receipt{}, err
		}
	} else if err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore inspect restore version: %w", err)
	}
	if err := validateExt4Image(targetPath, manifest.CapacityBytes); err != nil {
		return Receipt{}, err
	}
	rollback := rollbackManifest{
		FormatVersion: currentManifestFormatVersion,
		OperationID:   request.OperationID, WorkspaceID: request.WorkspaceID,
		SnapshotID:         request.SnapshotID,
		PreviousGeneration: manifest.Generation, PreviousImage: manifest.Image,
		NextGeneration: request.NextGeneration, NextImage: staged.Image,
		CapacityBytes: manifest.CapacityBytes,
	}
	if err := atomicJSON(store.rollbackPath(request.WorkspaceID, request.OperationID), rollback); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore publish rollback state: %w", err)
	}
	if err := syncDir(store.rollbackDir(request.WorkspaceID)); err != nil {
		return Receipt{}, err
	}
	if err := store.publishCurrentManifest(request.WorkspaceID, currentManifest{
		FormatVersion: currentManifestFormatVersion,
		WorkspaceID:   request.WorkspaceID, Generation: request.NextGeneration,
		Image: staged.Image, CapacityBytes: manifest.CapacityBytes,
	}); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptRestoreSwap, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: request.SnapshotID,
		InputDigest: digest, PreviousGeneration: request.ExpectedGeneration,
		Generation: request.NextGeneration, CapacityBytes: manifest.CapacityBytes,
	})
}

// FinalizeRestore removes the previous image and operation staging evidence
// only after the current manifest proves the new generation remains selected.
func (store *Store) FinalizeRestore(
	ctx context.Context,
	request RestoreMutation,
) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptRestoreFinalize); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	rollback, err := store.readRollback(request.WorkspaceID, request.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		swapReceipt, found, receiptErr := store.loadAnyReceipt(
			request.Mutation,
			ReceiptRestoreSwap,
		)
		if receiptErr != nil {
			return Receipt{}, receiptErr
		}
		if !found {
			return Receipt{}, fmt.Errorf("%w: restore swap receipt is absent", ErrCorruptState)
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptRestoreFinalize, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, SnapshotID: swapReceipt.SnapshotID,
			InputDigest: digest, PreviousGeneration: swapReceipt.PreviousGeneration,
			Generation: swapReceipt.Generation, CapacityBytes: swapReceipt.CapacityBytes,
		})
	}
	if err != nil {
		return Receipt{}, err
	}
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	if manifest.Generation != rollback.NextGeneration || manifest.Image != rollback.NextImage {
		return Receipt{}, ErrStaleGeneration
	}
	previousPath, err := store.validatedVersionPath(request.WorkspaceID, rollback.PreviousImage)
	if err != nil {
		return Receipt{}, err
	}
	if previousPath == store.versionPath(request.WorkspaceID, manifest.Image) {
		return Receipt{}, fmt.Errorf("%w: rollback image is current", ErrCorruptState)
	}
	if err := removeExactFile(previousPath); err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.stagedImagePath(request.WorkspaceID, request.OperationID)); err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.stagedManifestPath(request.WorkspaceID, request.OperationID)); err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.rollbackPath(request.WorkspaceID, request.OperationID)); err != nil {
		return Receipt{}, err
	}
	for _, directory := range []string{
		store.versionsDir(request.WorkspaceID),
		store.stagedDir(request.WorkspaceID),
		store.rollbackDir(request.WorkspaceID),
	} {
		if err := syncDir(directory); err != nil {
			return Receipt{}, err
		}
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptRestoreFinalize, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: rollback.SnapshotID,
		InputDigest: digest, PreviousGeneration: rollback.PreviousGeneration,
		Generation: rollback.NextGeneration, CapacityBytes: rollback.CapacityBytes,
	})
}

// AbortRestore removes only pre-swap staged state. A swapped restore must remain
// intact for control-plane commit reconciliation.
func (store *Store) AbortRestore(
	ctx context.Context,
	request RestoreMutation,
) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptRestoreAbort); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	if _, err := store.readRollback(request.WorkspaceID, request.OperationID); err == nil {
		return Receipt{}, ErrRestorePending
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, err
	}
	staged, err := store.readStagedRestore(request.WorkspaceID, request.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		return store.recordReceipt(Receipt{
			Kind: ReceiptRestoreAbort, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, InputDigest: digest,
		})
	}
	if err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.stagedImagePath(request.WorkspaceID, request.OperationID)); err != nil {
		return Receipt{}, err
	}
	if err := removeExactFile(store.stagedManifestPath(request.WorkspaceID, request.OperationID)); err != nil {
		return Receipt{}, err
	}
	if err := syncDir(store.stagedDir(request.WorkspaceID)); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptRestoreAbort, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, SnapshotID: staged.SnapshotID,
		InputDigest: digest, PreviousGeneration: staged.ExpectedGeneration,
		Generation: staged.NextGeneration, CapacityBytes: staged.CapacityBytes,
	})
}

// DeleteWorkspace removes only validated, known local Workspace paths. It also
// removes that Workspace's local Snapshots, but refuses staged/unfinalized
// restore state.
func (store *Store) DeleteWorkspace(
	ctx context.Context,
	request DeleteWorkspaceRequest,
) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Receipt{}, err
	}
	digest, err := inputDigest(request)
	if err != nil {
		return Receipt{}, err
	}
	if receipt, found, err := store.loadReceipt(request.Mutation, digest, ReceiptWorkspaceDelete); found || err != nil {
		return receipt, err
	}
	lock, err := store.lockWorkspace(request.WorkspaceID)
	if err != nil {
		return Receipt{}, err
	}
	defer closeLockedFile(lock)
	manifest, err := store.readCurrentManifest(request.WorkspaceID)
	if errors.Is(err, ErrWorkspaceNotFound) {
		if _, statErr := os.Lstat(store.workspaceDir(request.WorkspaceID)); statErr == nil {
			if err := store.deleteWorkspaceSnapshots(request.WorkspaceID); err != nil {
				return Receipt{}, err
			}
			if err := store.removeWorkspaceTree(request.WorkspaceID); err != nil {
				return Receipt{}, err
			}
			if err := syncDir(store.workspacesRoot()); err != nil {
				return Receipt{}, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Receipt{}, fmt.Errorf(
				"SecondBox WorkspaceStore inspect partially deleted Workspace: %w",
				statErr,
			)
		}
		if err := store.deleteWorkspaceReceipts(request.WorkspaceID); err != nil {
			return Receipt{}, err
		}
		return store.recordReceipt(Receipt{
			Kind: ReceiptWorkspaceDelete, OperationID: request.OperationID,
			WorkspaceID: request.WorkspaceID, InputDigest: digest,
			Generation: request.ExpectedGeneration,
		})
	}
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
	if err := store.deleteWorkspaceSnapshots(request.WorkspaceID); err != nil {
		return Receipt{}, err
	}
	if err := store.removeWorkspaceTree(request.WorkspaceID); err != nil {
		return Receipt{}, err
	}
	if err := syncDir(store.workspacesRoot()); err != nil {
		return Receipt{}, err
	}
	if err := store.deleteWorkspaceReceipts(request.WorkspaceID); err != nil {
		return Receipt{}, err
	}
	return store.recordReceipt(Receipt{
		Kind: ReceiptWorkspaceDelete, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, InputDigest: digest,
		Generation: manifest.Generation, CapacityBytes: manifest.CapacityBytes,
	})
}

// Inspect validates current logical Workspace evidence without acquiring a
// compute attachment.
func (store *Store) Inspect(
	ctx context.Context,
	workspaceID string,
) (WorkspaceInspection, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceInspection{}, err
	}
	if err := validateID(workspaceID); err != nil {
		return WorkspaceInspection{}, err
	}
	lock, err := store.lockWorkspace(workspaceID)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	defer closeLockedFile(lock)
	return store.inspectLocked(workspaceID)
}

func (store *Store) inspectLocked(workspaceID string) (WorkspaceInspection, error) {
	manifest, err := store.readCurrentManifest(workspaceID)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	imagePath, err := store.validatedVersionPath(workspaceID, manifest.Image)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	formatted := validateExt4Image(imagePath, manifest.CapacityBytes) == nil
	pending, err := store.restorePending(workspaceID)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	return WorkspaceInspection{
		WorkspaceID: workspaceID, Generation: manifest.Generation,
		CapacityBytes: manifest.CapacityBytes, Formatted: formatted,
		RestorePending: pending,
	}, nil
}

// Reconcile returns sorted manifest and receipt evidence after validating every
// known logical path. It does not infer successful mutations from arbitrary
// file presence.
func (store *Store) Reconcile(ctx context.Context) (ReconcileReport, error) {
	workspaceEntries, err := os.ReadDir(store.workspacesRoot())
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("SecondBox WorkspaceStore list Workspaces: %w", err)
	}
	report := ReconcileReport{}
	for _, entry := range workspaceEntries {
		if err := ctx.Err(); err != nil {
			return ReconcileReport{}, err
		}
		if !entry.IsDir() {
			return ReconcileReport{}, fmt.Errorf("%w: unexpected Workspace root entry", ErrCorruptState)
		}
		if err := validateID(entry.Name()); err != nil {
			return ReconcileReport{}, fmt.Errorf("%w: invalid Workspace directory", ErrCorruptState)
		}
		lock, err := store.lockWorkspace(entry.Name())
		activeWriter := errors.Is(err, ErrActiveWriter)
		if err != nil && !activeWriter {
			return ReconcileReport{}, err
		}
		inspection, inspectErr := store.inspectLocked(entry.Name())
		inspection.ActiveWriter = activeWriter
		closeErr := closeLockedFile(lock)
		if inspectErr != nil {
			return ReconcileReport{}, inspectErr
		}
		if closeErr != nil {
			return ReconcileReport{}, closeErr
		}
		report.Workspaces = append(report.Workspaces, inspection)
	}
	receiptWorkspaceEntries, err := os.ReadDir(store.receiptsRoot())
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("SecondBox WorkspaceStore list receipts: %w", err)
	}
	for _, workspaceEntry := range receiptWorkspaceEntries {
		if !workspaceEntry.IsDir() || validateID(workspaceEntry.Name()) != nil {
			return ReconcileReport{}, fmt.Errorf("%w: invalid receipt directory", ErrCorruptState)
		}
		operationEntries, err := os.ReadDir(filepath.Join(store.receiptsRoot(), workspaceEntry.Name()))
		if err != nil {
			return ReconcileReport{}, err
		}
		for _, operationEntry := range operationEntries {
			if !operationEntry.IsDir() || validateID(operationEntry.Name()) != nil {
				return ReconcileReport{}, fmt.Errorf("%w: invalid receipt operation directory", ErrCorruptState)
			}
			entries, err := os.ReadDir(filepath.Join(
				store.receiptsRoot(),
				workspaceEntry.Name(),
				operationEntry.Name(),
			))
			if err != nil {
				return ReconcileReport{}, err
			}
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					return ReconcileReport{}, fmt.Errorf("%w: invalid receipt entry", ErrCorruptState)
				}
				var receipt Receipt
				if err := readJSON(
					filepath.Join(
						store.receiptsRoot(),
						workspaceEntry.Name(),
						operationEntry.Name(),
						entry.Name(),
					),
					&receipt,
				); err != nil {
					return ReconcileReport{}, err
				}
				if err := validateReceipt(receipt); err != nil {
					return ReconcileReport{}, err
				}
				if receipt.WorkspaceID != workspaceEntry.Name() ||
					receipt.OperationID != operationEntry.Name() ||
					strings.TrimSuffix(entry.Name(), ".json") != receipt.Kind {
					return ReconcileReport{}, fmt.Errorf(
						"%w: receipt path disagrees with content",
						ErrCorruptState,
					)
				}
				report.Receipts = append(report.Receipts, receipt)
			}
		}
	}
	sort.Slice(report.Workspaces, func(i int, j int) bool {
		return report.Workspaces[i].WorkspaceID < report.Workspaces[j].WorkspaceID
	})
	sort.Slice(report.Receipts, func(i int, j int) bool {
		if report.Receipts[i].WorkspaceID == report.Receipts[j].WorkspaceID {
			return report.Receipts[i].OperationID < report.Receipts[j].OperationID
		}
		return report.Receipts[i].WorkspaceID < report.Receipts[j].WorkspaceID
	})
	return report, nil
}

func (store *Store) createFormattedImage(
	ctx context.Context,
	workspaceID string,
	operationID string,
	finalPath string,
	capacityBytes int64,
) error {
	if info, err := os.Stat(finalPath); err == nil {
		if info.Size() != capacityBytes {
			return ErrConflictingReplay
		}
		return validateExt4Image(finalPath, capacityBytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox WorkspaceStore inspect version image: %w", err)
	}
	tempPath := filepath.Join(store.versionsDir(workspaceID), "."+operationID+".formatting")
	if err := removeExactFile(tempPath); err != nil {
		return err
	}
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, writableImageMode)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore create sparse image: %w", err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			file.Close()
		}
	}()
	if err := file.Truncate(capacityBytes); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore size sparse image: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore close sparse image before format: %w", err)
	}
	closeFile = false
	if err := store.formatter.Format(ctx, tempPath, deterministicUUID(workspaceID)); err != nil {
		return err
	}
	file, err = os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore reopen formatted image: %w", err)
	}
	closeFile = true
	if err := file.Sync(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync formatted image: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore close formatted image: %w", err)
	}
	closeFile = false
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore publish formatted image: %w", err)
	}
	if err := syncDir(store.versionsDir(workspaceID)); err != nil {
		return err
	}
	return validateExt4Image(finalPath, capacityBytes)
}

func (store *Store) cloneImage(
	sourcePath string,
	finalPath string,
	mode os.FileMode,
	capacityBytes int64,
	operationID string,
) error {
	if info, err := os.Stat(finalPath); err == nil {
		if info.Size() != capacityBytes {
			return ErrConflictingReplay
		}
		return validateExt4Image(finalPath, capacityBytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox WorkspaceStore inspect reflink target: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open reflink source: %w", err)
	}
	defer source.Close()
	tempPath := filepath.Join(filepath.Dir(finalPath), "."+operationID+".reflinking")
	if err := removeExactFile(tempPath); err != nil {
		return err
	}
	destination, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, writableImageMode)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore create reflink target: %w", err)
	}
	removeTemp := true
	defer func() {
		destination.Close()
		if removeTemp {
			os.Remove(tempPath)
		}
	}()
	if err := store.cloner.Clone(destination, source); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore FICLONE failed: %w", err)
	}
	if err := destination.Chmod(mode); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore set reflink mode: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync reflink: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore close reflink: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore publish reflink: %w", err)
	}
	removeTemp = false
	if err := syncDir(filepath.Dir(finalPath)); err != nil {
		return err
	}
	return validateExt4Image(finalPath, capacityBytes)
}

func (store *Store) ensureWorkspaceLayout(workspaceID string) error {
	for _, path := range []string{
		store.workspaceDir(workspaceID),
		store.versionsDir(workspaceID),
		store.manifestDir(workspaceID),
		store.stagedDir(workspaceID),
		store.rollbackDir(workspaceID),
	} {
		if err := os.MkdirAll(path, privateDirectoryMode); err != nil {
			return fmt.Errorf("SecondBox WorkspaceStore create Workspace layout: %w", err)
		}
		if err := requirePrivateDirectory(path); err != nil {
			return err
		}
	}
	return syncDir(store.workspaceDir(workspaceID))
}

func (store *Store) publishCurrentManifest(workspaceID string, manifest currentManifest) error {
	if manifest.FormatVersion != currentManifestFormatVersion ||
		manifest.WorkspaceID != workspaceID ||
		manifest.Generation < 1 ||
		manifest.CapacityBytes < minimumExt4Bytes {
		return fmt.Errorf("%w: invalid current manifest", ErrCorruptState)
	}
	if _, err := store.validatedVersionPath(workspaceID, manifest.Image); err != nil {
		return err
	}
	if err := atomicJSON(store.currentManifestPath(workspaceID), manifest); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore publish current manifest: %w", err)
	}
	return syncDir(store.manifestDir(workspaceID))
}

func (store *Store) readCurrentManifest(workspaceID string) (currentManifest, error) {
	var manifest currentManifest
	err := readJSON(store.currentManifestPath(workspaceID), &manifest)
	if errors.Is(err, os.ErrNotExist) {
		return currentManifest{}, ErrWorkspaceNotFound
	}
	if err != nil {
		return currentManifest{}, fmt.Errorf("%w: current manifest: %v", ErrCorruptState, err)
	}
	if manifest.FormatVersion != currentManifestFormatVersion ||
		manifest.WorkspaceID != workspaceID ||
		manifest.Generation < 1 ||
		manifest.CapacityBytes < minimumExt4Bytes {
		return currentManifest{}, fmt.Errorf("%w: invalid current manifest", ErrCorruptState)
	}
	if _, err := store.validatedVersionPath(workspaceID, manifest.Image); err != nil {
		return currentManifest{}, err
	}
	return manifest, nil
}

func (store *Store) readSnapshotManifest(snapshotID string) (snapshotManifest, error) {
	var manifest snapshotManifest
	err := readJSON(store.snapshotManifestPath(snapshotID), &manifest)
	if errors.Is(err, os.ErrNotExist) {
		return snapshotManifest{}, ErrSnapshotNotFound
	}
	if err != nil {
		return snapshotManifest{}, fmt.Errorf("%w: Snapshot manifest: %v", ErrCorruptState, err)
	}
	if manifest.FormatVersion != currentManifestFormatVersion ||
		manifest.SnapshotID != snapshotID ||
		validateID(manifest.WorkspaceID) != nil ||
		manifest.Generation < 1 ||
		manifest.CapacityBytes < minimumExt4Bytes {
		return snapshotManifest{}, fmt.Errorf("%w: invalid Snapshot manifest", ErrCorruptState)
	}
	return manifest, nil
}

func (store *Store) readStagedRestore(workspaceID string, operationID string) (stagedRestore, error) {
	var staged stagedRestore
	if err := readJSON(store.stagedManifestPath(workspaceID, operationID), &staged); err != nil {
		return stagedRestore{}, err
	}
	if staged.FormatVersion != currentManifestFormatVersion ||
		staged.WorkspaceID != workspaceID ||
		staged.OperationID != operationID ||
		validateID(staged.SnapshotID) != nil ||
		staged.NextGeneration != staged.ExpectedGeneration+1 ||
		staged.CapacityBytes < minimumExt4Bytes {
		return stagedRestore{}, fmt.Errorf("%w: invalid staged restore", ErrCorruptState)
	}
	if _, err := store.validatedVersionPath(workspaceID, staged.Image); err != nil {
		return stagedRestore{}, err
	}
	return staged, nil
}

func (store *Store) readRollback(workspaceID string, operationID string) (rollbackManifest, error) {
	var rollback rollbackManifest
	if err := readJSON(store.rollbackPath(workspaceID, operationID), &rollback); err != nil {
		return rollbackManifest{}, err
	}
	if rollback.FormatVersion != currentManifestFormatVersion ||
		rollback.WorkspaceID != workspaceID ||
		rollback.OperationID != operationID ||
		rollback.NextGeneration != rollback.PreviousGeneration+1 ||
		rollback.CapacityBytes < minimumExt4Bytes {
		return rollbackManifest{}, fmt.Errorf("%w: invalid rollback manifest", ErrCorruptState)
	}
	if _, err := store.validatedVersionPath(workspaceID, rollback.PreviousImage); err != nil {
		return rollbackManifest{}, err
	}
	if _, err := store.validatedVersionPath(workspaceID, rollback.NextImage); err != nil {
		return rollbackManifest{}, err
	}
	return rollback, nil
}

func (store *Store) recordReceipt(receipt Receipt) (Receipt, error) {
	receipt.FormatVersion = receiptFormatVersion
	receipt.RecordedAt = store.now().UTC()
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, err
	}
	mutation := Mutation{OperationID: receipt.OperationID, WorkspaceID: receipt.WorkspaceID}
	path := store.receiptPath(mutation, receipt.Kind)
	if existing, found, err := store.loadReceipt(
		mutation,
		receipt.InputDigest,
		receipt.Kind,
	); found || err != nil {
		return existing, err
	}
	if err := os.MkdirAll(filepath.Dir(path), privateDirectoryMode); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore create receipt directory: %w", err)
	}
	if err := atomicJSON(path, receipt); err != nil {
		return Receipt{}, fmt.Errorf("SecondBox WorkspaceStore persist receipt: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return Receipt{}, err
	}
	if err := syncDir(filepath.Dir(filepath.Dir(path))); err != nil {
		return Receipt{}, err
	}
	if err := syncDir(store.receiptsRoot()); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (store *Store) loadReceipt(
	mutation Mutation,
	digest string,
	kind string,
) (Receipt, bool, error) {
	receipt, found, err := store.loadAnyReceipt(mutation, kind)
	if err != nil || !found {
		return Receipt{}, found, err
	}
	storedFence, _, storedDigestValid := strings.Cut(receipt.InputDigest, ":")
	replayedFence, _, replayedDigestValid := strings.Cut(digest, ":")
	if !storedDigestValid || !replayedDigestValid {
		return Receipt{}, true, fmt.Errorf("%w: invalid operation receipt digest", ErrCorruptState)
	}
	if storedFence != replayedFence {
		return Receipt{}, true, ErrStaleFence
	}
	if receipt.InputDigest != digest {
		return Receipt{}, true, ErrConflictingReplay
	}
	return receipt, true, nil
}

func (store *Store) loadAnyReceipt(
	mutation Mutation,
	kind string,
) (Receipt, bool, error) {
	var receipt Receipt
	err := readJSON(store.receiptPath(mutation, kind), &receipt)
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("%w: receipt: %v", ErrCorruptState, err)
	}
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, false, err
	}
	if receipt.Kind != kind ||
		receipt.OperationID != mutation.OperationID ||
		receipt.WorkspaceID != mutation.WorkspaceID {
		return Receipt{}, true, ErrConflictingReplay
	}
	return receipt, true, nil
}

func validateReceipt(receipt Receipt) error {
	if receipt.FormatVersion != receiptFormatVersion ||
		validateID(receipt.OperationID) != nil ||
		validateID(receipt.WorkspaceID) != nil ||
		!validOperationDigest(receipt.InputDigest) ||
		receipt.RecordedAt.IsZero() {
		return fmt.Errorf("%w: invalid operation receipt", ErrCorruptState)
	}
	switch receipt.Kind {
	case ReceiptWorkspaceCreate,
		ReceiptGenerationAdvance,
		ReceiptSnapshotCreate,
		ReceiptSnapshotDelete,
		ReceiptRestorePrepare,
		ReceiptRestoreSwap,
		ReceiptRestoreFinalize,
		ReceiptRestoreAbort,
		ReceiptWorkspaceDelete:
	default:
		return fmt.Errorf("%w: unknown receipt kind", ErrCorruptState)
	}
	if receipt.SnapshotID != "" && validateID(receipt.SnapshotID) != nil {
		return fmt.Errorf("%w: invalid receipt Snapshot ID", ErrCorruptState)
	}
	return nil
}

func validOperationDigest(value string) bool {
	fenceDigest, requestDigest, found := strings.Cut(value, ":")
	if !found || len(fenceDigest) != sha256.Size*2 || len(requestDigest) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(fenceDigest); err != nil {
		return false
	}
	if _, err := hex.DecodeString(requestDigest); err != nil {
		return false
	}
	return true
}

func (store *Store) restorePending(workspaceID string) (bool, error) {
	operationID, err := store.pendingRestoreOperation(workspaceID)
	return operationID != "", err
}

func (store *Store) stagedRestorePending(workspaceID string) (bool, error) {
	entries, err := os.ReadDir(store.stagedDir(workspaceID))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("SecondBox WorkspaceStore inspect staged restore state: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			return true, nil
		}
	}
	return false, nil
}

func (store *Store) pendingRestoreOperation(workspaceID string) (string, error) {
	for _, directory := range []string{
		store.stagedDir(workspaceID),
		store.rollbackDir(workspaceID),
	} {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("SecondBox WorkspaceStore inspect restore state: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			operationID := strings.TrimSuffix(entry.Name(), ".json")
			if err := validateID(operationID); err != nil {
				return "", fmt.Errorf("%w: invalid restore operation file", ErrCorruptState)
			}
			return operationID, nil
		}
	}
	return "", nil
}

func (store *Store) snapshotReferencedByRestore(
	workspaceID string,
	snapshotID string,
) (bool, error) {
	entries, err := os.ReadDir(store.stagedDir(workspaceID))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		operationID := strings.TrimSuffix(entry.Name(), ".json")
		staged, err := store.readStagedRestore(workspaceID, operationID)
		if err != nil {
			return false, err
		}
		if staged.SnapshotID == snapshotID {
			return true, nil
		}
	}
	return false, nil
}

func (store *Store) deleteWorkspaceSnapshots(workspaceID string) error {
	entries, err := os.ReadDir(store.snapshotsRoot())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || validateID(entry.Name()) != nil {
			return fmt.Errorf("%w: invalid Snapshot directory", ErrCorruptState)
		}
		manifest, err := store.readSnapshotManifest(entry.Name())
		if err != nil {
			return err
		}
		if manifest.WorkspaceID != workspaceID {
			continue
		}
		if err := os.Chmod(store.snapshotImagePath(entry.Name()), writableImageMode); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := removeExactFile(store.snapshotImagePath(entry.Name())); err != nil {
			return err
		}
		if err := removeExactFile(store.snapshotManifestPath(entry.Name())); err != nil {
			return err
		}
		if err := os.Remove(store.snapshotDir(entry.Name())); err != nil {
			return err
		}
	}
	return syncDir(store.snapshotsRoot())
}

func (store *Store) removeWorkspaceTree(workspaceID string) error {
	for _, directory := range []string{
		store.versionsDir(workspaceID),
		store.manifestDir(workspaceID),
		store.stagedDir(workspaceID),
		store.rollbackDir(workspaceID),
	} {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return fmt.Errorf("%w: unexpected nested Workspace directory", ErrCorruptState)
			}
			if err := removeExactFile(filepath.Join(directory, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(store.workspaceDir(workspaceID)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (store *Store) deleteWorkspaceReceipts(workspaceID string) error {
	if err := validateID(workspaceID); err != nil {
		return err
	}
	workspaceReceipts := filepath.Join(store.receiptsRoot(), workspaceID)
	operationEntries, err := os.ReadDir(workspaceReceipts)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, operationEntry := range operationEntries {
		if !operationEntry.IsDir() || validateID(operationEntry.Name()) != nil {
			return fmt.Errorf("%w: invalid Workspace receipt directory", ErrCorruptState)
		}
		operationReceipts := filepath.Join(workspaceReceipts, operationEntry.Name())
		receiptEntries, err := os.ReadDir(operationReceipts)
		if err != nil {
			return err
		}
		for _, receiptEntry := range receiptEntries {
			if receiptEntry.IsDir() ||
				filepath.Ext(receiptEntry.Name()) != ".json" ||
				filepath.Base(receiptEntry.Name()) != receiptEntry.Name() {
				return fmt.Errorf("%w: invalid Workspace receipt file", ErrCorruptState)
			}
			if err := removeExactFile(filepath.Join(operationReceipts, receiptEntry.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(operationReceipts); err != nil {
			return err
		}
	}
	if err := os.Remove(workspaceReceipts); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(store.receiptsRoot())
}

func (store *Store) lockWorkspace(workspaceID string) (*os.File, error) {
	if err := validateID(workspaceID); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(
		filepath.Join(store.locksRoot(), workspaceID+".lock"),
		os.O_CREATE|os.O_RDWR,
		writableImageMode,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore open Workspace lock: %w", err)
	}
	if err := tryLockFile(lock); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrActiveWriter
		}
		return nil, fmt.Errorf("SecondBox WorkspaceStore acquire Workspace lock: %w", err)
	}
	return lock, nil
}

func closeLockedFile(lock *os.File) error {
	if lock == nil {
		return nil
	}
	var first error
	if err := unlockFile(lock); err != nil {
		first = err
	}
	if err := lock.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

func atomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		temp.Close()
		if removeTemp {
			os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(writableImageMode); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	removeTemp = false
	return syncDir(directory)
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func inputDigest(value any) (string, error) {
	fencingToken, err := operationFencingToken(value)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("SecondBox WorkspaceStore encode operation input: %w", err)
	}
	fenceDigest := sha256.Sum256(fencingToken)
	requestDigest := sha256.Sum256(data)
	return hex.EncodeToString(fenceDigest[:]) + ":" +
		hex.EncodeToString(requestDigest[:]), nil
}

func operationFencingToken(value any) ([]byte, error) {
	var token []byte
	switch request := value.(type) {
	case CreateWorkspaceRequest:
		token = request.FencingToken
	case AdvanceGenerationRequest:
		token = request.FencingToken
	case CreateSnapshotRequest:
		token = request.FencingToken
	case DeleteSnapshotRequest:
		token = request.FencingToken
	case PrepareRestoreRequest:
		token = request.FencingToken
	case SwapRestoreRequest:
		token = request.FencingToken
	case RestoreMutation:
		token = request.FencingToken
	case DeleteWorkspaceRequest:
		token = request.FencingToken
	default:
		return nil, errors.New("SecondBox WorkspaceStore operation input type is unsupported")
	}
	if len(token) < 32 {
		return nil, ErrStaleFence
	}
	return token, nil
}

func validateMutation(mutation Mutation) error {
	if err := validateID(mutation.OperationID); err != nil {
		return err
	}
	if err := validateID(mutation.WorkspaceID); err != nil {
		return err
	}
	if len(mutation.FencingToken) < 32 {
		return ErrStaleFence
	}
	return nil
}

func validateRestoreRequest(
	mutation Mutation,
	snapshotID string,
	expectedGeneration uint64,
	nextGeneration uint64,
) error {
	if err := validateMutation(mutation); err != nil {
		return err
	}
	if err := validateID(snapshotID); err != nil {
		return err
	}
	if expectedGeneration < 1 || nextGeneration != expectedGeneration+1 {
		return ErrStaleGeneration
	}
	return nil
}

func validateID(id string) error {
	if !logicalIDPattern.MatchString(id) || id == "." || id == ".." {
		return ErrInvalidID
	}
	return nil
}

func stagedMatches(staged stagedRestore, request PrepareRestoreRequest) bool {
	return staged.OperationID == request.OperationID &&
		staged.WorkspaceID == request.WorkspaceID &&
		staged.SnapshotID == request.SnapshotID &&
		staged.ExpectedGeneration == request.ExpectedGeneration &&
		staged.NextGeneration == request.NextGeneration
}

func deterministicUUID(workspaceID string) string {
	digest := sha256.Sum256([]byte("SecondBox WorkspaceStore ext4 UUID\x00" + workspaceID))
	bytes := digest[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32]
}

func generationImageName(generation uint64, suffix string) string {
	return fmt.Sprintf("g%020d-%s.ext4", generation, suffix)
}

func requestNonce(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:16])
}

func validateExt4Image(path string, capacityBytes int64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open ext4 image: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore stat ext4 image: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != capacityBytes {
		return fmt.Errorf("%w: Workspace image size or type is invalid", ErrCorruptState)
	}
	magic := make([]byte, 2)
	if _, err := file.ReadAt(magic, ext4MagicOffset); err != nil {
		return fmt.Errorf("%w: read ext4 superblock: %v", ErrCorruptState, err)
	}
	if magic[0] != 0x53 || magic[1] != 0xef {
		return fmt.Errorf("%w: Workspace image is not ext4", ErrCorruptState)
	}
	return nil
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore inspect directory %q: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: WorkspaceStore path is not a directory", ErrCorruptState)
	}
	return nil
}

func removeExactFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore inspect deletion target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: deletion target is not a regular file", ErrCorruptState)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore remove exact file: %w", err)
	}
	return nil
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open directory for fsync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync directory: %w", err)
	}
	return nil
}

func (store *Store) validateImage(workspaceID string, image string, capacityBytes int64) error {
	path, err := store.validatedVersionPath(workspaceID, image)
	if err != nil {
		return err
	}
	return validateExt4Image(path, capacityBytes)
}

func (store *Store) validatedVersionPath(workspaceID string, image string) (string, error) {
	if filepath.Base(image) != image ||
		!strings.HasPrefix(image, "g") ||
		!strings.HasSuffix(image, ".ext4") ||
		strings.Contains(image, string(filepath.Separator)) {
		return "", fmt.Errorf("%w: invalid version image reference", ErrCorruptState)
	}
	path := filepath.Join(store.versionsDir(workspaceID), image)
	if filepath.Dir(path) != store.versionsDir(workspaceID) {
		return "", fmt.Errorf("%w: version image escaped Workspace", ErrCorruptState)
	}
	return path, nil
}

func (store *Store) workspacesRoot() string {
	return filepath.Join(store.root, "workspaces")
}

func (store *Store) snapshotsRoot() string {
	return filepath.Join(store.root, "snapshots")
}

func (store *Store) locksRoot() string {
	return filepath.Join(store.root, "locks")
}

func (store *Store) receiptsRoot() string {
	return filepath.Join(store.root, "receipts")
}

func (store *Store) workspaceDir(workspaceID string) string {
	return filepath.Join(store.workspacesRoot(), workspaceID)
}

func (store *Store) versionsDir(workspaceID string) string {
	return filepath.Join(store.workspaceDir(workspaceID), "versions")
}

func (store *Store) manifestDir(workspaceID string) string {
	return filepath.Join(store.workspaceDir(workspaceID), "manifest")
}

func (store *Store) stagedDir(workspaceID string) string {
	return filepath.Join(store.workspaceDir(workspaceID), "staged")
}

func (store *Store) rollbackDir(workspaceID string) string {
	return filepath.Join(store.workspaceDir(workspaceID), "rollback")
}

func (store *Store) versionPath(workspaceID string, image string) string {
	return filepath.Join(store.versionsDir(workspaceID), image)
}

func (store *Store) currentManifestPath(workspaceID string) string {
	return filepath.Join(store.manifestDir(workspaceID), "current.json")
}

func (store *Store) snapshotDir(snapshotID string) string {
	return filepath.Join(store.snapshotsRoot(), snapshotID)
}

func (store *Store) snapshotImagePath(snapshotID string) string {
	return filepath.Join(store.snapshotDir(snapshotID), "image.ext4")
}

func (store *Store) snapshotManifestPath(snapshotID string) string {
	return filepath.Join(store.snapshotDir(snapshotID), "snapshot.json")
}

func (store *Store) stagedImagePath(workspaceID string, operationID string) string {
	return filepath.Join(store.stagedDir(workspaceID), operationID+".ext4")
}

func (store *Store) stagedManifestPath(workspaceID string, operationID string) string {
	return filepath.Join(store.stagedDir(workspaceID), operationID+".json")
}

func (store *Store) rollbackPath(workspaceID string, operationID string) string {
	return filepath.Join(store.rollbackDir(workspaceID), operationID+".json")
}

func (store *Store) receiptPath(mutation Mutation, kind string) string {
	return filepath.Join(
		store.receiptsRoot(),
		mutation.WorkspaceID,
		mutation.OperationID,
		kind+".json",
	)
}
