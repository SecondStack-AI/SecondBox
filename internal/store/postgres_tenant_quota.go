package store

import (
	"context"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (store *PostgresControlPlaneStore) UpdateManagedTenantQuota(ctx context.Context, tenantRef string, quota contracts.TenantQuota, expectedRevision int64, now time.Time, idempotency ports.AdminIdempotencyInput) (contracts.Tenant, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant quota transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Tenant
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
		}
		return replayed, result, nil
	}
	if err := rowlock.TenantQuota(ctx, tx, tenantRef); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR UPDATE`, tenantRef))
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if tenant.Revision != expectedRevision {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if tenant.State == contracts.TenantStateExpired {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrResourceExpired
	}
	usage, err := readTenantQuotaUsage(ctx, tx, tenantRef, now)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if tenantDataPlaneQuotaWouldExceed(quota, usage, quotaUsage{}) || !quota.MaxActiveSubjects.Allows(usage.ActiveSubjects) || !quota.MaxApplicationAuthorities.Allows(usage.ApplicationAuthorities) {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrManagementConflict
	}
	encoded, err := encodeManagementJSON("Tenant aggregate quota", quota)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	tenant.AggregateQuota, tenant.Revision, tenant.UpdatedAt = quota, tenant.Revision+1, now.UTC()
	if _, err := tx.Exec(ctx, `UPDATE secondbox.tenants SET aggregate_quota_json=$2,revision=$3,updated_at=$4 WHERE ref=$1`, tenantRef, encoded, tenant.Revision, tenant.UpdatedAt); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant quota update failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.tenant_quotas SET max_sandboxes=$2,max_active_instances=$3,max_vcpu_count=$4,max_memory_bytes=$5,max_snapshots=$6,max_port_sessions=$7,max_concurrent_operations=$8,max_active_subjects=$9,max_application_authorities=$10,updated_at=$11 WHERE tenant_ref=$1`, tenantRef, quota.MaxSandboxes, quota.MaxActiveInstances, quota.MaxVCPUCount, quota.MaxMemoryBytes, quota.MaxSnapshots, quota.MaxPortSessions, quota.MaxConcurrentOperations, quota.MaxActiveSubjects, quota.MaxApplicationAuthorities, tenant.UpdatedAt); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant quota ledger update failed: %w", err)
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, tenant)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant quota commit failed: %w", err)
	}
	return tenant, result, nil
}
