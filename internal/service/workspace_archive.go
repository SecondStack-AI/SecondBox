package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type workspaceArchiveStore interface {
	ArchiveSandbox(context.Context, ports.WorkspaceArchiveInput) (contracts.Operation, error)
}

func (service *ControlPlaneService) ArchiveSandbox(ctx context.Context, principal contracts.Principal, sandboxID, key string, revision int64) (contracts.Operation, bool, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Operation{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(key); err != nil {
		return contracts.Operation{}, false, err
	}
	if revision < 1 {
		return contracts.Operation{}, false, invalidField("If-Match", "must contain a positive revision ETag")
	}
	store, ok := service.store.(workspaceArchiveStore)
	if !ok {
		return contracts.Operation{}, false, errors.New("SecondBox workspace archive storage is unavailable")
	}
	now := service.now().UTC()
	operation := contracts.Operation{ID: service.newID("op"), SandboxID: sandboxID, Kind: "archive", State: contracts.OperationStatePending, RequestID: service.requestID(ctx), CreatedAt: now, UpdatedAt: now}
	digest := sha256.Sum256([]byte(sandboxID))
	stored, err := store.ArchiveSandbox(ctx, ports.WorkspaceArchiveInput{Principal: principal, AuditEvent: service.newAudit(ctx, principal, "sandbox.archive", "sandbox", sandboxID, principal.TenantRef, now), SandboxID: sandboxID, Operation: operation, ArchiveID: service.newID("archive"), CommandID: service.newID("command"), FencingToken: []byte(service.newCredentialMaterial()), ExpectedRevision: revision, IdempotencyKey: key, RequestHash: hex.EncodeToString(digest[:]), IdempotencyEnds: service.idempotencyExpiration(now), Now: now})
	if err != nil {
		return contracts.Operation{}, false, err
	}
	return stored, stored.ID != operation.ID, nil
}
