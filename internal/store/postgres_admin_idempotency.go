package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func lookupAdminIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	input ports.AdminIdempotencyInput,
	response any,
) (ports.AdminIdempotencyResult, bool, error) {
	if input.Key == "" {
		return ports.AdminIdempotencyResult{}, false, nil
	}
	tenantRef, subjectRef := adminIdempotencyRefs(input)
	scope := tenantRef + "\x1f" + subjectRef + "\x1f" +
		input.Operation + "\x1f" + input.TargetID + "\x1f" + input.Key
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope); err != nil {
		return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox admin idempotency lock failed: %w", err)
	}
	var requestHash string
	var responseJSON []byte
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_json,expires_at
		FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		tenantRef, subjectRef, input.Operation, input.TargetID, input.Key,
	).Scan(&requestHash, &responseJSON, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.AdminIdempotencyResult{}, false, nil
	}
	if err != nil {
		return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox admin idempotency lookup failed: %w", err)
	}
	if !expiresAt.After(input.Now.UTC()) {
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.idempotency_records
			WHERE tenant_ref=$1 AND subject_ref=$2
			  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
			tenantRef, subjectRef, input.Operation, input.TargetID, input.Key,
		); err != nil {
			return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox expired admin idempotency cleanup failed: %w", err)
		}
		return ports.AdminIdempotencyResult{}, false, nil
	}
	if requestHash != input.RequestHash {
		return ports.AdminIdempotencyResult{}, false, ports.ErrIdempotencyConflict
	}
	if len(responseJSON) == 0 {
		return ports.AdminIdempotencyResult{}, false, errors.New("SecondBox admin idempotency response is missing")
	}
	if err := json.Unmarshal(responseJSON, response); err != nil {
		return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox admin idempotency response decode failed: %w", err)
	}
	return ports.AdminIdempotencyResult{Replayed: true}, true, nil
}

func insertAdminIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	input ports.AdminIdempotencyInput,
	response any,
) (ports.AdminIdempotencyResult, error) {
	if input.Key == "" {
		if input.AuditEvent != nil {
			if err := insertAuditEvent(ctx, tx, *input.AuditEvent); err != nil {
				return ports.AdminIdempotencyResult{}, err
			}
		}
		return ports.AdminIdempotencyResult{}, nil
	}
	tenantRef, subjectRef := adminIdempotencyRefs(input)
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox admin idempotency response encode failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,response_resource_id,
			response_json,created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenantRef, subjectRef,
		input.Operation, input.TargetID, input.Key, input.RequestHash,
		adminResponseResourceID(response), responseJSON,
		input.Now.UTC(), input.Ends.UTC(),
	); err != nil {
		return ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox admin idempotency insert failed: %w", err)
	}
	if input.AuditEvent == nil {
		return ports.AdminIdempotencyResult{}, errors.New("SecondBox admin mutation audit event is required")
	}
	event := *input.AuditEvent
	if event.ResourceID == "" {
		event.ResourceID = adminResponseResourceID(response)
	}
	if err := insertAuditEvent(ctx, tx, event); err != nil {
		return ports.AdminIdempotencyResult{}, err
	}
	return ports.AdminIdempotencyResult{}, nil
}

func adminIdempotencyRefs(input ports.AdminIdempotencyInput) (string, string) {
	tenantRef, subjectRef := input.TenantRef, input.SubjectRef
	if tenantRef == "" {
		tenantRef = input.TenantRef
	}
	if tenantRef == "" {
		tenantRef = "secondbox"
	}
	if subjectRef == "" {
		subjectRef = "secondbox-admin"
	}
	return tenantRef, subjectRef
}

func adminResponseResourceID(response any) string {
	switch resource := response.(type) {
	case contracts.Profile:
		return resource.Name
	case contracts.Tenant:
		return resource.Ref
	case contracts.Subject:
		return resource.Ref
	case contracts.TenantControllerAuthority:
		return resource.ID
	case contracts.TenantControllerCredentialResponse:
		return resource.Authority.ID
	case contracts.ApplicationAuthority:
		return resource.ID
	case contracts.ApplicationCredentialResponse:
		return resource.Authority.ID
	case contracts.Operation:
		return resource.ID
	default:
		return ""
	}
}
