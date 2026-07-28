package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// AssignmentScheduler persists one fenced placement and runner command atomically.
type AssignmentScheduler interface {
	Schedule(context.Context, scheduler.ScheduleRequest) (scheduler.DurableAssignment, bool, error)
}

// ActiveSessionCanceller sends fenced cancellation to in-flight generation operations.
type ActiveSessionCanceller interface {
	CancelSandboxSessions(context.Context, string, int64, string, time.Time) (int64, error)
}

// EffectBrokerConfig contains explicit lifecycle effect bounds and artifact trust.
type EffectBrokerConfig struct {
	AssignmentClaimDuration time.Duration
	AssignmentDeadline      time.Duration
	HeartbeatTimeout        time.Duration
	RetryLimit              int64
	SerializationRetryLimit int
	AssetCatalog            SignedAssetCatalog
	SessionCanceller        ActiveSessionCanceller
	NewID                   func(string) string
	NewFencingToken         func() ([]byte, error)
}

// PostgresEffectBroker turns lifecycle decisions into durable scheduler and runner commands.
type PostgresEffectBroker struct {
	pool      *pgxpool.Pool
	scheduler AssignmentScheduler
	config    EffectBrokerConfig
}

// NewPostgresEffectBroker validates the durable effect composition.
func NewPostgresEffectBroker(
	ctx context.Context,
	databaseURL string,
	assignmentScheduler AssignmentScheduler,
	config EffectBrokerConfig,
) (*PostgresEffectBroker, error) {
	if databaseURL == "" || assignmentScheduler == nil ||
		config.AssignmentClaimDuration <= 0 || config.AssignmentDeadline <= 0 ||
		config.HeartbeatTimeout <= 0 || config.RetryLimit < 0 ||
		config.SerializationRetryLimit < 0 || config.AssetCatalog == nil ||
		config.SessionCanceller == nil || config.NewID == nil || config.NewFencingToken == nil {
		return nil, errors.New("SecondBox lifecycle effect broker requires database, scheduler, trust, identity, and retry bounds")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle effect PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox lifecycle effect PostgreSQL readiness: %w", err)
	}
	return &PostgresEffectBroker{pool: pool, scheduler: assignmentScheduler, config: config}, nil
}

func (broker *PostgresEffectBroker) Close() {
	broker.pool.Close()
}

// ExecuteLifecycleEffect executes one restart-safe external-effect transition.
func (broker *PostgresEffectBroker) ExecuteLifecycleEffect(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	decision Decision,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	switch decision.Action {
	case ActionMaterialize, ActionStartInstance:
		return broker.materializeAndStart(ctx, claim, now, nextReconcileAt)
	case ActionStopInstance:
		return broker.queueStop(ctx, claim, decision.TerminationReason, now, nextReconcileAt)
	case ActionCheckpoint:
		return broker.queueCheckpoint(ctx, claim, now, nextReconcileAt)
	default:
		return fmt.Errorf("SecondBox lifecycle effect action %q is unsupported", decision.Action)
	}
}

func (broker *PostgresEffectBroker) queueCheckpoint(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	tx, err := broker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var (
		assignmentID, instanceID, runnerID, workspaceID, operationID, requestID string
		generation, activeSessions, workspaceBytes, retentionSeconds            int64
		fencingToken                                                            []byte
	)
	if err := tx.QueryRow(ctx, `
		SELECT assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.generation,assignment.fencing_token,sandbox.workspace_id,
		       (revision.spec_json->'resources'->>'workspaceBytes')::bigint,
		       (revision.spec_json->'checkpoint'->>'retentionSeconds')::bigint,
		       COALESCE((
		         SELECT operation.id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),''),
		       COALESCE((
		         SELECT operation.request_id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),''),
		       (
		         SELECT count(*) FROM secondbox.activity_sessions AS session
		         WHERE session.sandbox_id=sandbox.id AND session.generation=sandbox.generation
		           AND session.state='active'
		       )
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1 AND sandbox.reconcile_owner=$2 AND sandbox.revision=$3
		  AND assignment.generation=sandbox.generation AND assignment.state='ready'
		ORDER BY assignment.created_at DESC,assignment.id DESC LIMIT 1
		FOR UPDATE OF assignment,sandbox`,
		claim.SandboxID, claim.WorkerID, claim.Revision,
	).Scan(
		&assignmentID, &instanceID, &runnerID, &generation, &fencingToken,
		&workspaceID, &workspaceBytes, &retentionSeconds, &operationID, &requestID, &activeSessions,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.ErrRevisionConflict
		}
		return fmt.Errorf("SecondBox lifecycle checkpoint authority lookup failed: %w", err)
	}
	if activeSessions != 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET next_reconcile_at=$2,reconcile_owner='',reconcile_claim_expires_at=NULL,
			    revision=revision+1,updated_at=$3
			WHERE id=$1 AND revision=$4 AND reconcile_owner=$5`,
			claim.SandboxID, nextReconcileAt.UTC(), now.UTC(), claim.Revision, claim.WorkerID,
		); err != nil {
			return fmt.Errorf("SecondBox lifecycle checkpoint active-session wait failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("SecondBox lifecycle checkpoint active-session wait commit failed: %w", err)
		}
		if _, err := broker.config.SessionCanceller.CancelSandboxSessions(
			ctx, claim.SandboxID, generation, "Sandbox drain grace expired", now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox lifecycle forced session cancellation failed: %w", err)
		}
		return nil
	}
	generationText := fmt.Sprintf("%d", generation)
	effectID := stableEffectID("checkpoint-effect", claim.SandboxID, generationText)
	checkpointID := stableEffectID("checkpoint", claim.SandboxID, generationText)
	commandID := stableEffectID("checkpoint-command", claim.SandboxID, generationText)
	if (operationID == "") != (requestID == "") {
		return errors.New("SecondBox lifecycle checkpoint correlation is incomplete")
	}
	if operationID == "" && requestID == "" {
		// Policy-triggered checkpoints use stable effect identities when no client lifecycle operation exists.
		operationID = effectID
		requestID = commandID
	}
	storageObjectID := "checkpoints/" + claim.SandboxID + "/" + generationText + "/" + checkpointID + ".ext4"
	deadline := now.UTC().Add(broker.config.AssignmentDeadline)
	command := &runnerv1.CheckpointCommand{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: assignmentID, SandboxId: claim.SandboxID, InstanceId: instanceID,
			SandboxGeneration: uint64(generation), FencingToken: fencingToken,
		},
		CheckpointId: checkpointID, StorageObjectId: storageObjectID,
		MaximumSizeBytes: uint64(workspaceBytes), DeadlineUnixMs: uint64(deadline.UnixMilli()),
		Correlation: &runnerv1.Correlation{
			RequestId: requestID, OperationId: operationID, SandboxId: claim.SandboxID, InstanceId: instanceID,
			SandboxGeneration: uint64(generation), AssignmentId: assignmentID, RunnerId: runnerID,
		},
	}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Checkpoint{Checkpoint: command},
	})
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle Checkpoint command encoding failed: %w", err)
	}
	effectPayload, err := json.Marshal(map[string]any{
		"workspaceId": workspaceID, "retainUntil": now.UTC().Add(time.Duration(retentionSeconds) * time.Second),
		"maximumSizeBytes": workspaceBytes,
	})
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint effect encoding failed: %w", err)
	}
	handled, err := broker.resumeCheckpointEffect(
		ctx, tx, claim, effectID, commandID, runnerID, assignmentID,
		payload, deadline, now.UTC(), nextReconcileAt.UTC(),
	)
	if err != nil {
		return err
	}
	if handled {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,checkpoint_id,storage_object_id,fencing_token,retry_count,retry_limit,
			effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
			payload_json,evidence_json,created_at,updated_at
		) VALUES (
			$1,$2,$3,'checkpoint','queued',$4,$5,$6,$7,$8,$9,$10,0,$11,$12,'',$13,
			'','',$14,'{}',$15,$15
		) ON CONFLICT (id) DO NOTHING`,
		effectID, claim.SandboxID, generation, assignmentID, instanceID, runnerID,
		commandID, checkpointID, storageObjectID, fencingToken, broker.config.RetryLimit,
		deadline, now.UTC(), effectPayload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint effect insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'checkpoint',$4,'pending','',0,$5,$5,NULL)
		ON CONFLICT (id) DO NOTHING`,
		commandID, runnerID, assignmentID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox lifecycle Checkpoint command insert failed: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state='checkpointing',lifecycle_action='checkpoint',next_reconcile_at=$2,
		    reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$3
		WHERE id=$1 AND revision=$4 AND reconcile_owner=$5`,
		claim.SandboxID, nextReconcileAt.UTC(), now.UTC(), claim.Revision, claim.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint transition failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrRevisionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint commit failed: %w", err)
	}
	return nil
}

func (broker *PostgresEffectBroker) resumeCheckpointEffect(
	ctx context.Context,
	tx pgx.Tx,
	claim ports.LifecycleReconcileClaim,
	effectID string,
	initialCommandID string,
	runnerID string,
	assignmentID string,
	commandPayload []byte,
	nextDeadline time.Time,
	now time.Time,
	nextReconcileAt time.Time,
) (bool, error) {
	var state string
	var retryCount, retryLimit int64
	var effectDeadline time.Time
	err := tx.QueryRow(ctx, `
		SELECT state,retry_count,retry_limit,effect_deadline
		FROM secondbox.lifecycle_effects WHERE id=$1 FOR UPDATE`,
		effectID,
	).Scan(&state, &retryCount, &retryLimit, &effectDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("SecondBox lifecycle checkpoint retry lookup failed: %w", err)
	}
	if state != "queued" || effectDeadline.After(now) {
		if err := releaseCheckpointReconcileClaim(
			ctx, tx, claim, now, nextReconcileAt,
		); err != nil {
			return false, err
		}
		return true, nil
	}
	var currentCommandID string
	if err := tx.QueryRow(ctx, `
		SELECT command_id FROM secondbox.lifecycle_effects WHERE id=$1`,
		effectID,
	).Scan(&currentCommandID); err != nil {
		return false, fmt.Errorf("SecondBox lifecycle checkpoint current command lookup failed: %w", err)
	}
	if retryCount >= retryLimit {
		const failureMessage = "checkpoint command exhausted its delivery deadline retry bound"
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.lifecycle_effects
			SET state='runner_failed',failure_class='checkpoint_retry_exhausted',
			    failure_message=$2,updated_at=$3
			WHERE id=$1 AND state='queued' AND retry_count=$4`,
			effectID, failureMessage, now, retryCount,
		)
		if err != nil {
			return false, fmt.Errorf("SecondBox lifecycle checkpoint retry exhaustion failed: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return false, ports.ErrRevisionConflict
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='failed',target_connection_id='',updated_at=$2
			WHERE id=$1 AND state IN ('pending','delivering','delivered')`,
			currentCommandID, now,
		)
		if err != nil {
			return false, fmt.Errorf("SecondBox lifecycle checkpoint exhausted command failed: %w", err)
		}
		if commandTag.RowsAffected() != 1 {
			return false, errors.New("SecondBox lifecycle checkpoint exhausted command is missing")
		}
		if err := releaseCheckpointReconcileClaim(
			ctx, tx, claim, now, nextReconcileAt,
		); err != nil {
			return false, err
		}
		return true, nil
	}
	nextRetryCount := retryCount + 1
	retryCommandID := stableEffectID(
		initialCommandID, fmt.Sprintf("retry-%d", nextRetryCount),
	)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='expired',target_connection_id='',updated_at=$2
		WHERE id=$1 AND state IN ('pending','delivering','delivered')`,
		currentCommandID, now,
	); err != nil {
		return false, fmt.Errorf("SecondBox lifecycle checkpoint expired command update failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'checkpoint',$4,'pending','',0,$5,$5,NULL)`,
		retryCommandID, runnerID, assignmentID, commandPayload, now,
	); err != nil {
		return false, fmt.Errorf("SecondBox lifecycle checkpoint retry command insert failed: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET command_id=$2,retry_count=$3,effect_deadline=$4,
		    failure_class='checkpoint_deadline_retry',
		    failure_message='checkpoint command delivery deadline expired; retry queued',
		    updated_at=$5
		WHERE id=$1 AND state='queued' AND retry_count=$6`,
		effectID, retryCommandID, nextRetryCount, nextDeadline, now, retryCount,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox lifecycle checkpoint retry evidence update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, ports.ErrRevisionConflict
	}
	if err := releaseCheckpointReconcileClaim(
		ctx, tx, claim, now, nextReconcileAt,
	); err != nil {
		return false, err
	}
	return true, nil
}

func releaseCheckpointReconcileClaim(
	ctx context.Context,
	tx pgx.Tx,
	claim ports.LifecycleReconcileClaim,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=$2,reconcile_owner='',reconcile_claim_expires_at=NULL,
		    revision=revision+1,updated_at=$3
		WHERE id=$1 AND revision=$4 AND reconcile_owner=$5`,
		claim.SandboxID, nextReconcileAt, now, claim.Revision, claim.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle checkpoint reconcile release failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrRevisionConflict
	}
	return nil
}

type materializePlan struct {
	workspaceID        string
	generation         int64
	profileRevisionID  string
	sourceCheckpointID string
	operationID        string
	requestID          string
	spec               contracts.ProfileRevisionSpec
}

func (broker *PostgresEffectBroker) materializeAndStart(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	plan, err := broker.loadMaterializePlan(ctx, claim)
	if err != nil {
		return err
	}
	assignmentID := broker.config.NewID("assignment")
	instanceID := broker.config.NewID("instance")
	materializationID := broker.config.NewID("materialization")
	commandID := broker.config.NewID("assignment-command")
	fencingToken, err := broker.config.NewFencingToken()
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle fencing token generation failed: %w", err)
	}
	if len(fencingToken) < 32 {
		return errors.New("SecondBox lifecycle fencing token must contain at least 32 bytes")
	}
	networkPolicy, err := assignmentNetworkPolicy(plan.spec.Network)
	if err != nil {
		return err
	}
	assets, guestProtocolGeneration, err := resolveProfileAssets(
		broker.config.AssetCatalog, plan.spec,
	)
	if err != nil {
		return err
	}
	requiredCapabilities := []string{"network-policy", "storage", "cleanup"}
	if plan.sourceCheckpointID != "" || plan.spec.Checkpoint.OnStop {
		requiredCapabilities = append(requiredCapabilities, "checkpoint")
	}
	deadline := now.UTC().Add(broker.config.AssignmentDeadline)
	assignmentCommand := &runnerv1.AssignmentCommand{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: assignmentID, SandboxId: claim.SandboxID, InstanceId: instanceID,
			SandboxGeneration: uint64(plan.generation), FencingToken: fencingToken,
		},
		ProfileRevisionId: plan.profileRevisionID,
		Requirements: &runnerv1.ProfileRequirements{
			VcpuCount:            uint32((plan.spec.Resources.CPUMillis + 999) / 1000),
			MemoryBytes:          uint64(plan.spec.Resources.MemoryBytes),
			DiskBytes:            uint64(plan.spec.Resources.WorkspaceBytes),
			Architecture:         plan.spec.Architecture,
			RequiredCapabilities: requiredCapabilities,
			MaximumOperationMs:   uint64(plan.spec.Execution.MaximumDeadlineMilliseconds),
			MaximumOutputBytes:   uint64(plan.spec.Execution.MaximumBufferedOutputBytes),
		},
		Assets: assets, SourceCheckpointId: plan.sourceCheckpointID,
		DeadlineUnixMs: uint64(deadline.UnixMilli()),
		Correlation: &runnerv1.Correlation{
			RequestId: plan.requestID, OperationId: plan.operationID, SandboxId: claim.SandboxID,
			SandboxGeneration: uint64(plan.generation),
		},
		NetworkPolicy: networkPolicy,
	}
	assignment, _, err := broker.scheduler.Schedule(ctx, scheduler.ScheduleRequest{
		AssignmentID: assignmentID, AssignmentCommandID: commandID,
		InstanceID: instanceID, SandboxID: claim.SandboxID,
		WorkspaceID: plan.workspaceID, MaterializationID: materializationID,
		SourceCheckpointID: plan.sourceCheckpointID, ProfileRevisionID: plan.profileRevisionID,
		Requirements: scheduler.Requirements{
			PoolName: plan.spec.Pool, BackendKind: plan.spec.Backend,
			Architecture: plan.spec.Architecture, RequiredCapabilities: requiredCapabilities,
			Capacity: scheduler.Capacity{
				CPUMillis:   plan.spec.Resources.CPUMillis,
				MemoryBytes: plan.spec.Resources.MemoryBytes,
				DiskBytes:   plan.spec.Resources.WorkspaceBytes,
				Instances:   1, Operations: plan.spec.Resources.ConcurrentOperations,
			},
			GuestProtocolGeneration: guestProtocolGeneration,
			WorkspaceCheckpointID:   plan.sourceCheckpointID,
			PreferredArtifactDigests: []string{
				plan.spec.RuntimeBundleDigest, plan.spec.ToolchainBundleDigest,
			},
		},
		AssignmentCommand: assignmentCommand, FencingToken: fencingToken,
		ResolvedArtifacts: map[string]string{
			"runtime": plan.spec.RuntimeBundleDigest, "toolchain": plan.spec.ToolchainBundleDigest,
		},
		ClaimExpiresAt:    now.UTC().Add(broker.config.AssignmentClaimDuration),
		OperationDeadline: deadline, RetryLimit: broker.config.RetryLimit,
		SerializationRetryLimit: broker.config.SerializationRetryLimit,
		HeartbeatTimeout:        broker.config.HeartbeatTimeout, Now: now.UTC(),
	})
	if err != nil {
		return err
	}
	tag, err := broker.pool.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET lifecycle_action='materialize',next_reconcile_at=$4,reconcile_owner='',
		    reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$3
		WHERE id=$1 AND generation=$2 AND current_instance_id=$5
		  AND reconcile_owner=$6`,
		claim.SandboxID, plan.generation, now.UTC(), nextReconcileAt.UTC(),
		assignment.InstanceID, claim.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle materialize claim completion failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var currentInstanceID string
		if err := broker.pool.QueryRow(ctx, `
			SELECT current_instance_id FROM secondbox.sandboxes
			WHERE id=$1 AND generation=$2`, claim.SandboxID, plan.generation,
		).Scan(&currentInstanceID); err != nil {
			return fmt.Errorf("SecondBox lifecycle materialize completion lookup failed: %w", err)
		}
		if currentInstanceID != assignment.InstanceID {
			return ports.ErrRevisionConflict
		}
	}
	return nil
}

func resolveProfileAssets(
	catalog SignedAssetCatalog,
	spec contracts.ProfileRevisionSpec,
) ([]*runnerv1.SignedAssetReference, uint32, error) {
	runtimeAsset, err := catalog.Resolve(spec.RuntimeBundleDigest)
	if err != nil {
		return nil, 0, err
	}
	toolchainAsset, err := catalog.Resolve(spec.ToolchainBundleDigest)
	if err != nil {
		return nil, 0, err
	}
	if runtimeAsset.ManifestDigest != spec.RuntimeBundleDigest ||
		toolchainAsset.ManifestDigest != spec.ToolchainBundleDigest ||
		runtimeAsset.Architecture != spec.Architecture ||
		toolchainAsset.Architecture != spec.Architecture ||
		runtimeAsset.GuestProtocolGeneration != toolchainAsset.GuestProtocolGeneration {
		return nil, 0, errors.New("SecondBox signed asset catalog is incompatible with the pinned Profile")
	}
	assets := []*runnerv1.SignedAssetReference{
		{
			ArtifactId: runtimeAsset.ArtifactID, ManifestDigest: runtimeAsset.ManifestDigest,
			SignatureKeyId: runtimeAsset.SignatureKeyID, Architecture: runtimeAsset.Architecture,
			GuestProtocolGeneration: runtimeAsset.GuestProtocolGeneration,
			MandatoryGuestFeatures:  append([]string(nil), runtimeAsset.MandatoryGuestFeatures...),
		},
		{
			ArtifactId: toolchainAsset.ArtifactID, ManifestDigest: toolchainAsset.ManifestDigest,
			SignatureKeyId: toolchainAsset.SignatureKeyID, Architecture: toolchainAsset.Architecture,
			GuestProtocolGeneration: toolchainAsset.GuestProtocolGeneration,
			MandatoryGuestFeatures:  append([]string(nil), toolchainAsset.MandatoryGuestFeatures...),
		},
	}
	return assets, runtimeAsset.GuestProtocolGeneration, nil
}

func (broker *PostgresEffectBroker) loadMaterializePlan(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
) (materializePlan, error) {
	var plan materializePlan
	var specJSON []byte
	err := broker.pool.QueryRow(ctx, `
		SELECT sandbox.workspace_id,sandbox.generation,sandbox.profile_revision_id,
		       revision.spec_json,
		       COALESCE((
		         SELECT checkpoint.id FROM secondbox.workspace_checkpoints AS checkpoint
		         WHERE checkpoint.id=workspace.current_checkpoint_id
		           AND checkpoint.state='published'
		       ),''),
		       COALESCE((
		         SELECT operation.id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),''),
		       COALESCE((
		         SELECT operation.request_id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),'')
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1 AND sandbox.reconcile_owner=$2`,
		claim.SandboxID, claim.WorkerID,
	).Scan(
		&plan.workspaceID, &plan.generation, &plan.profileRevisionID,
		&specJSON, &plan.sourceCheckpointID, &plan.operationID, &plan.requestID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return materializePlan{}, ports.ErrRevisionConflict
	}
	if err != nil {
		return materializePlan{}, fmt.Errorf("SecondBox lifecycle materialize plan lookup failed: %w", err)
	}
	if err := json.Unmarshal(specJSON, &plan.spec); err != nil {
		return materializePlan{}, fmt.Errorf("SecondBox lifecycle materialize Profile decoding failed: %w", err)
	}
	return plan, nil
}

func (broker *PostgresEffectBroker) queueStop(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	terminationReason string,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	tx, err := broker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle stop transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var assignmentID, instanceID, runnerID, operationID, requestID string
	var generation int64
	var fencingToken []byte
	if err := tx.QueryRow(ctx, `
		SELECT assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.generation,assignment.fencing_token,
		       COALESCE((
		         SELECT operation.id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),''),
		       COALESCE((
		         SELECT operation.request_id FROM secondbox.operations AS operation
		         WHERE operation.sandbox_id=sandbox.id
		           AND operation.state IN ('pending','running')
		         ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1
		       ),'')
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		WHERE sandbox.id=$1 AND sandbox.reconcile_owner=$2
		  AND sandbox.revision=$3 AND assignment.generation=sandbox.generation
		  AND assignment.state IN ('assigned','accepted','starting','ready','uncertain','fencing')
		ORDER BY assignment.created_at DESC,assignment.id DESC LIMIT 1
		FOR UPDATE OF assignment`,
		claim.SandboxID, claim.WorkerID, claim.Revision,
	).Scan(
		&assignmentID, &instanceID, &runnerID, &generation, &fencingToken,
		&operationID, &requestID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.ErrRevisionConflict
		}
		return fmt.Errorf("SecondBox lifecycle stop Assignment lookup failed: %w", err)
	}
	generationText := fmt.Sprintf("%d", generation)
	effectID := stableEffectID("stop-effect", claim.SandboxID, generationText)
	commandID := stableEffectID("stop-command", claim.SandboxID, generationText)
	deadline := now.UTC().Add(broker.config.AssignmentDeadline)
	command := &runnerv1.FenceCommand{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: assignmentID, SandboxId: claim.SandboxID, InstanceId: instanceID,
			SandboxGeneration: uint64(generation), FencingToken: fencingToken,
		},
		Reason:         runnerv1.FenceReason_FENCE_REASON_OPERATOR_REQUEST,
		DeadlineUnixMs: uint64(deadline.UnixMilli()),
		Correlation: &runnerv1.Correlation{
			RequestId: requestID, OperationId: operationID, SandboxId: claim.SandboxID,
			InstanceId: instanceID, SandboxGeneration: uint64(generation),
			AssignmentId: assignmentID, RunnerId: runnerID,
		},
	}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Fence{Fence: command},
	})
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle Fence command encoding failed: %w", err)
	}
	handled, err := broker.resumeStopEffect(
		ctx, tx, claim, effectID, commandID, runnerID, assignmentID,
		payload, deadline, now.UTC(), nextReconcileAt.UTC(),
	)
	if err != nil {
		return err
	}
	if handled {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,checkpoint_id,storage_object_id,fencing_token,retry_count,retry_limit,
			effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
			payload_json,evidence_json,created_at,updated_at
		) VALUES (
			$1,$2,$3,'stop','queued',$4,$5,$6,$7,'','',$8,0,$9,$10,'',$11,
			'','','{}','{}',$12,$12
		) ON CONFLICT (id) DO NOTHING`,
		effectID, claim.SandboxID, generation, assignmentID, instanceID, runnerID,
		commandID, fencingToken, broker.config.RetryLimit, deadline, now.UTC(), now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox lifecycle stop effect insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'fence',$4,'pending','',0,$5,$5,NULL)
		ON CONFLICT (id) DO NOTHING`,
		commandID, runnerID, assignmentID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox lifecycle Fence command insert failed: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state='stopping',lifecycle_action='stop_instance',
		    lifecycle_termination_reason=CASE WHEN $4='' THEN lifecycle_termination_reason ELSE $4 END,
		    next_reconcile_at=$5,reconcile_owner='',reconcile_claim_expires_at=NULL,
		    revision=revision+1,updated_at=$6
		WHERE id=$1 AND generation=$2 AND revision=$3 AND reconcile_owner=$7`,
		claim.SandboxID, generation, claim.Revision, terminationReason,
		nextReconcileAt.UTC(), now.UTC(), claim.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle stop transition failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrRevisionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox lifecycle stop commit failed: %w", err)
	}
	return nil
}

func (broker *PostgresEffectBroker) resumeStopEffect(
	ctx context.Context,
	tx pgx.Tx,
	claim ports.LifecycleReconcileClaim,
	effectID string,
	initialCommandID string,
	runnerID string,
	assignmentID string,
	commandPayload []byte,
	nextDeadline time.Time,
	now time.Time,
	nextReconcileAt time.Time,
) (bool, error) {
	var state, currentCommandID string
	var retryCount, retryLimit int64
	var effectDeadline time.Time
	err := tx.QueryRow(ctx, `
		SELECT state,command_id,retry_count,retry_limit,effect_deadline
		FROM secondbox.lifecycle_effects WHERE id=$1 FOR UPDATE`,
		effectID,
	).Scan(&state, &currentCommandID, &retryCount, &retryLimit, &effectDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("SecondBox lifecycle stop retry lookup failed: %w", err)
	}
	if state != "queued" || effectDeadline.After(now) {
		if err := releaseEffectReconcileClaim(
			ctx, tx, claim, now, nextReconcileAt, "stop",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	if retryCount >= retryLimit {
		const failureMessage = "stop command exhausted its delivery deadline retry bound"
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.lifecycle_effects
			SET state='runner_failed',failure_class='stop_retry_exhausted',
			    failure_message=$2,updated_at=$3
			WHERE id=$1 AND state='queued' AND retry_count=$4`,
			effectID, failureMessage, now, retryCount,
		)
		if err != nil {
			return false, fmt.Errorf("SecondBox lifecycle stop retry exhaustion failed: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return false, ports.ErrRevisionConflict
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='failed',target_connection_id='',updated_at=$2
			WHERE id=$1 AND state IN ('pending','delivering','delivered')`,
			currentCommandID, now,
		)
		if err != nil {
			return false, fmt.Errorf("SecondBox lifecycle stop exhausted command failed: %w", err)
		}
		if commandTag.RowsAffected() != 1 {
			return false, errors.New("SecondBox lifecycle stop exhausted command is missing")
		}
		if err := releaseEffectReconcileClaim(
			ctx, tx, claim, now, nextReconcileAt, "stop",
		); err != nil {
			return false, err
		}
		return true, nil
	}
	nextRetryCount := retryCount + 1
	retryCommandID := stableEffectID(
		initialCommandID, fmt.Sprintf("retry-%d", nextRetryCount),
	)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='expired',target_connection_id='',updated_at=$2
		WHERE id=$1 AND state IN ('pending','delivering','delivered')`,
		currentCommandID, now,
	); err != nil {
		return false, fmt.Errorf("SecondBox lifecycle stop expired command update failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'fence',$4,'pending','',0,$5,$5,NULL)`,
		retryCommandID, runnerID, assignmentID, commandPayload, now,
	); err != nil {
		return false, fmt.Errorf("SecondBox lifecycle stop retry command insert failed: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET command_id=$2,retry_count=$3,effect_deadline=$4,
		    failure_class='stop_deadline_retry',
		    failure_message='stop command delivery deadline expired; retry queued',
		    updated_at=$5
		WHERE id=$1 AND state='queued' AND retry_count=$6`,
		effectID, retryCommandID, nextRetryCount, nextDeadline, now, retryCount,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox lifecycle stop retry evidence update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, ports.ErrRevisionConflict
	}
	if err := releaseEffectReconcileClaim(
		ctx, tx, claim, now, nextReconcileAt, "stop",
	); err != nil {
		return false, err
	}
	return true, nil
}

func releaseEffectReconcileClaim(
	ctx context.Context,
	tx pgx.Tx,
	claim ports.LifecycleReconcileClaim,
	now time.Time,
	nextReconcileAt time.Time,
	effectKind string,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=$2,reconcile_owner='',reconcile_claim_expires_at=NULL,
		    revision=revision+1,updated_at=$3
		WHERE id=$1 AND revision=$4 AND reconcile_owner=$5`,
		claim.SandboxID, nextReconcileAt, now, claim.Revision, claim.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle %s reconcile release failed: %w", effectKind, err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrRevisionConflict
	}
	return nil
}

func assignmentNetworkPolicy(policy contracts.NetworkPolicy) (*runnerv1.NetworkPolicy, error) {
	resolved := &runnerv1.NetworkPolicy{}
	switch policy.Mode {
	case "deny_all":
		resolved.Mode = runnerv1.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL
	case "allow_list":
		resolved.Mode = runnerv1.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST
	default:
		return nil, errors.New("SecondBox lifecycle Profile network mode is invalid")
	}
	for _, destination := range policy.Destinations {
		item := &runnerv1.NetworkDestination{Port: uint32(destination.Port)}
		switch destination.Protocol {
		case "tcp":
			item.Protocol = runnerv1.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP
		case "http":
			item.Protocol = runnerv1.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTP
		case "https":
			item.Protocol = runnerv1.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS
		default:
			return nil, errors.New("SecondBox lifecycle Profile network protocol is invalid")
		}
		switch {
		case destination.Domain != "":
			item.Target = &runnerv1.NetworkDestination_Domain{Domain: destination.Domain}
		case destination.CIDR != "":
			item.Target = &runnerv1.NetworkDestination_Cidr{Cidr: destination.CIDR}
		default:
			return nil, errors.New("SecondBox lifecycle Profile network target is absent")
		}
		resolved.Destinations = append(resolved.Destinations, item)
	}
	return resolved, nil
}

func stableEffectID(kind string, components ...string) string {
	hasher := sha256.New()
	hasher.Write([]byte(kind))
	for _, component := range components {
		hasher.Write([]byte{0})
		hasher.Write([]byte(component))
	}
	return kind + "-" + hex.EncodeToString(hasher.Sum(nil))[:32]
}
