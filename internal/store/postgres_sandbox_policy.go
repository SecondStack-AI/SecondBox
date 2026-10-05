package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func readStoredSubjectSandboxPolicy(ctx context.Context, tx pgx.Tx, tenantRef, subjectRef string) (*contracts.SubjectSandboxPolicy, error) {
	var encoded []byte
	if err := tx.QueryRow(ctx, `SELECT sandbox_policy_json FROM secondbox.subjects WHERE tenant_ref=$1 AND ref=$2`, tenantRef, subjectRef).Scan(&encoded); err != nil {
		return nil, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if len(encoded) == 0 {
		return nil, nil
	}
	var selection contracts.SubjectSandboxPolicy
	if err := json.Unmarshal(encoded, &selection); err != nil {
		return nil, fmt.Errorf("SecondBox Subject Sandbox policy decoding failed: %w", err)
	}
	return &selection, nil
}

// readSubjectSandboxSelection returns the Subject policy only when it names profile.
func readSubjectSandboxSelection(ctx context.Context, tx pgx.Tx, tenantRef, subjectRef, profile string) (*contracts.SubjectSandboxPolicy, error) {
	selection, err := readStoredSubjectSandboxPolicy(ctx, tx, tenantRef, subjectRef)
	if err != nil || selection == nil || !selection.Selects(profile) {
		return nil, err
	}
	return selection, nil
}

func readSubjectSandboxPolicy(ctx context.Context, tx pgx.Tx, tenantRef, subjectRef, profileName string, now time.Time) (contracts.SubjectSandboxPolicyObservation, error) {
	var result contracts.SubjectSandboxPolicyObservation
	subject, err := scanSubject(tx.QueryRow(ctx, subjectSelect+` WHERE tenant_ref=$1 AND ref=$2`, tenantRef, subjectRef))
	if err != nil {
		return result, mapNotFound(err, ports.ErrManagementNotFound)
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1`, tenantRef))
	if err != nil {
		return result, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if !contains(tenant.AllowedProfileGrants, profileName) {
		return result, ports.ErrGrantEscalationDenied
	}
	profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+` WHERE profile.name=$1`, profileName))
	if err != nil {
		return result, mapNotFound(err, ports.ErrProfileNotFound)
	}
	selection, err := readSubjectSandboxSelection(ctx, tx, tenantRef, subjectRef, profileName)
	if err != nil {
		return result, err
	}
	var limits *contracts.SandboxLifecycleLimits
	if selection != nil {
		limits = &selection.Lifecycle
	}
	effective, err := profile.CurrentRevision.Spec.ResolveLifecycle(limits)
	if err != nil {
		return result, fmt.Errorf("%w: %w", ports.ErrProfilePolicyCeilingExceeded, err)
	}
	spec := profile.CurrentRevision.Spec
	attributed, err := spec.AttributedConnectionGrant()
	if err != nil {
		return result, err
	}
	if attributed != nil {
		var requested *contracts.AttributedExecutionConnectionLimits
		if selection != nil {
			requested = selection.AttributedExecution
		}
		resolved, err := attributed.Resolve(requested)
		if err != nil {
			return result, err
		}
		attributed = &resolved
	}
	return contracts.SubjectSandboxPolicyObservation{
		SubjectRef: subjectRef, Revision: subject.Revision, Profile: profileName, ProfileRevisionID: profile.CurrentRevision.ID,
		AttributedExecution: attributed,
		Desired:             selection, Effective: effective, Ceiling: spec.LifecycleCeilings(), Resources: spec.Resources, ResourceCeiling: spec.ResourceCeiling,
		Execution: spec.Execution, Retention: spec.Retention, Quota: subject.Quota, TenantQuota: tenant.AggregateQuota, ObservedAt: now.UTC(),
	}, nil
}

func (store *PostgresControlPlaneStore) GetSubjectSandboxPolicy(ctx context.Context, tenantRef, subjectRef, profile string, now time.Time) (contracts.SubjectSandboxPolicyObservation, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return contracts.SubjectSandboxPolicyObservation{}, err
	}
	defer tx.Rollback(ctx)
	result, err := readSubjectSandboxPolicy(ctx, tx, tenantRef, subjectRef, profile, now)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (store *PostgresControlPlaneStore) UpdateSubjectSandboxPolicy(ctx context.Context, tenantRef, subjectRef string, selection contracts.SubjectSandboxPolicy, revision int64, now time.Time, idempotency ports.AdminIdempotencyInput) (contracts.SubjectSandboxPolicyObservation, ports.AdminIdempotencyResult, error) {
	var result contracts.SubjectSandboxPolicyObservation
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return result, ports.AdminIdempotencyResult{}, err
	}
	defer tx.Rollback(ctx)
	receipt, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &result)
	if err != nil {
		return result, receipt, err
	}
	if found {
		return result, receipt, tx.Commit(ctx)
	}
	if _, _, err := lockTenantAndSubjectQuotaForAdmission(ctx, tx, tenantRef, subjectRef, now); err != nil {
		return result, receipt, err
	}
	subject, err := scanSubject(tx.QueryRow(ctx, subjectSelect+` WHERE tenant_ref=$1 AND ref=$2 FOR UPDATE`, tenantRef, subjectRef))
	if err != nil {
		return result, receipt, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if subject.Revision != revision {
		return result, receipt, ports.ErrRevisionConflict
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1`, tenantRef))
	if err != nil {
		return result, receipt, mapNotFound(err, ports.ErrManagementNotFound)
	}
	previous, err := readStoredSubjectSandboxPolicy(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return result, receipt, err
	}
	if err := selection.Lifecycle.Validate(); err != nil {
		return result, receipt, errors.Join(&ports.InvalidFieldError{
			Field: "lifecycle", Reason: "must have null or positive representable idleSeconds and maximumDurationSeconds",
		}, err)
	}
	if selection.AttributedExecution != nil {
		if err := selection.AttributedExecution.Validate(); err != nil {
			return result, receipt, errors.Join(&ports.InvalidFieldError{
				Field: "attributedExecution.maximumConnections", Reason: "must be between 1 and 4096",
			}, err)
		}
	}
	// Lock every named Profile head in name order so validation and the
	// effective response agree and concurrent writers take locks consistently.
	for _, name := range slices.Sorted(slices.Values(selection.Profiles)) {
		if !contains(tenant.AllowedProfileGrants, name) {
			return result, receipt, ports.ErrGrantEscalationDenied
		}
		profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+` WHERE profile.name=$1 FOR SHARE OF profile`, name))
		if err != nil {
			return result, receipt, mapNotFound(err, ports.ErrProfileNotFound)
		}
		if profile.State != "enabled" {
			return result, receipt, ports.ErrProfileDisabled
		}
		var prior *contracts.SubjectSandboxPolicy
		if previous != nil && previous.Selects(name) {
			prior = previous
		}
		if err := validateSubjectSandboxSelection(profile.CurrentRevision.Spec, selection, prior); err != nil {
			return result, receipt, err
		}
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return result, receipt, err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.subjects SET sandbox_policy_json=$3,revision=revision+1,updated_at=$4 WHERE tenant_ref=$1 AND ref=$2`, tenantRef, subjectRef, encoded, now.UTC()); err != nil {
		return result, receipt, err
	}
	// The response observes the first named Profile; GET observes any other.
	result, err = readSubjectSandboxPolicy(ctx, tx, tenantRef, subjectRef, selection.Profiles[0], now)
	if err != nil {
		return result, receipt, err
	}
	receipt, err = insertAdminIdempotency(ctx, tx, idempotency, result)
	if err != nil {
		return result, receipt, err
	}
	return result, receipt, tx.Commit(ctx)
}

// validateSubjectSandboxSelection checks the selection against one named
// Profile's current grants. prior is the stored policy if it already named that
// Profile: a complete PUT can preserve its unchanged desired block after an
// operator tightens the grant. The other block remains editable; effective
// resolution still enforces current ceilings. A newly named Profile has no prior.
func validateSubjectSandboxSelection(spec contracts.ProfileRevisionSpec, selection contracts.SubjectSandboxPolicy, prior *contracts.SubjectSandboxPolicy) error {
	if prior == nil || prior.Lifecycle != selection.Lifecycle {
		if err := spec.ValidateLifecycleSelection(selection.Lifecycle); err != nil {
			return fmt.Errorf("%w: %w", ports.ErrProfilePolicyCeilingExceeded, err)
		}
	}
	if selection.AttributedExecution == nil ||
		prior != nil && prior.AttributedExecution != nil && *prior.AttributedExecution == *selection.AttributedExecution {
		return nil
	}
	grant, err := spec.AttributedConnectionGrant()
	if err != nil {
		return err
	}
	if grant == nil {
		return &ports.InvalidFieldError{Field: "attributedExecution", Reason: "requires every named Profile to permit attributed execution"}
	}
	if selection.AttributedExecution.MaximumConnections > grant.MaximumConnectionsCeiling {
		return fmt.Errorf("%w: SecondBox attributed execution maximumConnections exceeds Profile ceiling", ports.ErrProfilePolicyCeilingExceeded)
	}
	return nil
}
