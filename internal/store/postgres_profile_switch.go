package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// SwitchSandboxProfile repins one stopped, Snapshot-free Sandbox to the target
// Profile's current revision. The Sandbox identity, Workspace, home Runner,
// resources, and egress-context pin are unchanged; the next start builds its
// Assignment from the new revision. A Sandbox already pinned to that revision is
// returned unchanged. The bool reports an idempotent replay.
func (store *PostgresControlPlaneStore) SwitchSandboxProfile(
	ctx context.Context,
	input ports.SwitchSandboxProfileInput,
) (contracts.Sandbox, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
	); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch quota lock failed: %w", err)
	}
	lockKey := input.Principal.TenantRef + "\x1f" + input.Principal.SubjectRef +
		"\x1fswitch-profile\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch idempotency lock failed: %w", err)
	}
	var priorHash string
	var expiresAt time.Time
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,expires_at FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='sandbox.switch_profile'
		  AND target_id=$3 AND idempotency_key=$4`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &expiresAt)
	if idempotencyErr == nil {
		expired, err := deleteExpiredIdempotencyRecord(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
			"sandbox.switch_profile", input.SandboxID, input.IdempotencyKey, expiresAt, input.Now,
		)
		if err != nil {
			return contracts.Sandbox{}, false, fmt.Errorf("SecondBox expired Sandbox Profile switch idempotency cleanup failed: %w", err)
		}
		if expired {
			idempotencyErr = pgx.ErrNoRows
		} else {
			if priorHash != input.RequestHash {
				return contracts.Sandbox{}, false, ports.ErrIdempotencyConflict
			}
			sandbox, err := getSandboxWithQuerier(
				ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
			)
			if err != nil {
				return contracts.Sandbox{}, false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch replay commit failed: %w", err)
			}
			return sandbox, true, nil
		}
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch idempotency lookup failed: %w", idempotencyErr)
	}
	locked, err := lockSandboxWorkspace(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
	)
	if err != nil {
		return contracts.Sandbox{}, false, err
	}
	if locked.Revision != input.ExpectedRevision {
		return contracts.Sandbox{}, false, ports.ErrRevisionConflict
	}
	var currentProfile string
	var currentSpecJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT sandbox.profile_name,revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1`,
		locked.SandboxID,
	).Scan(&currentProfile, &currentSpecJSON); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch pinned revision lookup failed: %w", err)
	}
	// Both Profiles are capability boundaries: the caller must hold the one it
	// leaves as well as the one it enters, evaluated against this locked pin.
	if input.ProfileGrants != nil &&
		(!contains(input.ProfileGrants, currentProfile) || !contains(input.ProfileGrants, input.Profile)) {
		return contracts.Sandbox{}, false, ports.ErrAuthorizationDenied
	}
	target, err := scanProfile(tx.QueryRow(ctx, profileSelect+`
		WHERE profile.name=$1 FOR SHARE OF profile`, input.Profile))
	if err != nil {
		return contracts.Sandbox{}, false, mapNotFound(err, ports.ErrProfileNotFound)
	}
	now := input.Now.UTC()
	if currentProfile == target.Name && locked.ProfileRevisionID == target.CurrentRevision.ID {
		// Converged: a retry after a lost response, or crash repair by a caller
		// that cannot tell whether its switch committed, changes nothing.
		if err := insertProfileSwitchIdempotency(ctx, tx, input, now); err != nil {
			return contracts.Sandbox{}, false, err
		}
		sandbox, err := getSandboxWithQuerier(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
		)
		if err != nil {
			return contracts.Sandbox{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch commit failed: %w", err)
		}
		return sandbox, false, nil
	}
	if target.State != contracts.ProfileStateEnabled {
		return contracts.Sandbox{}, false, ports.ErrProfileDisabled
	}
	if locked.SandboxState != contracts.SandboxStateStopped ||
		locked.DesiredState != contracts.SandboxDesiredStateStopped ||
		locked.CurrentInstanceID != "" {
		return contracts.Sandbox{}, false, ports.ErrProfileSwitchSandboxNotStopped
	}
	workspace := locked.Workspace
	if workspace.State != "ready" || workspace.Generation != locked.Generation {
		return contracts.Sandbox{}, false, ports.ErrGenerationFenced
	}
	if workspace.Mutation.State != "" {
		return contracts.Sandbox{}, false, ports.ErrWorkspaceMutation
	}
	var snapshotsPresent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.snapshots
			WHERE sandbox_id=$1 AND workspace_id=$2 AND state<>'deleted'
		)`,
		locked.SandboxID, locked.WorkspaceID,
	).Scan(&snapshotsPresent); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch Snapshot lookup failed: %w", err)
	}
	if snapshotsPresent {
		return contracts.Sandbox{}, false, ports.ErrProfileSwitchSnapshotsPresent
	}
	var current contracts.ProfileRevisionSpec
	if err := json.Unmarshal(currentSpecJSON, &current); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch pinned revision decoding failed: %w", err)
	}
	targetSpec := target.CurrentRevision.Spec
	if err := profileSwitchCompatible(current, targetSpec, locked.Resources); err != nil {
		return contracts.Sandbox{}, false, err
	}
	selection, err := readSubjectSandboxSelection(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, target.Name,
	)
	if err != nil {
		return contracts.Sandbox{}, false, err
	}
	var selectedLifecycle *contracts.SandboxLifecycleLimits
	if selection != nil {
		selectedLifecycle = &selection.Lifecycle
	}
	// Lifecycle resolves exactly as it would for a Sandbox created under the
	// target revision, so the switch carries no policy from the left Profile.
	resolvedLifecycle, err := targetSpec.ResolveLifecycle(selectedLifecycle)
	if err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("%w: %w", ports.ErrProfilePolicyCeilingExceeded, err)
	}
	lifecycleJSON, err := json.Marshal(resolvedLifecycle)
	if err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch lifecycle encoding failed: %w", err)
	}
	if err := ensureHomeRunnerServesProfile(ctx, tx, workspace.HomeRunnerID, targetSpec); err != nil {
		return contracts.Sandbox{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET profile_name=$2,profile_revision_id=$3,lifecycle_policy_json=$4,
		    revision=revision+1,updated_at=$5
		WHERE id=$1`,
		locked.SandboxID, target.Name, target.CurrentRevision.ID, lifecycleJSON, now,
	); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch update failed: %w", err)
	}
	audit := input.AuditEvent
	audit.Details = map[string]string{
		"fromProfile": currentProfile, "fromProfileRevisionId": locked.ProfileRevisionID,
		"toProfile": target.Name, "toProfileRevisionId": target.CurrentRevision.ID,
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Sandbox{}, false, err
	}
	if err := insertProfileSwitchIdempotency(ctx, tx, input, now); err != nil {
		return contracts.Sandbox{}, false, err
	}
	sandbox, err := getSandboxWithQuerier(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
	)
	if err != nil {
		return contracts.Sandbox{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Sandbox{}, false, fmt.Errorf("SecondBox Sandbox Profile switch commit failed: %w", err)
	}
	return sandbox, false, nil
}

func insertProfileSwitchIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	input ports.SwitchSandboxProfileInput,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'sandbox.switch_profile',$3,$4,$5,$3,$6,$7)`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		input.SandboxID, input.IdempotencyKey, input.RequestHash,
		now, input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Sandbox Profile switch idempotency insert failed: %w", err)
	}
	return nil
}

// profileSwitchCompatible admits a target revision only when the Sandbox's
// existing placement, startup, egress pin, and resources stay valid under it.
func profileSwitchCompatible(
	current contracts.ProfileRevisionSpec,
	target contracts.ProfileRevisionSpec,
	resources contracts.SandboxResources,
) error {
	if target.Pool != current.Pool {
		return &ports.ProfileIncompatibleError{Property: "pool", Reason: "must equal the Sandbox's RunnerPool " + current.Pool}
	}
	if target.Architecture != current.Architecture {
		return &ports.ProfileIncompatibleError{Property: "architecture", Reason: "must equal the Sandbox's architecture " + current.Architecture}
	}
	if target.Startup.Mode != current.Startup.Mode {
		return &ports.ProfileIncompatibleError{Property: "startup.mode", Reason: "must equal the Sandbox's startup mode " + current.Startup.Mode}
	}
	if current.Network.RequiresTenantEgressContext == nil || target.Network.RequiresTenantEgressContext == nil {
		return errors.New("SecondBox Profile egress-context requirement is absent")
	}
	if *target.Network.RequiresTenantEgressContext != *current.Network.RequiresTenantEgressContext {
		return &ports.ProfileIncompatibleError{Property: "network.requiresTenantEgressContext", Reason: "must equal the Sandbox's egress-context requirement"}
	}
	if !sandboxResourcesFitProfile(target, resources) {
		return &ports.ProfileIncompatibleError{Property: "resources", Reason: "must admit the Sandbox's pinned vcpuCount, memoryBytes, and workspaceBytes"}
	}
	return nil
}

// ensureHomeRunnerServesProfile checks the home Runner's advertised
// capabilities against what an Assignment of the target revision requires. A
// Sandbox never leaves its home, so a missing capability is incompatibility.
func ensureHomeRunnerServesProfile(
	ctx context.Context,
	tx pgx.Tx,
	homeRunnerID string,
	spec contracts.ProfileRevisionSpec,
) error {
	var capabilitiesJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT capabilities_json FROM secondbox.runners WHERE id=$1 FOR SHARE`,
		homeRunnerID,
	).Scan(&capabilitiesJSON); err != nil {
		return mapNotFound(err, ports.ErrHomeRunnerUnavailable)
	}
	var capabilities []string
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return fmt.Errorf("SecondBox Sandbox Profile switch home Runner capabilities decoding failed: %w", err)
	}
	required := []string{}
	if spec.AttributedExecution != nil {
		required = append(required, contracts.RunnerCapabilityPerExecAttribution)
	}
	if spec.Startup.Mode == contracts.StartupModeSnapshotResume {
		required = append(required, contracts.RunnerCapabilitySnapshotResume)
	}
	for _, capability := range required {
		if !contains(capabilities, capability) {
			return &ports.ProfileIncompatibleError{Property: "homeRunner", Reason: "does not advertise " + capability}
		}
	}
	return nil
}
