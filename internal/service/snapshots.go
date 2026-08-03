package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// CreateSandboxSnapshot admits an asynchronous runner-local clone.
func (service *ControlPlaneService) CreateSandboxSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
	request contracts.CreateSnapshotRequest,
) (contracts.Operation, bool, error) {
	if err := requireSnapshotLifecycle(principal); err != nil {
		return contracts.Operation{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Operation{}, false, err
	}
	if expectedRevision < 1 {
		return contracts.Operation{}, false, invalidRequest(errors.New("SecondBox Snapshot expected revision must be positive"))
	}
	if err := validateSnapshotRequest(request); err != nil {
		return contracts.Operation{}, false, err
	}
	_, retentionPolicy, err := service.store.GetSandboxLifecyclePolicy(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
	)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	now := service.now().UTC()
	retainUntil := now.Add(time.Duration(retentionPolicy.SnapshotRetentionSeconds) * time.Second)
	snapshot := contracts.Snapshot{
		ID: service.newID("snp"), TenantRef: principal.TenantRef,
		SubjectRef: principal.SubjectRef, SandboxID: sandboxID,
		Name: request.Name, Metadata: cloneMetadata(request.Metadata),
		RetainUntil: &retainUntil,
		CreatedAt:   now,
	}
	operation := contracts.Operation{
		ID: service.newID("op"), SandboxID: sandboxID, Kind: "snapshot_create",
		State: contracts.OperationStatePending, RequestID: service.requestID(ctx),
		CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(
		ctx, principal, "snapshot.created", "snapshot", snapshot.ID, principal.TenantRef, now,
	)
	created, err := service.store.CreateSnapshot(ctx, ports.SnapshotCreationInput{
		Snapshot: snapshot, Operation: operation, EffectID: service.newID("effect"),
		CommandID: service.newID("command"), FencingToken: []byte(service.newCredentialMaterial()),
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return contracts.Operation{}, false, err
	}
	replayed := created.ID != operation.ID
	if !replayed {
		if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
			return contracts.Operation{}, false, err
		}
	}
	return created, replayed, nil
}

// ListSandboxSnapshots returns one retained Snapshot page inside the authenticated Project.
func (service *ControlPlaneService) ListSandboxSnapshots(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	limit int,
	cursor string,
) (contracts.SnapshotPage, error) {
	if err := requireSnapshotRead(principal); err != nil {
		return contracts.SnapshotPage{}, err
	}
	if len(cursor) > 512 {
		return contracts.SnapshotPage{}, invalidRequest(errors.New("SecondBox Snapshot page cursor exceeds its bound"))
	}
	return service.store.ListSnapshots(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
		boundedLimit(limit), cursor, service.now().UTC(),
	)
}

// GetSnapshot returns retained immutable disk-state metadata without provider authority.
func (service *ControlPlaneService) GetSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	snapshotID string,
) (contracts.Snapshot, error) {
	if err := requireSnapshotRead(principal); err != nil {
		return contracts.Snapshot{}, err
	}
	return service.store.GetSnapshot(
		ctx, principal.TenantRef, principal.SubjectRef, snapshotID, service.now().UTC(),
	)
}

// DeleteSnapshot admits an asynchronous runner-local deletion.
func (service *ControlPlaneService) DeleteSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	snapshotID string,
	idempotencyKey string,
) (contracts.Operation, bool, error) {
	if err := requireSnapshotLifecycle(principal); err != nil {
		return contracts.Operation{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Operation{}, false, err
	}
	requestHash, err := hashCanonicalRequest(struct {
		SnapshotID string `json:"snapshotId"`
	}{SnapshotID: snapshotID})
	if err != nil {
		return contracts.Operation{}, false, err
	}
	now := service.now().UTC()
	audit := service.newAudit(
		ctx, principal, "snapshot.retention_ended", "snapshot",
		snapshotID, principal.TenantRef, now,
	)
	operation := contracts.Operation{
		ID: service.newID("op"), Kind: "snapshot_delete", State: contracts.OperationStatePending,
		RequestID: service.requestID(ctx), CreatedAt: now, UpdatedAt: now,
	}
	stored, err := service.store.DeleteSnapshot(ctx, ports.SnapshotDeletionInput{
		TenantRef:  principal.TenantRef,
		SubjectRef: principal.SubjectRef, SnapshotID: snapshotID,
		Operation: operation, EffectID: service.newID("effect"), CommandID: service.newID("command"),
		FencingToken:   []byte(service.newCredentialMaterial()),
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), Now: now,
	})
	if err != nil {
		return contracts.Operation{}, false, err
	}
	replayed := stored.ID != operation.ID
	if !replayed {
		if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
			return contracts.Operation{}, false, err
		}
	}
	return stored, replayed, nil
}

// RestoreSandboxSnapshot admits a stopped-only in-place restore.
func (service *ControlPlaneService) RestoreSandboxSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
	request contracts.RestoreSnapshotRequest,
) (contracts.Operation, bool, error) {
	if err := requireSnapshotLifecycle(principal); err != nil {
		return contracts.Operation{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Operation{}, false, err
	}
	if expectedRevision < 1 || strings.TrimSpace(request.SnapshotID) == "" {
		return contracts.Operation{}, false, invalidRequest(errors.New("SecondBox Snapshot restore revision and Snapshot ID are required"))
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	now := service.now().UTC()
	operation := contracts.Operation{
		ID: service.newID("op"), SandboxID: sandboxID, Kind: "snapshot_restore",
		State: contracts.OperationStatePending, RequestID: service.requestID(ctx),
		CreatedAt: now, UpdatedAt: now,
	}
	stored, err := service.store.RestoreSnapshot(ctx, ports.SnapshotRestoreInput{
		TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef,
		SandboxID: sandboxID, SnapshotID: request.SnapshotID, Operation: operation,
		RestoreID: service.newID("restore"), PrepareEffectID: service.newID("effect"),
		SwapEffectID: service.newID("effect"), FinalizeEffectID: service.newID("effect"),
		AbortEffectID: service.newID("effect"), PrepareCommandID: service.newID("command"),
		SwapCommandID: service.newID("command"), FinalizeCommandID: service.newID("command"),
		AbortCommandID: service.newID("command"),
		FencingToken:   []byte(service.newCredentialMaterial()),
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), ExpectedRevision: expectedRevision, Now: now,
	})
	if err != nil {
		return contracts.Operation{}, false, err
	}
	replayed := stored.ID != operation.ID
	if !replayed {
		audit := service.newAudit(
			ctx, principal, "snapshot.restore_requested", "snapshot",
			request.SnapshotID, principal.TenantRef, now,
		)
		if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
			return contracts.Operation{}, false, err
		}
	}
	return stored, replayed, nil
}

func requireSnapshotLifecycle(principal contracts.Principal) error {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func requireSnapshotRead(principal contracts.Principal) error {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func validateSnapshotRequest(request contracts.CreateSnapshotRequest) error {
	if !utf8.ValidString(request.Name) || strings.TrimSpace(request.Name) == "" ||
		len(request.Name) > 255 {
		return invalidRequest(errors.New("SecondBox Snapshot name is invalid"))
	}
	return validateSandboxMetadata(request.Metadata)
}
