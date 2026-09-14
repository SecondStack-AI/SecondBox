package store

import (
	"context"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func (store *PostgresControlPlaneStore) GetSubjectCapacity(ctx context.Context, tenantRef, subjectRef string, observedAt time.Time) (contracts.SubjectCapacity, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return contracts.SubjectCapacity{}, fmt.Errorf("SecondBox Subject capacity transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	limits, err := readSubjectQuota(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return contracts.SubjectCapacity{}, err
	}
	usage, err := readSubjectQuotaUsage(ctx, tx, tenantRef, subjectRef, observedAt)
	if err != nil {
		return contracts.SubjectCapacity{}, err
	}
	tenantLimits, err := readTenantQuota(ctx, tx, tenantRef)
	if err != nil {
		return contracts.SubjectCapacity{}, err
	}
	tenantUsage, err := readTenantQuotaUsage(ctx, tx, tenantRef, observedAt)
	if err != nil {
		return contracts.SubjectCapacity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.SubjectCapacity{}, fmt.Errorf("SecondBox Subject capacity commit failed: %w", err)
	}
	return contracts.SubjectCapacity{
		SubjectRef: subjectRef, Limits: limits, ObservedAt: observedAt.UTC(),
		Usage: contracts.QuotaUsage{Sandboxes: usage.sandboxes, ActiveInstances: usage.activeInstances, VCPUCount: usage.vcpuCount, MemoryBytes: usage.memoryBytes, Snapshots: usage.snapshots, PortSessions: usage.portSessions, ConcurrentOperations: usage.concurrentOperations},
		ConstrainingScopes: contracts.QuotaConstrainingScopes{
			Sandboxes:            quotaHeadroomScope(limits.MaxSandboxes.Remaining(usage.sandboxes), tenantLimits.MaxSandboxes.Remaining(tenantUsage.Sandboxes)),
			ActiveInstances:      quotaHeadroomScope(limits.MaxActiveInstances.Remaining(usage.activeInstances), tenantLimits.MaxActiveInstances.Remaining(tenantUsage.ActiveInstances)),
			VCPUCount:            quotaHeadroomScope(limits.MaxVCPUCount.Remaining(usage.vcpuCount), tenantLimits.MaxVCPUCount.Remaining(tenantUsage.VCPUCount)),
			MemoryBytes:          quotaHeadroomScope(limits.MaxMemoryBytes.Remaining(usage.memoryBytes), tenantLimits.MaxMemoryBytes.Remaining(tenantUsage.MemoryBytes)),
			Snapshots:            quotaHeadroomScope(limits.MaxSnapshots.Remaining(usage.snapshots), tenantLimits.MaxSnapshots.Remaining(tenantUsage.Snapshots)),
			PortSessions:         quotaHeadroomScope(limits.MaxPortSessions.Remaining(usage.portSessions), tenantLimits.MaxPortSessions.Remaining(tenantUsage.PortSessions)),
			ConcurrentOperations: quotaHeadroomScope(limits.MaxConcurrentOperations.Remaining(usage.concurrentOperations), tenantLimits.MaxConcurrentOperations.Remaining(tenantUsage.ConcurrentOperations)),
		},
		Available: contracts.QuotaHeadroom{
			Sandboxes:            contracts.MinimumPolicyLimit(limits.MaxSandboxes.Remaining(usage.sandboxes), tenantLimits.MaxSandboxes.Remaining(tenantUsage.Sandboxes)),
			ActiveInstances:      contracts.MinimumPolicyLimit(limits.MaxActiveInstances.Remaining(usage.activeInstances), tenantLimits.MaxActiveInstances.Remaining(tenantUsage.ActiveInstances)),
			VCPUCount:            contracts.MinimumPolicyLimit(limits.MaxVCPUCount.Remaining(usage.vcpuCount), tenantLimits.MaxVCPUCount.Remaining(tenantUsage.VCPUCount)),
			MemoryBytes:          contracts.MinimumPolicyLimit(limits.MaxMemoryBytes.Remaining(usage.memoryBytes), tenantLimits.MaxMemoryBytes.Remaining(tenantUsage.MemoryBytes)),
			Snapshots:            contracts.MinimumPolicyLimit(limits.MaxSnapshots.Remaining(usage.snapshots), tenantLimits.MaxSnapshots.Remaining(tenantUsage.Snapshots)),
			PortSessions:         contracts.MinimumPolicyLimit(limits.MaxPortSessions.Remaining(usage.portSessions), tenantLimits.MaxPortSessions.Remaining(tenantUsage.PortSessions)),
			ConcurrentOperations: contracts.MinimumPolicyLimit(limits.MaxConcurrentOperations.Remaining(usage.concurrentOperations), tenantLimits.MaxConcurrentOperations.Remaining(tenantUsage.ConcurrentOperations)),
		},
	}, nil
}

func quotaHeadroomScope(subject, tenant contracts.PolicyLimit) string {
	if subject.IsUnlimited() && tenant.IsUnlimited() {
		return "none"
	}
	if subject == tenant {
		return "tenant_and_subject"
	}
	if tenant.IsUnlimited() || !subject.IsUnlimited() && subject < tenant {
		return "subject"
	}
	return "tenant"
}
