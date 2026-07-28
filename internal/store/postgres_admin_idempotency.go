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
	scope := input.ProjectID + "\x1f" + input.Operation + "\x1f" + input.TargetID + "\x1f" + input.Key
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope); err != nil {
		return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox admin idempotency lock failed: %w", err)
	}
	var requestHash string
	var responseJSON []byte
	var responseSecret []byte
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_json,response_secret,expires_at
		FROM secondbox.idempotency_records
		WHERE project_id=$1 AND operation=$2 AND target_id=$3 AND idempotency_key=$4`,
		input.ProjectID, input.Operation, input.TargetID, input.Key,
	).Scan(&requestHash, &responseJSON, &responseSecret, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.AdminIdempotencyResult{}, false, nil
	}
	if err != nil {
		return ports.AdminIdempotencyResult{}, false, fmt.Errorf("SecondBox admin idempotency lookup failed: %w", err)
	}
	if !expiresAt.After(input.Now.UTC()) {
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.idempotency_records
			WHERE project_id=$1 AND operation=$2 AND target_id=$3 AND idempotency_key=$4`,
			input.ProjectID, input.Operation, input.TargetID, input.Key,
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
	return ports.AdminIdempotencyResult{
		Replayed: true, ResponseSecret: append([]byte(nil), responseSecret...),
	}, true, nil
}

func insertAdminIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	input ports.AdminIdempotencyInput,
	response any,
) (ports.AdminIdempotencyResult, error) {
	if input.Key == "" {
		return ports.AdminIdempotencyResult{}, nil
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox admin idempotency response encode failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			project_id,operation,target_id,idempotency_key,request_hash,response_resource_id,
			response_json,response_secret,created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		input.ProjectID, input.Operation, input.TargetID, input.Key, input.RequestHash,
		adminResponseResourceID(response), responseJSON, input.ResponseSecret,
		input.Now.UTC(), input.Ends.UTC(),
	); err != nil {
		return ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox admin idempotency insert failed: %w", err)
	}
	return ports.AdminIdempotencyResult{
		ResponseSecret: append([]byte(nil), input.ResponseSecret...),
	}, nil
}

func adminResponseResourceID(response any) string {
	switch resource := response.(type) {
	case contracts.Project:
		return resource.ID
	case contracts.ServiceAccount:
		return resource.ID
	case contracts.APIKey:
		return resource.ID
	case contracts.Profile:
		return resource.Name
	default:
		return ""
	}
}
