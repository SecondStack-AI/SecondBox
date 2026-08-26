package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/pagination"
)

const (
	profileListCursorResource                   = "profiles"
	runnerPoolListCursorResource                = "runner_pools"
	runnerListCursorResource                    = "runners"
	sandboxListCursorResource                   = "sandboxes"
	tenantListCursorResource                    = "tenants"
	deploymentUsageTenantListCursorResource     = "deployment_usage_tenants"
	tenantUsageSubjectListCursorResource        = "tenant_usage_subjects"
	subjectListCursorResource                   = "subjects"
	tenantControllerAuthorityListCursorResource = "tenant_controller_authorities"
	applicationAuthorityListCursorResource      = "application_authorities"
)

type postgresListCursorBoundary struct {
	Active    bool
	CreatedAt time.Time
	ItemKey   string
}

func (store *PostgresControlPlaneStore) resolvePostgresListCursor(
	ctx context.Context,
	resourceKind string,
	scope string,
	cursor string,
	createdAtLookup string,
	lookupArguments ...any,
) (postgresListCursorBoundary, error) {
	itemKey, err := pagination.DecodeListCursor(resourceKind, scope, cursor)
	if err != nil {
		return postgresListCursorBoundary{}, err
	}
	if itemKey == "" {
		return postgresListCursorBoundary{}, nil
	}
	arguments := append(append([]any(nil), lookupArguments...), itemKey)
	var createdAt time.Time
	if err := store.pool.QueryRow(ctx, createdAtLookup, arguments...).Scan(&createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return postgresListCursorBoundary{}, pagination.ErrInvalidListCursor
		}
		return postgresListCursorBoundary{}, fmt.Errorf("SecondBox list page cursor lookup failed: %w", err)
	}
	return postgresListCursorBoundary{
		Active: true, CreatedAt: createdAt.UTC(), ItemKey: itemKey,
	}, nil
}

func encodePostgresListNextCursor(
	resourceKind string,
	scope string,
	itemKey string,
) (*string, error) {
	cursor, err := pagination.EncodeListCursor(resourceKind, scope, itemKey)
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}
