package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type ImagePreparationStore interface {
	PrepareImage(context.Context, ports.ImagePreparationInput) (contracts.Operation, bool, error)
}

func (service *ControlPlaneService) PrepareImage(ctx context.Context, principal contracts.Principal, key string, request contracts.PrepareImageRequest, grants []string) (contracts.Operation, bool, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Operation{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(key); err != nil {
		return contracts.Operation{}, false, err
	}
	if err := request.Image.Validate(); err != nil {
		return contracts.Operation{}, false, invalidRequest(err)
	}
	if request.Profile != "" && !profileNamePattern.MatchString(request.Profile) {
		return contracts.Operation{}, false, ports.ErrInvalidRequest
	}
	store, ok := service.store.(ImagePreparationStore)
	if !ok {
		return contracts.Operation{}, false, errors.New("SecondBox image preparation store is not configured")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	hash := sha256.Sum256(encoded)
	now := service.now().UTC()
	operation := contracts.Operation{ID: service.newID("op"), Kind: "prepare_image", State: contracts.OperationStatePending, RequestID: service.requestID(ctx), RequestMetadata: request.Image.LifecycleMetadata(), CreatedAt: now, UpdatedAt: now}
	audit := service.newAudit(ctx, principal, "image.preparation_requested", "operation", operation.ID, principal.TenantRef, now)
	return store.PrepareImage(ctx, ports.ImagePreparationInput{Principal: principal, Request: request, ProfileGrants: grants, Operation: operation, Idempotency: ports.AdminIdempotencyInput{TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, Operation: "image.prepare", Key: key, RequestHash: hex.EncodeToString(hash[:]), Now: now, Ends: service.idempotencyExpiration(now), AuditEvent: &audit}})
}
