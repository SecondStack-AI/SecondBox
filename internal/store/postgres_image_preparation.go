package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (store *PostgresControlPlaneStore) PrepareImage(ctx context.Context, input ports.ImagePreparationInput) (contracts.Operation, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	defer tx.Rollback(ctx)
	var replay contracts.Operation
	_, found, err := lookupAdminIdempotency(ctx, tx, input.Idempotency, &replay)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	if found {
		operation, err := getOperationWithQuerier(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, "id=$3", replay.ID)
		if err != nil {
			return contracts.Operation{}, false, err
		}
		return operation, true, tx.Commit(ctx)
	}
	now := input.Operation.CreatedAt
	tenantQuota, subjectQuota, err := lockTenantAndSubjectQuotaForAdmission(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, now)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	tenantUsage, err := readTenantQuotaUsage(ctx, tx, input.Principal.TenantRef, now)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	subjectUsage, err := readSubjectQuotaUsage(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, now)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	if !tenantQuota.MaxConcurrentOperations.Allows(tenantUsage.ConcurrentOperations+1) || !subjectQuota.MaxConcurrentOperations.Allows(subjectUsage.concurrentOperations+1) {
		return contracts.Operation{}, false, ports.ErrQuotaExceeded
	}
	rows, err := tx.Query(ctx, `SELECT runner.id,jsonb_agg(DISTINCT revision.spec_json->>'architecture')
	 FROM secondbox.runners runner
	 JOIN secondbox.profile_revisions revision ON revision.spec_json->>'pool'=runner.pool_name
	 JOIN secondbox.profiles profile ON profile.current_revision_id=revision.id
	 JOIN secondbox.tenants tenant ON tenant.ref=$1
	 WHERE profile.state='enabled' AND tenant.allowed_profile_grants_json ? profile.name
	 AND ($2='' OR profile.name=$2) AND ($3::text[] IS NULL OR profile.name=ANY($3))
	 AND runner.state='ready' AND runner.drain_phase='active' AND runner.active_connection_id<>''
	 AND runner.capabilities_json ? 'client-selected-image'
	 AND runner.architectures_json ? (revision.spec_json->>'architecture')
	 GROUP BY runner.id ORDER BY runner.id LIMIT $4`, input.Principal.TenantRef, input.Request.Profile, input.ProfileGrants, imagepreparation.MaximumTargets+1)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	var targets []imagepreparation.Target
	for rows.Next() {
		var target imagepreparation.Target
		var architectures []byte
		if err := rows.Scan(&target.RunnerID, &architectures); err != nil {
			rows.Close()
			return contracts.Operation{}, false, err
		}
		if err := json.Unmarshal(architectures, &target.Architectures); err != nil {
			rows.Close()
			return contracts.Operation{}, false, err
		}
		targets = append(targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return contracts.Operation{}, false, err
	}
	if len(targets) == 0 {
		return contracts.Operation{}, false, ports.ErrRunnerPoolUnavailable
	}
	if len(targets) > imagepreparation.MaximumTargets {
		return contracts.Operation{}, false, fmt.Errorf("%w: SecondBox image preparation exceeds the target bound; select a narrower Profile", ports.ErrQuotaExceeded)
	}
	if err := insertOperation(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.Operation); err != nil {
		return contracts.Operation{}, false, err
	}
	if err := imagepreparation.Queue(ctx, tx, input.Operation.ID+"-resolve", input.Operation.ID, input.Principal.TenantRef, input.Request.Image.Reference, imagepreparation.ResolveTarget(targets).RunnerID, imagepreparation.Payload{Role: "resolve", Targets: targets}, now, now.Add(imagepreparation.Deadline)); err != nil {
		return contracts.Operation{}, false, err
	}
	if _, err := insertAdminIdempotency(ctx, tx, input.Idempotency, input.Operation); err != nil {
		return contracts.Operation{}, false, err
	}
	return input.Operation, false, tx.Commit(ctx)
}
