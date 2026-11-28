package store

import (
	"context"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// The ledger is the Sandbox row. This predicate also includes running intent
// waiting for placement and compute draining toward a stop.
const activeResourceAccountRows = ` FROM secondbox.sandboxes
 WHERE tenant_ref=$1 AND subject_ref=$2 AND resource_account_key=$3
 AND secondbox.sandbox_quota_active(state,desired_state,current_instance_id)`

func (store *PostgresControlPlaneStore) GetResourceAccountUsage(ctx context.Context, tenantRef, subjectRef, key string) (contracts.ResourceAccountUsage, error) {
	usage := contracts.ResourceAccountUsage{AccountKey: key, ObservedAt: time.Now().UTC(), Allocations: []contracts.ResourceAccountAllocation{}}
	rows, err := store.pool.Query(ctx, `SELECT id,state,desired_state,vcpu_count,memory_bytes`+activeResourceAccountRows+` ORDER BY id`, tenantRef, subjectRef, key)
	if err != nil {
		return usage, fmt.Errorf("SecondBox resource account usage query failed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var allocation contracts.ResourceAccountAllocation
		if err := rows.Scan(&allocation.SandboxID, &allocation.State, &allocation.DesiredState, &allocation.VCPUCount, &allocation.MemoryBytes); err != nil {
			return usage, err
		}
		usage.ActiveInstances++
		usage.VCPUCount += allocation.VCPUCount
		usage.MemoryBytes += allocation.MemoryBytes
		usage.Allocations = append(usage.Allocations, allocation)
	}
	return usage, rows.Err()
}

// Called with the Tenant and Subject quota ledgers locked, before placement.
func admitResourceAccount(ctx context.Context, tx pgx.Tx, principal contracts.Principal, required bool, account *contracts.ResourceAccount, resources contracts.SandboxResources) error {
	if account == nil {
		if required {
			return &ports.InvalidFieldError{Field: "resourceAccount", Reason: "is required by the Profile"}
		}
		return nil
	}
	var instances, cpu, memory int64
	if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(vcpu_count),0),COALESCE(sum(memory_bytes),0)`+activeResourceAccountRows, principal.TenantRef, principal.SubjectRef, account.Key).Scan(&instances, &cpu, &memory); err != nil {
		return fmt.Errorf("SecondBox resource account admission query failed: %w", err)
	}
	axes := []struct {
		name            string
		limit           contracts.PolicyLimit
		used, requested int64
	}{
		{"maxActiveInstances", account.Limits.MaxActiveInstances, instances, 1},
		{"maxVcpuCount", account.Limits.MaxVCPUCount, cpu, resources.VCPUCount},
		{"maxMemoryBytes", account.Limits.MaxMemoryBytes, memory, resources.MemoryBytes},
	}
	for _, axis := range axes {
		if !axis.limit.IsUnlimited() && axis.requested > int64(axis.limit)-axis.used {
			return &ports.ResourceAccountQuotaError{Refusal: contracts.ResourceAccountQuotaRefusal{AccountKey: account.Key, Axis: axis.name, Used: axis.used, Requested: axis.requested, Limit: int64(axis.limit)}}
		}
	}
	return nil
}
