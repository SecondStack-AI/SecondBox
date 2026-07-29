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

// CreateSandboxSnapshot retains the current published checkpoint without claiming process state.
func (service *ControlPlaneService) CreateSandboxSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
	request contracts.CreateSnapshotRequest,
) (contracts.Snapshot, error) {
	if err := requireSnapshotLifecycle(principal); err != nil {
		return contracts.Snapshot{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Snapshot{}, err
	}
	if expectedRevision < 1 {
		return contracts.Snapshot{}, errors.New("SecondBox Snapshot expected revision must be positive")
	}
	if err := validateSnapshotRequest(request); err != nil {
		return contracts.Snapshot{}, err
	}
	_, checkpointPolicy, err := service.store.GetSandboxLifecyclePolicy(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
	)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	now := service.now().UTC()
	snapshot := contracts.Snapshot{
		ID: service.newID("snp"), ProjectID: principal.ProjectID,
		TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SandboxID: sandboxID,
		Name: request.Name, Metadata: cloneMetadata(request.Metadata),
		RetainUntil: now.Add(time.Duration(checkpointPolicy.RetentionSeconds) * time.Second),
		CreatedAt:   now,
	}
	audit := service.newAudit(
		ctx, principal, "snapshot.created", "snapshot", snapshot.ID, principal.ProjectID, now,
	)
	created, err := service.store.CreateSnapshot(ctx, ports.SnapshotCreationInput{
		Snapshot: snapshot, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return contracts.Snapshot{}, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Snapshot{}, err
	}
	return created, nil
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
		return contracts.SnapshotPage{}, errors.New("SecondBox Snapshot page cursor exceeds its bound")
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

// DeleteSnapshot ends one checkpoint retention root idempotently.
func (service *ControlPlaneService) DeleteSnapshot(
	ctx context.Context,
	principal contracts.Principal,
	snapshotID string,
	idempotencyKey string,
) error {
	if err := requireSnapshotLifecycle(principal); err != nil {
		return err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	requestHash, err := hashCanonicalRequest(struct {
		SnapshotID string `json:"snapshotId"`
	}{SnapshotID: snapshotID})
	if err != nil {
		return err
	}
	now := service.now().UTC()
	audit := service.newAudit(
		ctx, principal, "snapshot.retention_ended", "snapshot",
		snapshotID, principal.ProjectID, now,
	)
	if err := service.store.EndSnapshotRetention(ctx, ports.SnapshotRetentionInput{
		ProjectID: principal.ProjectID, TenantRef: principal.TenantRef,
		SubjectRef: principal.SubjectRef, SnapshotID: snapshotID,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), Now: now,
	}); err != nil {
		return err
	}
	return service.store.AppendAuditEvent(ctx, audit)
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
		return errors.New("SecondBox Snapshot name is invalid")
	}
	return validateSandboxMetadata(request.Metadata)
}
