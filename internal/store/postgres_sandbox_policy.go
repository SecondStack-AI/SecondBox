package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func readSubjectSandboxSelection(ctx context.Context, tx pgx.Tx, tenantRef, subjectRef, profile string) (*contracts.SubjectSandboxPolicy, error) {
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
	if selection.Profile != profile {
		return nil, nil
	}
	return &selection, nil
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
	return contracts.SubjectSandboxPolicyObservation{
		SubjectRef: subjectRef, Revision: subject.Revision, Profile: profileName, ProfileRevisionID: profile.CurrentRevision.ID,
		Desired: selection, Effective: effective, Ceiling: spec.LifecycleCeilings(), Resources: spec.Resources, ResourceCeiling: spec.ResourceCeiling,
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
	// Lock the selected Profile head so validation and the effective response agree.
	profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+` WHERE profile.name=$1 FOR SHARE OF profile`, selection.Profile))
	if err != nil {
		return result, receipt, mapNotFound(err, ports.ErrProfileNotFound)
	}
	if profile.State != "enabled" {
		return result, receipt, ports.ErrProfileDisabled
	}
	if err := profile.CurrentRevision.Spec.ValidateLifecycleSelection(selection.Lifecycle); err != nil {
		return result, receipt, fmt.Errorf("%w: %w", ports.ErrProfilePolicyCeilingExceeded, err)
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return result, receipt, err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.subjects SET sandbox_policy_json=$3,revision=revision+1,updated_at=$4 WHERE tenant_ref=$1 AND ref=$2`, tenantRef, subjectRef, encoded, now.UTC()); err != nil {
		return result, receipt, err
	}
	result, err = readSubjectSandboxPolicy(ctx, tx, tenantRef, subjectRef, selection.Profile, now)
	if err != nil {
		return result, receipt, err
	}
	receipt, err = insertAdminIdempotency(ctx, tx, idempotency, result)
	if err != nil {
		return result, receipt, err
	}
	return result, receipt, tx.Commit(ctx)
}
