package rowlock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// QuotaScope identifies one tenant and Subject quota-ledger pair.
type QuotaScope struct {
	TenantRef  string
	SubjectRef string
}

// TenantQuota locks the tenant-level quota ledger before any domain row.
func TenantQuota(ctx context.Context, tx pgx.Tx, tenantRef string) error {
	var lockedTenantRef string
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref FROM secondbox.tenant_quotas
		WHERE tenant_ref=$1 FOR UPDATE`, tenantRef,
	).Scan(&lockedTenantRef); err != nil {
		return fmt.Errorf("SecondBox Tenant quota ledger lock failed: %w", err)
	}
	return nil
}

// TenantAndSubjectQuota establishes the global PostgreSQL mutation lock order:
// tenant quota ledger, subject quota ledger, then domain rows such as Subject,
// ApplicationAuthority, Sandbox, Workspace, Snapshot, PortSession, or data-plane
// session. Trigger-backed quota accounting depends on every writer preserving
// this order before it locks or changes a quota-bearing domain row.
func TenantAndSubjectQuota(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
) error {
	if err := TenantQuota(ctx, tx, tenantRef); err != nil {
		return err
	}
	var lockedSubjectRef string
	if err := tx.QueryRow(ctx, `
		SELECT subject_ref FROM secondbox.subject_quotas
		WHERE tenant_ref=$1 AND subject_ref=$2 FOR UPDATE`, tenantRef, subjectRef,
	).Scan(&lockedSubjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject quota ledger lock failed: %w", err)
	}
	return nil
}

// ActiveSubject locks and validates the Subject after its quota ledger and
// before quota-bearing domain rows.
func ActiveSubject(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	now time.Time,
) error {
	var state string
	var expiresAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT state,expires_at FROM secondbox.subjects
		WHERE tenant_ref=$1 AND ref=$2 FOR UPDATE`, tenantRef, subjectRef,
	).Scan(&state, &expiresAt); errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrManagementNotFound
	} else if err != nil {
		return fmt.Errorf("SecondBox Subject admission lock failed: %w", err)
	}
	if state == contracts.SubjectStateExpired || expiresAt != nil && !expiresAt.After(now.UTC()) {
		return ports.ErrResourceExpired
	}
	if state != contracts.SubjectStateActive {
		return ports.ErrInvalidLifecycleTransition
	}
	return nil
}

// QuotaScopes locks all tenant ledgers first, then all Subject ledgers, with
// each level sorted by logical identity before any domain row is locked.
func QuotaScopes(ctx context.Context, tx pgx.Tx, scopes []QuotaScope) error {
	ordered := append([]QuotaScope(nil), scopes...)
	slices.SortFunc(ordered, func(left, right QuotaScope) int {
		if left.TenantRef != right.TenantRef {
			if left.TenantRef < right.TenantRef {
				return -1
			}
			return 1
		}
		if left.SubjectRef < right.SubjectRef {
			return -1
		}
		if left.SubjectRef > right.SubjectRef {
			return 1
		}
		return 0
	})
	previousTenant := ""
	for _, scope := range ordered {
		if scope.TenantRef == previousTenant {
			continue
		}
		if err := TenantQuota(ctx, tx, scope.TenantRef); err != nil {
			return err
		}
		previousTenant = scope.TenantRef
	}
	var previous QuotaScope
	for index, scope := range ordered {
		if index > 0 && scope == previous {
			continue
		}
		var lockedSubjectRef string
		if err := tx.QueryRow(ctx, `
			SELECT subject_ref FROM secondbox.subject_quotas
			WHERE tenant_ref=$1 AND subject_ref=$2 FOR UPDATE`,
			scope.TenantRef, scope.SubjectRef,
		).Scan(&lockedSubjectRef); err != nil {
			return fmt.Errorf("SecondBox Subject quota ledger lock failed: %w", err)
		}
		previous = scope
	}
	return nil
}

// SandboxQuota locks the quota ledgers for a Sandbox before locking the Sandbox itself.
func SandboxQuota(ctx context.Context, tx pgx.Tx, sandboxID string) error {
	return SandboxQuotas(ctx, tx, []string{sandboxID})
}

// SandboxQuotas resolves Sandbox ownership without row locks, then locks every
// quota-ledger pair before a caller acquires any durable domain-row lock.
func SandboxQuotas(ctx context.Context, tx pgx.Tx, sandboxIDs []string) error {
	if len(sandboxIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT tenant_ref,subject_ref FROM secondbox.sandboxes
		WHERE id=ANY($1::text[]) ORDER BY tenant_ref,subject_ref`, sandboxIDs)
	if err != nil {
		return fmt.Errorf("SecondBox Sandbox quota scope lookup failed: %w", err)
	}
	var scopes []QuotaScope
	for rows.Next() {
		var scope QuotaScope
		if err := rows.Scan(&scope.TenantRef, &scope.SubjectRef); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox Sandbox quota scope scan failed: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("SecondBox Sandbox quota scope rows failed: %w", err)
	}
	rows.Close()
	if len(scopes) == 0 {
		return pgx.ErrNoRows
	}
	return QuotaScopes(ctx, tx, scopes)
}
