package rowlock

import (
	"context"
	"errors"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// ErrQuotaLedgerRequired reports that a Sandbox locked without its quota
// ledgers turned out to be in a state from which the caller's transition
// increases quota usage. The caller rolls back and retries with the ledgers.
var ErrQuotaLedgerRequired = errors.New("SecondBox Sandbox transition increases quota usage and requires its quota ledger lock")

// QuotaTransition reports, for a Sandbox's current state and desired state,
// whether the caller's transition increases the quota usage admission counts.
type QuotaTransition func(state, desiredState string) bool

// NonIncreasingTransition is the QuotaTransition of a caller whose writes never
// increase quota usage.
func NonIncreasingTransition(string, string) bool { return false }

// AlwaysQuotaLedger is the QuotaTransition of a caller that holds the ledgers
// unconditionally, such as a retry after ErrQuotaLedgerRequired.
func AlwaysQuotaLedger(string, string) bool { return true }

// QuotaUsageActive mirrors secondbox.sandbox_quota_active: the Sandbox holds a
// compute reservation (active Instance, CPU, and memory) in quota usage.
func QuotaUsageActive(state, desiredState string) bool {
	switch state {
	case contracts.SandboxStateStarting, contracts.SandboxStateReady,
		contracts.SandboxStateDraining, contracts.SandboxStateStopping:
		return true
	case contracts.SandboxStateCreating, contracts.SandboxStateStopped:
		return desiredState == contracts.SandboxDesiredStateRunning
	default:
		return false
	}
}

// QuotaUsageIncreases mirrors the sandboxes_quota_ledger_lock trigger: moving a
// Sandbox from one state pair to another adds to counted quota usage.
func QuotaUsageIncreases(fromState, fromDesired, toState, toDesired string) bool {
	if fromState == contracts.SandboxStateDeleted && toState != contracts.SandboxStateDeleted {
		return true
	}
	return QuotaUsageActive(toState, toDesired) && !QuotaUsageActive(fromState, fromDesired)
}

// SandboxWorkspaceForTransition locks one Sandbox and its Workspace for a
// transition, taking the quota ledgers first only when that transition can
// increase quota usage. Admission counts usage under the ledgers, so only an
// increase has to serialize with it; any other transition can at worst make a
// concurrent admission count conservatively.
//
// The ledger decision reads the Sandbox without a lock and is confirmed once
// the row is locked. A Sandbox locked without its ledgers is recorded for the
// rest of the transaction, and the quota ledger trigger rejects any later write
// that increases its usage, because taking the ledgers after the Sandbox row
// would invert the lock order.
func SandboxWorkspaceForTransition(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
	increases QuotaTransition,
) (SandboxWorkspace, error) {
	var tenantRef, subjectRef, state, desiredState string
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref,subject_ref,state,desired_state FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&tenantRef, &subjectRef, &state, &desiredState); err != nil {
		return SandboxWorkspace{}, err
	}
	withLedger := increases(state, desiredState)
	if withLedger {
		if err := TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
			return SandboxWorkspace{}, err
		}
	}
	var locked SandboxWorkspace
	locked.SandboxID = sandboxID
	var marker string
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref,subject_ref,workspace_id,profile_revision_id,egress_context,state,desired_state,generation,revision,
		       current_instance_id,COALESCE(reconcile_owner,''),vcpu_count,memory_bytes,workspace_bytes,
		       CASE WHEN $4 THEN '' ELSE set_config(
		         'secondbox.ledgerless_sandboxes',
		         COALESCE(NULLIF(current_setting('secondbox.ledgerless_sandboxes', true),''),',')||id||',',
		         true
		       ) END
		FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		FOR UPDATE`,
		sandboxID, tenantRef, subjectRef, withLedger,
	).Scan(
		&locked.TenantRef, &locked.SubjectRef,
		&locked.WorkspaceID, &locked.ProfileRevisionID, &locked.EgressContext, &locked.SandboxState,
		&locked.DesiredState, &locked.Generation, &locked.Revision,
		&locked.CurrentInstanceID, &locked.ReconcileOwner,
		&locked.Resources.VCPUCount, &locked.Resources.MemoryBytes, &locked.Resources.WorkspaceBytes,
		&marker,
	); err != nil {
		return SandboxWorkspace{}, err
	}
	if !withLedger && increases(locked.SandboxState, locked.DesiredState) {
		return SandboxWorkspace{}, ErrQuotaLedgerRequired
	}
	if locked.EgressContext != nil {
		if err := contracts.ValidateEgressContextName(*locked.EgressContext); err != nil {
			return SandboxWorkspace{}, fmt.Errorf("SecondBox persisted Sandbox egress context is invalid: %w", err)
		}
	}
	workspace, err := lockWorkspace(ctx, tx, locked.WorkspaceID, sandboxID)
	if err != nil {
		return SandboxWorkspace{}, err
	}
	locked.Workspace = workspace
	return locked, nil
}

// SandboxesInOrder locks several Sandboxes in ascending identity order. A
// transaction that holds more than one Sandbox row lock takes them in this
// order, so two such transactions cannot wait on each other's Sandboxes
// whether or not they also hold the quota ledgers.
func SandboxesInOrder(ctx context.Context, tx pgx.Tx, sandboxIDs []string) error {
	if len(sandboxIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		SELECT id FROM secondbox.sandboxes
		WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE`, sandboxIDs,
	); err != nil {
		return fmt.Errorf("SecondBox ordered Sandbox lock failed: %w", err)
	}
	return nil
}
