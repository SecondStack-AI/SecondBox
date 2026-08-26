// Package workspacestore owns runner-local durable Sandbox workspace images.
//
// The package deliberately exposes opaque handles instead of image paths. Only
// compute adapters in the privileged Runner process may resolve a handle to an
// open image file.
package workspacestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var (
	// ErrInvalidID rejects logical identifiers that cannot be mapped to an exact
	// traversal-safe local path.
	ErrInvalidID = errors.New("SecondBox WorkspaceStore logical ID is invalid")
	// ErrWorkspaceNotFound reports absent authoritative local Workspace state.
	ErrWorkspaceNotFound = errors.New("SecondBox WorkspaceStore Workspace is absent")
	// ErrSnapshotNotFound reports absent authoritative local Snapshot state.
	ErrSnapshotNotFound = errors.New("SecondBox WorkspaceStore Snapshot is absent")
	// ErrActiveWriter reports that compute still owns the Workspace attachment.
	ErrActiveWriter = errors.New("SecondBox WorkspaceStore Workspace has an active writer")
	// ErrStaleGeneration reports a manifest generation different from the
	// command's expected generation.
	ErrStaleGeneration = errors.New("SecondBox WorkspaceStore generation is stale")
	// ErrStaleFence reports replay of an operation under different fencing authority.
	ErrStaleFence = errors.New("SecondBox WorkspaceStore fencing authority is stale")
	// ErrConflictingReplay reports reuse of an operation ID with different
	// immutable command inputs.
	ErrConflictingReplay = errors.New("SecondBox WorkspaceStore operation replay conflicts")
	// ErrRestorePending reports staged or swapped restore state that must be
	// finalized or aborted before another destructive mutation.
	ErrRestorePending = errors.New("SecondBox WorkspaceStore restore is pending")
	// ErrSnapshotInUse reports a Snapshot referenced by staged restore state.
	ErrSnapshotInUse = errors.New("SecondBox WorkspaceStore Snapshot is in use")
	// ErrStorageIncompatible reports a root that cannot provide the mandatory
	// same-filesystem FICLONE semantics.
	ErrStorageIncompatible = errors.New("SecondBox WorkspaceStore storage is incompatible")
	// ErrCorruptState reports malformed manifests, receipts, or image evidence.
	ErrCorruptState = errors.New("SecondBox WorkspaceStore local state is corrupt")
	// ErrRelocationSealed reports a Workspace that is read-only while an
	// operator-authorized relocation owns its mutation slot.
	ErrRelocationSealed = errors.New("SecondBox WorkspaceStore Workspace is sealed for relocation")
)

const (
	ReceiptWorkspaceCreate       = "workspace_create"
	ReceiptWorkspaceClone        = "workspace_clone"
	ReceiptGenerationAdvance     = "generation_advance"
	ReceiptSnapshotCreate        = "snapshot_create"
	ReceiptSnapshotDelete        = "snapshot_delete"
	ReceiptRestorePrepare        = "restore_prepare"
	ReceiptRestoreSwap           = "restore_swap"
	ReceiptRestoreFinalize       = "restore_finalize"
	ReceiptRestoreAbort          = "restore_abort"
	ReceiptWorkspaceDelete       = "workspace_delete"
	ReceiptRelocationExport      = "relocation_export"
	ReceiptRelocationImport      = "relocation_import"
	ReceiptRelocationAbort       = "relocation_abort"
	workspaceFilesystemLabel     = "secondbox"
	currentManifestFormatVersion = 1
	receiptFormatVersion         = 1
)

// WorkspaceHandle is provider-neutral attachment authority. Its local image
// identity is intentionally private to this package.
type WorkspaceHandle struct {
	workspaceID string
	image       string
	generation  uint64
	nonce       string
}

// WorkspaceID returns the logical Workspace identity.
func (handle WorkspaceHandle) WorkspaceID() string {
	return handle.workspaceID
}

// Generation returns the manifest generation resolved for this handle.
func (handle WorkspaceHandle) Generation() uint64 {
	return handle.generation
}

// Attachment holds the exclusive runner-local writer lock and an open image
// descriptor. Callers must close it only after compute and every host-side user
// have stopped.
type Attachment struct {
	handle         WorkspaceHandle
	file           *os.File
	lock           *os.File
	driver         platformDriver
	capacityBytes  int64
	filesystemUUID string
}

// ComputeAttachment is the only runner-private value a compute backend may
// consume. It exposes an already-open image descriptor without exposing a
// reusable host path to lifecycle or protocol code.
type ComputeAttachment interface {
	Handle() WorkspaceHandle
	WorkspaceID() string
	Generation() uint64
	Descriptor() *os.File
	LockDescriptor() *os.File
	StableBlockID() string
	CapacityBytes() int64
	FilesystemUUID() string
	ChildDescriptorPath(int) string
	LinkInto(string) error
	Close() error
}

// Handle returns the opaque compute attachment value.
func (attachment *Attachment) Handle() WorkspaceHandle {
	if attachment == nil {
		return WorkspaceHandle{}
	}
	return attachment.handle
}

// WorkspaceID returns the logical Workspace bound to this attachment.
func (attachment *Attachment) WorkspaceID() string {
	return attachment.Handle().WorkspaceID()
}

// Generation returns the manifest generation held by the writer lock.
func (attachment *Attachment) Generation() uint64 {
	return attachment.Handle().Generation()
}

// Descriptor returns the already-open inherited Workspace descriptor. Its
// display name is provider-neutral and cannot be reused as a host path.
func (attachment *Attachment) Descriptor() *os.File {
	if attachment == nil {
		return nil
	}
	return attachment.file
}

// LockDescriptor returns the open descriptor holding the exclusive writer
// lock. A compute process that inherits a duplicate shares the same open file
// description, so the flock stays held until the last holder exits: a crashed
// runner cannot release the Workspace to a replacement while its compute is
// still flushing.
func (attachment *Attachment) LockDescriptor() *os.File {
	if attachment == nil {
		return nil
	}
	return attachment.lock
}

func (*Attachment) StableBlockID() string { return "workspace" }

func (attachment *Attachment) CapacityBytes() int64 {
	if attachment == nil {
		return 0
	}
	return attachment.capacityBytes
}

func (attachment *Attachment) FilesystemUUID() string {
	if attachment == nil {
		return ""
	}
	return attachment.filesystemUUID
}

func (attachment *Attachment) ChildDescriptorPath(descriptor int) string {
	if attachment == nil || attachment.driver == nil {
		return ""
	}
	return attachment.driver.ChildDescriptorPath(descriptor)
}

// LinkInto publishes a same-filesystem hard link for a compute backend's
// private jail without revealing the authoritative WorkspaceStore path.
func (attachment *Attachment) LinkInto(destination string) error {
	if attachment == nil || attachment.driver == nil || attachment.file == nil {
		return fmt.Errorf("SecondBox WorkspaceStore attachment is closed")
	}
	return attachment.driver.LinkDescriptor(attachment.file, destination)
}

// Close releases the image descriptor and exclusive Workspace writer lock.
func (attachment *Attachment) Close() error {
	if attachment == nil {
		return nil
	}
	var first error
	if attachment.file != nil {
		if err := attachment.file.Sync(); err != nil {
			first = err
		}
		if err := attachment.file.Close(); err != nil {
			if first == nil {
				first = err
			}
		}
		attachment.file = nil
	}
	if attachment.lock != nil {
		// Never explicitly release the writer flock: a compute backend's
		// helper inherits a duplicate of this descriptor sharing the same
		// open-file description, so an explicit unlock would surrender the
		// fence while that helper may still be flushing the image. Closing
		// this descriptor lets the kernel release the lock only when the
		// last holder - the helper included - has closed it.
		if err := attachment.lock.Close(); err != nil && first == nil {
			first = err
		}
		attachment.lock = nil
	}
	return first
}

// Mutation identifies one idempotent runner-local command.
type Mutation struct {
	OperationID  string
	WorkspaceID  string
	FencingToken []byte
}

// CreateWorkspaceRequest creates generation one at an explicit logical
// capacity. Capacity is never inferred from a Runner default.
type CreateWorkspaceRequest struct {
	Mutation
	CapacityBytes int64
}

// CloneWorkspaceRequest creates generation one from one immutable local Snapshot.
type CloneWorkspaceRequest struct {
	Mutation
	SourceSnapshot string
	CapacityBytes  int64
}

// AdvanceGenerationRequest durably republishes the current image at exactly the
// requested next generation.
type AdvanceGenerationRequest struct {
	Mutation
	ExpectedGeneration uint64
	NextGeneration     uint64
}

// CreateSnapshotRequest reflinks the current stopped Workspace image.
type CreateSnapshotRequest struct {
	Mutation
	SnapshotID         string
	ExpectedGeneration uint64
}

// DeleteSnapshotRequest removes one unattached immutable local Snapshot.
type DeleteSnapshotRequest struct {
	Mutation
	SnapshotID string
}

// PrepareRestoreRequest stages a writable reflink child without changing the
// authoritative current-image manifest.
type PrepareRestoreRequest struct {
	Mutation
	SnapshotID         string
	ExpectedGeneration uint64
	NextGeneration     uint64
}

// SwapRestoreRequest atomically selects a previously staged restore.
type SwapRestoreRequest struct {
	Mutation
	SnapshotID         string
	ExpectedGeneration uint64
	NextGeneration     uint64
}

// RestoreMutation identifies finalize or abort cleanup for one restore.
type RestoreMutation struct {
	Mutation
}

// DeleteWorkspaceRequest removes one stopped Workspace after its local
// Snapshots and restore state have been removed.
type DeleteWorkspaceRequest struct {
	Mutation
	ExpectedGeneration uint64
}

// RelocationExportRequest seals and opens one stopped current Workspace image.
type RelocationExportRequest struct {
	Mutation
	ExpectedGeneration uint64
}

// RelocationImportRequest creates one target image at the source generation.
type RelocationImportRequest struct {
	Mutation
	Generation    uint64
	CapacityBytes int64
}

// RelocationExport holds the source reader lock after the seal receipt is durable.
type RelocationExport interface {
	io.ReadCloser
	SizeBytes() int64
	Receipt() Receipt
}

// RelocationImport accepts sequential credit-bounded chunks into target staging.
type RelocationImport interface {
	WriteChunk(uint64, []byte) error
	Complete(uint64, string) (Receipt, error)
	Abort() error
	CompletedReceipt() (Receipt, bool)
}

// Receipt is immutable replay evidence persisted before acknowledging a local
// mutation result.
type Receipt struct {
	FormatVersion      int       `json:"formatVersion"`
	Kind               string    `json:"kind"`
	OperationID        string    `json:"operationId"`
	WorkspaceID        string    `json:"workspaceId"`
	SnapshotID         string    `json:"snapshotId,omitempty"`
	InputDigest        string    `json:"inputDigest"`
	Generation         uint64    `json:"generation,omitempty"`
	PreviousGeneration uint64    `json:"previousGeneration,omitempty"`
	CapacityBytes      int64     `json:"capacityBytes,omitempty"`
	Checksum           string    `json:"checksum,omitempty"`
	RecordedAt         time.Time `json:"recordedAt"`
}

// WorkspaceInspection is bounded logical local-storage evidence.
type WorkspaceInspection struct {
	WorkspaceID      string
	Generation       uint64
	CapacityBytes    int64
	Formatted        bool
	RestorePending   bool
	ActiveWriter     bool
	RelocationSealed bool
}

// ReconcileReport contains bounded logical evidence used after Runner restart.
type ReconcileReport struct {
	Workspaces []WorkspaceInspection
	Receipts   []Receipt
}

// WorkspaceStore is the provider-neutral runner-owned local workspace port.
// Every mutating request includes a stable operation ID.
type WorkspaceStore interface {
	Create(context.Context, CreateWorkspaceRequest) (Receipt, error)
	CloneFromSnapshot(context.Context, CloneWorkspaceRequest) (Receipt, error)
	Open(context.Context, string, uint64) (ComputeAttachment, error)
	AdvanceGeneration(context.Context, AdvanceGenerationRequest) (Receipt, error)
	CreateSnapshot(context.Context, CreateSnapshotRequest) (Receipt, error)
	DeleteSnapshot(context.Context, DeleteSnapshotRequest) (Receipt, error)
	PrepareRestore(context.Context, PrepareRestoreRequest) (Receipt, error)
	SwapRestore(context.Context, SwapRestoreRequest) (Receipt, error)
	FinalizeRestore(context.Context, RestoreMutation) (Receipt, error)
	AbortRestore(context.Context, RestoreMutation) (Receipt, error)
	DeleteWorkspace(context.Context, DeleteWorkspaceRequest) (Receipt, error)
	OpenRelocationExport(context.Context, RelocationExportRequest) (RelocationExport, error)
	BeginRelocationImport(context.Context, RelocationImportRequest) (RelocationImport, error)
	AbortRelocation(context.Context, RelocationExportRequest) (Receipt, error)
	Inspect(context.Context, string) (WorkspaceInspection, error)
	Reconcile(context.Context) (ReconcileReport, error)
}
