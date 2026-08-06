package reconcile

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
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func retryAssignmentCommandID(assignmentID string, retryCount int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"SecondBox\x00assignment-retry\x00%s\x00%d", assignmentID, retryCount,
	)))
	return "assignment-retry-" + hex.EncodeToString(sum[:16])
}

var (
	ErrClaimLost        = errors.New("SecondBox reconciliation claim is no longer current")
	ErrFenceProofAbsent = errors.New("SecondBox reconciliation cannot advance without fence proof")
)

// PostgresStore coordinates restart-safe reconciliation workers.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// Claim is one revision-fenced bounded reconciliation work item.
type Claim struct {
	AssignmentID   string
	SandboxID      string
	InstanceID     string
	RunnerID       string
	WorkerID       string
	FencingToken   []byte
	Correlation    *runnerv1.Correlation
	Revision       int64
	State          AssignmentState
	ClaimExpiresAt time.Time
}

// NewPostgresStore connects the reconciler to PostgreSQL authority.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox reconcile PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox reconcile PostgreSQL readiness: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

func (store *PostgresStore) Close() {
	store.pool.Close()
}

// MarkExpiredRunners makes assignments uncertain without authorizing replacement.
func (store *PostgresStore) MarkExpiredRunners(
	ctx context.Context,
	heartbeatCutoff time.Time,
	now time.Time,
) (int64, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("SecondBox runner loss transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		UPDATE secondbox.runners AS runner
		SET state='offline',revision=revision+1,updated_at=$2
		WHERE runner.last_seen_at<$1
		  AND (
		    runner.state IN ('ready','draining','connected')
		    OR (
		      runner.state='offline'
		      AND EXISTS (
		        SELECT 1 FROM secondbox.assignments AS assignment
		        WHERE assignment.runner_id=runner.id
		          AND assignment.state IN ('assigned','accepted','starting','ready')
		      )
		    )
		  )
		RETURNING id`, heartbeatCutoff.UTC(), now.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("SecondBox expired Runner update: %w", err)
	}
	runnerIDs := make([]string, 0)
	for rows.Next() {
		var runnerID string
		if err := rows.Scan(&runnerID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("SecondBox expired Runner scan: %w", err)
		}
		runnerIDs = append(runnerIDs, runnerID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("SecondBox expired Runner iteration: %w", err)
	}
	for _, runnerID := range runnerIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='uncertain',failure_class='transient',next_reconcile_at=$2,
				revision=revision+1,updated_at=$2
			WHERE runner_id=$1 AND state IN ('assigned','accepted','starting','ready')`,
			runnerID, now.UTC(),
		); err != nil {
			return 0, fmt.Errorf("SecondBox lost Runner Assignment update: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("SecondBox runner loss commit: %w", err)
	}
	return int64(len(runnerIDs)), nil
}

// RequestRunnerDrain records the admission barrier before a Drain command is sent.
func (store *PostgresStore) RequestRunnerDrain(
	ctx context.Context,
	runnerID string,
	drainCommand *runnerv1.DrainCommand,
	now time.Time,
) error {
	if runnerID == "" || drainCommand == nil || drainCommand.MessageId == "" ||
		drainCommand.Mode == runnerv1.DrainMode_DRAIN_MODE_UNSPECIFIED ||
		drainCommand.DeadlineUnixMs == 0 {
		return errors.New("SecondBox Runner drain requires identity, mode, command, and deadline")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Runner drain transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runners
		SET state='draining',drain_phase='draining',revision=revision+1,updated_at=$2
		WHERE id=$1 AND state IN ('ready','connected')`, runnerID, now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox Runner drain barrier: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("SecondBox Runner drain barrier requires an active Runner")
	}
	commandID := drainCommand.MessageId
	queuedCommand := proto.Clone(drainCommand).(*runnerv1.DrainCommand)
	queuedCommand.MessageId = ""
	queuedCommand.Sequence = 0
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Drain{Drain: queuedCommand},
	})
	if err != nil {
		return fmt.Errorf("SecondBox Runner Drain command encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,'','drain',$3,'pending','',0,$4,$4,NULL)`,
		commandID, runnerID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Runner Drain command insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Runner drain commit: %w", err)
	}
	return nil
}

// ClaimNext leases one due assignment using SKIP LOCKED across control-plane replicas.
func (store *PostgresStore) ClaimNext(
	ctx context.Context,
	workerID string,
	claimExpiresAt time.Time,
	now time.Time,
) (Claim, bool, error) {
	if workerID == "" || !claimExpiresAt.After(now) {
		return Claim{}, false, errors.New("SecondBox reconciliation claim requires worker identity and future expiry")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation claim transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var claim Claim
	var failureClass string
	var deadline time.Time
	var retryCount, retryLimit int
	var releaseProofJSON []byte
	var assignmentCommandPayload []byte
	err = tx.QueryRow(ctx, `
		SELECT assignments.id,assignments.sandbox_id,assignments.instance_id,assignments.runner_id,
			assignments.fencing_token,assignments.state,assignments.generation,
			assignments.failure_class,assignments.retry_count,
			assignments.retry_limit,assignments.operation_deadline,
			assignments.release_proof_json,assignments.revision,
			(
			  SELECT command.payload FROM secondbox.runner_commands AS command
			  WHERE command.assignment_id=assignments.id AND command.kind='assignment'
			  ORDER BY command.created_at DESC,command.id DESC LIMIT 1
			)
		FROM secondbox.assignments
		JOIN secondbox.sandboxes
		  ON sandboxes.id=assignments.sandbox_id
		  AND sandboxes.current_instance_id=assignments.instance_id
		  AND sandboxes.generation=assignments.generation
		WHERE assignments.next_reconcile_at<=$1
			AND assignments.reconcile_claim_expires_at<=$1
			AND (
			  assignments.state IN ('assigned','accepted','starting','uncertain','failed','fencing')
			  OR (
			    assignments.state='fenced'
			    AND assignments.failure_class IN ('fencing','startup_timeout')
			  )
			)
		ORDER BY assignments.next_reconcile_at,assignments.id
		FOR UPDATE OF assignments SKIP LOCKED LIMIT 1`, now.UTC(),
	).Scan(
		&claim.AssignmentID, &claim.SandboxID, &claim.InstanceID, &claim.RunnerID,
		&claim.FencingToken, &claim.State.State,
		&claim.State.Generation, &failureClass, &retryCount, &retryLimit, &deadline,
		&releaseProofJSON, &claim.Revision, &assignmentCommandPayload,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation claim lookup: %w", err)
	}
	claim.WorkerID = workerID
	claim.ClaimExpiresAt = claimExpiresAt.UTC()
	claim.State.FailureClass = FailureClass(failureClass)
	claim.State.RetryCount = retryCount
	claim.State.RetryLimit = retryLimit
	claim.State.Deadline = deadline
	var proof map[string]string
	if err := json.Unmarshal(releaseProofJSON, &proof); err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation fence proof decoding: %w", err)
	}
	claim.State.FenceProofDigest = proof["terminationEvidenceDigest"]
	var assignmentEnvelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(assignmentCommandPayload, &assignmentEnvelope); err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation Assignment command decoding: %w", err)
	}
	assignmentCommand := assignmentEnvelope.GetAssignment()
	if assignmentCommand == nil || assignmentCommand.Correlation == nil {
		return Claim{}, false, errors.New("SecondBox reconciliation Assignment correlation is absent")
	}
	claim.Correlation = proto.Clone(assignmentCommand.Correlation).(*runnerv1.Correlation)
	if claim.Correlation.RequestId == "" ||
		claim.Correlation.OperationId == "" ||
		claim.Correlation.SandboxId != claim.SandboxID ||
		claim.Correlation.InstanceId != claim.InstanceID ||
		claim.Correlation.SandboxGeneration != uint64(claim.State.Generation) ||
		claim.Correlation.AssignmentId != claim.AssignmentID ||
		claim.Correlation.RunnerId != claim.RunnerID {
		return Claim{}, false, errors.New("SecondBox reconciliation Assignment correlation does not match durable authority")
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET reconcile_owner=$2,reconcile_claim_expires_at=$3
		WHERE id=$1 AND revision=$4`,
		claim.AssignmentID, workerID, claim.ClaimExpiresAt, claim.Revision,
	)
	if err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation claim update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return Claim{}, false, ErrClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, false, fmt.Errorf("SecondBox reconciliation claim commit: %w", err)
	}
	return claim, true, nil
}

// ApplyDecision commits one idempotent revision-fenced reconciliation transition.
func (store *PostgresStore) ApplyDecision(
	ctx context.Context,
	claim Claim,
	decision Decision,
	fenceCommand *runnerv1.FenceCommand,
	nextReconcileAt time.Time,
	now time.Time,
) error {
	var state, failureClass string
	nextRetryCount := claim.State.RetryCount
	nextDeadline := claim.State.Deadline
	switch decision.Action {
	case ActionWait:
		state = claim.State.State
		failureClass = string(claim.State.FailureClass)
	case ActionFence:
		state = "fencing"
		failureClass = string(FailureStartupTimeout)
		if claim.State.State == "uncertain" || claim.State.FailureClass == FailureFencing {
			failureClass = string(FailureFencing)
		}
		if fenceCommand == nil || fenceCommand.MessageId == "" ||
			fenceCommand.Fence == nil ||
			fenceCommand.Fence.AssignmentId != claim.AssignmentID ||
			int64(fenceCommand.Fence.SandboxGeneration) != claim.State.Generation ||
			claim.Correlation == nil ||
			!proto.Equal(fenceCommand.Correlation, claim.Correlation) ||
			fenceCommand.DeadlineUnixMs == 0 {
			return errors.New("SecondBox reconciliation fence action requires matching command authority")
		}
		nextDeadline = time.UnixMilli(int64(fenceCommand.DeadlineUnixMs)).UTC()
		if claim.State.State == "fencing" {
			nextRetryCount++
		} else {
			nextRetryCount = 0
		}
	case ActionRetry:
		state = "assigned"
		failureClass = ""
		nextRetryCount++
	case ActionFailTerminal:
		state = "failed_terminal"
		failureClass = string(claim.State.FailureClass)
	case ActionAdvanceGeneration:
		return errors.New("SecondBox reconciliation generation advancement requires AdvanceFencedGeneration")
	default:
		return fmt.Errorf("SecondBox reconciliation action %q is invalid", decision.Action)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox reconciliation decision transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, claim.SandboxID)
	if err != nil {
		return fmt.Errorf("SecondBox reconciliation Sandbox/Workspace lock: %w", err)
	}
	if locked.Generation != claim.State.Generation ||
		locked.CurrentInstanceID != claim.InstanceID {
		return ErrClaimLost
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET state=$4,failure_class=$5,retry_count=$6,operation_deadline=$7,next_reconcile_at=$8,
			reconcile_owner='',reconcile_claim_expires_at=$9,revision=revision+1,updated_at=$9
		WHERE id=$1 AND revision=$2 AND reconcile_owner=$3`,
		claim.AssignmentID, claim.Revision, claim.WorkerID, state, failureClass,
		nextRetryCount, nextDeadline.UTC(), nextReconcileAt.UTC(), now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox reconciliation decision update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrClaimLost
	}
	if decision.Action == ActionFence {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='expired',target_connection_id='',updated_at=$2
			WHERE assignment_id=$1 AND kind IN ('assignment','fence')
			  AND state IN ('pending','delivering','delivered')`,
			claim.AssignmentID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation superseded command expiry: %w", err)
		}
		commandID := fenceCommand.MessageId
		queuedCommand := proto.Clone(fenceCommand).(*runnerv1.FenceCommand)
		queuedCommand.MessageId = ""
		queuedCommand.Sequence = 0
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Fence{Fence: queuedCommand},
		})
		if err != nil {
			return fmt.Errorf("SecondBox reconciliation Fence command encoding: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'fence',$4,'pending','',0,$5,$5,NULL)`,
			commandID, claim.RunnerID, claim.AssignmentID, payload, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation Fence command insert: %w", err)
		}
	}
	if decision.Action == ActionRetry {
		var priorPayload []byte
		if err := tx.QueryRow(ctx, `
			SELECT payload FROM secondbox.runner_commands
			WHERE assignment_id=$1 AND kind='assignment'
			ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`,
			claim.AssignmentID,
		).Scan(&priorPayload); err != nil {
			return fmt.Errorf("SecondBox reconciliation Assignment retry payload lookup: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='expired',target_connection_id='',updated_at=$2
			WHERE assignment_id=$1 AND kind='assignment'
			  AND state IN ('pending','delivering','delivered')`,
			claim.AssignmentID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation Assignment command expiry: %w", err)
		}
		retryCommandID := retryAssignmentCommandID(claim.AssignmentID, nextRetryCount)
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'assignment',$4,'pending','',0,$5,$5,NULL)
			ON CONFLICT (id) DO NOTHING`,
			retryCommandID, claim.RunnerID, claim.AssignmentID, priorPayload, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation Assignment retry command insert: %w", err)
		}
	}
	if decision.Action == ActionFailTerminal {
		if locked.Workspace.Mutation.State != "" {
			if locked.Workspace.Mutation.Kind != "start" ||
				locked.Workspace.Mutation.ExpectedGeneration != claim.State.Generation {
				// A concurrent Workspace mutation (an in-flight stop, for
				// example) can legitimately coincide with a terminal
				// Assignment failure. The sentinel lets the worker defer the
				// row instead of ending the reconciler.
				return ports.ErrWorkspaceMutation
			}
			mutationTag, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces
				SET mutation_kind='',mutation_id='',mutation_effect_id='',
				    mutation_operation_id='',mutation_expected_generation=NULL,
				    mutation_target_generation=NULL,mutation_state='',updated_at=$2
				WHERE id=$1 AND mutation_kind='start'
				  AND mutation_expected_generation=$3`,
				locked.WorkspaceID, now.UTC(), claim.State.Generation,
			)
			if err != nil {
				return fmt.Errorf("SecondBox reconciliation terminal start mutation release: %w", err)
			}
			if mutationTag.RowsAffected() != 1 {
				return ErrClaimLost
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='failed',target_connection_id='',updated_at=$2
			WHERE assignment_id=$1 AND kind IN ('assignment','fence')
			  AND state IN ('pending','delivering','delivered')`,
			claim.AssignmentID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation terminal command failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET state='failed',lifecycle_action='fail',
			    lifecycle_termination_reason='startup_failed',
			    lifecycle_failure_class=CASE
			      WHEN $2='' THEN 'internal' ELSE $2
			    END,
			    lifecycle_failure_message='Assignment reconciliation exhausted its retry bound',
			    next_reconcile_at=NULL,reconcile_owner='',
			    reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$3
			WHERE id=$1 AND generation=$4`,
			claim.SandboxID, failureClass, now.UTC(), claim.State.Generation,
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation terminal Sandbox failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.instances
			SET state='failed',guest_liveness='lost',
			    termination_reason=CASE
			      WHEN termination_reason='' THEN 'startup_failed' ELSE termination_reason
			    END,
			    updated_at=$2
			WHERE id=$1 AND state<>'stopped'`,
			claim.InstanceID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation terminal Instance failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.operations
			SET state='failed',error_code='startup_failed',
			    error_message='Assignment reconciliation exhausted its retry bound',
			    retryable=false,started_at=COALESCE(started_at,$2),
			    completed_at=$2,updated_at=$2
			WHERE sandbox_id=$1 AND kind IN ('create','start')
			  AND state IN ('pending','running')`,
			claim.SandboxID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox reconciliation terminal Operation failure: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox reconciliation decision commit: %w", err)
	}
	return nil
}

// AdvanceFencedGeneration converts proven Runner loss into the same runner-local
// generation-advance boundary used by an ordinary stop. It never relocates the
// Sandbox or advances PostgreSQL ahead of the home runner's durable receipt.
func (store *PostgresStore) AdvanceFencedGeneration(
	ctx context.Context,
	assignmentID string,
	expectedRevision int64,
	now time.Time,
) (int64, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("SecondBox fenced generation transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var sandboxID string
	var generation int64
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id,generation FROM secondbox.assignments WHERE id=$1`,
		assignmentID,
	).Scan(&sandboxID, &generation); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Assignment identity lookup: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"secondbox-assignment\x1f"+sandboxID,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Sandbox lock: %w", err)
	}

	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return 0, fmt.Errorf("SecondBox fenced Sandbox/Workspace lookup: %w", err)
	}
	var lockedSandboxID, instanceID, runnerID, state string
	var lockedGeneration, revision int64
	var releaseProofJSON, fencingToken []byte
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id,instance_id,runner_id,state,generation,
		       fencing_token,release_proof_json,revision
		FROM secondbox.assignments WHERE id=$1 FOR UPDATE`, assignmentID,
	).Scan(
		&lockedSandboxID, &instanceID, &runnerID, &state, &lockedGeneration,
		&fencingToken, &releaseProofJSON, &revision,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Assignment lookup: %w", err)
	}
	var proof map[string]string
	if err := json.Unmarshal(releaseProofJSON, &proof); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Assignment proof decoding: %w", err)
	}
	if lockedSandboxID != sandboxID || lockedGeneration != generation ||
		locked.Generation != generation || locked.CurrentInstanceID != instanceID {
		return 0, ErrClaimLost
	}
	if state != "fenced" || proof["terminationEvidenceDigest"] == "" {
		return 0, ErrFenceProofAbsent
	}
	if expectedRevision != 0 && revision != expectedRevision {
		return 0, ErrClaimLost
	}

	now = now.UTC()
	nextGeneration := generation + 1
	workspace := locked.Workspace
	if workspace.HomeRunnerID != runnerID || workspace.State != "ready" ||
		workspace.Generation != generation {
		return 0, ErrClaimLost
	}
	stopEffectID := "runner-loss-stop-" + assignmentID
	queueLocalAdvance := workspace.Mutation.State == ""
	if queueLocalAdvance {
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET mutation_kind='stop',mutation_id=$2,mutation_effect_id=$2,
			    mutation_operation_id=$2,mutation_expected_generation=$3,
			    mutation_target_generation=$4,mutation_state='advancing',updated_at=$5
			WHERE id=$1 AND mutation_state=''`,
			workspace.ID, stopEffectID, generation, nextGeneration, now,
		)
		if err != nil {
			return 0, fmt.Errorf("SecondBox runner-loss Workspace mutation acquisition: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf(
				"%w: runner loss conflicts with an active Workspace mutation",
				ports.ErrWorkspaceMutation,
			)
		}
	} else if workspace.Mutation.Kind != "stop" ||
		workspace.Mutation.ID == "" ||
		workspace.Mutation.EffectID == "" ||
		workspace.Mutation.ExpectedGeneration != generation ||
		workspace.Mutation.TargetGeneration != nextGeneration {
		return 0, fmt.Errorf(
			"%w: runner loss conflicts with an active Workspace mutation",
			ports.ErrWorkspaceMutation,
		)
	}
	if queueLocalAdvance {
		commandID := stopEffectID + "-generation-advance"
		command := &runnerv1.LocalWorkspaceCommand{
			MessageId: commandID, CommandVersion: 1,
			Kind:        runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
			OperationId: stopEffectID, EffectId: stopEffectID,
			SandboxId: sandboxID, WorkspaceId: workspace.ID,
			ExpectedGeneration: uint64(generation), NextGeneration: uint64(nextGeneration),
			LogicalCapacityBytes: uint64(workspace.LogicalCapacityBytes),
			FencingToken:         append([]byte(nil), fencingToken...),
			Correlation: &runnerv1.Correlation{
				RequestId: "request-" + stopEffectID, OperationId: stopEffectID,
				SandboxId: sandboxID, SandboxGeneration: uint64(generation),
				RunnerId: runnerID,
			},
		}
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
				LocalWorkspace: command,
			},
		})
		if err != nil {
			return 0, fmt.Errorf("SecondBox runner-loss generation-advance command encoding: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.lifecycle_effects (
				id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
				command_id,storage_object_id,fencing_token,retry_count,retry_limit,
				effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
				payload_json,evidence_json,created_at,updated_at
			) VALUES (
				$1,$2,$3,'stop','queued',$4,$5,$6,$7,'',$8,0,8,$9,'',$10,
				'','','{}',$11,$10,$10
			)`,
			stopEffectID, sandboxID, generation, assignmentID, instanceID, runnerID,
			commandID, fencingToken, now.Add(10*time.Minute), now, releaseProofJSON,
		); err != nil {
			return 0, fmt.Errorf("SecondBox runner-loss stop effect insert: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
			commandID, runnerID, stopEffectID, payload, now,
		); err != nil {
			return 0, fmt.Errorf("SecondBox runner-loss generation-advance command insert: %w", err)
		}
	} else {
		var effectKind string
		if err := tx.QueryRow(ctx, `
			SELECT kind FROM secondbox.lifecycle_effects WHERE id=$1 FOR UPDATE`,
			workspace.Mutation.EffectID,
		).Scan(&effectKind); err != nil {
			return 0, fmt.Errorf("SecondBox runner-loss existing stop effect lookup: %w", err)
		}
		if effectKind != "stop" {
			return 0, fmt.Errorf(
				"%w: runner-loss Workspace mutation is not a stop effect",
				ports.ErrWorkspaceMutation,
			)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET state='released',reconcile_owner='',reconcile_claim_expires_at=$2,
		    revision=revision+1,updated_at=$2
		WHERE id=$1`,
		assignmentID, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Assignment release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='expired',target_connection_id='',updated_at=$2
		WHERE assignment_id=$1 AND state IN ('pending','delivering','delivered')`,
		assignmentID, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Runner command expiry: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='lost',
		    termination_reason=CASE
		      WHEN termination_reason='' THEN 'runner_lost' ELSE termination_reason
		    END,
		    stopped_at=$2,updated_at=$2
		WHERE id=$1 AND sandbox_id=$3 AND generation=$4`,
		instanceID, now, sandboxID, generation,
	); err != nil {
		return 0, fmt.Errorf("SecondBox lost Instance update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases
		SET state='fenced',revision=revision+1,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
		sandboxID, generation, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced Lease update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state='failed',
		    terminal_kind=CASE
		      WHEN kind IN ('exec','terminal') THEN 'EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED'
		      WHEN kind='port' THEN 'PORT_TERMINAL_KIND_FENCED'
		      ELSE 'FILE_TERMINAL_KIND_FENCED'
		    END,
		    terminal_detail='Sandbox generation was fenced',
		    infrastructure_failure_reason='INFRASTRUCTURE_FAILURE_REASON_GENERATION_FENCED',
		    retryable=false,terminal_message='Sandbox generation was fenced',
		    completed_at=$3,updated_at=$3,retain_until=GREATEST(retain_until,$3)
		WHERE sandbox_id=$1 AND generation=$2
		  AND state IN ('pending','running','cancelling')`,
		sandboxID, generation, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced data-plane update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET state='fenced',closed_at=$3,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state IN ('open','closing')`,
		sandboxID, generation, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced PortSession update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=COALESCE(closed_at,$3),updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
		sandboxID, generation, now,
	); err != nil {
		return 0, fmt.Errorf("SecondBox fenced activity update: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state='stopping',
		    lifecycle_termination_reason=CASE
		      WHEN COALESCE(lifecycle_termination_reason,'')='' THEN 'runner_lost'
		      ELSE lifecycle_termination_reason
		    END,
		    lifecycle_failure_class='',lifecycle_failure_message='',
		    reconcile_owner='',reconcile_claim_expires_at=$3,next_reconcile_at=$3,
		    revision=revision+1,updated_at=$3
		WHERE id=$1 AND generation=$2`,
		sandboxID, generation, now,
	)
	if err != nil {
		return 0, fmt.Errorf("SecondBox runner-loss Sandbox stop transition: %w", err)
	}
	if command.RowsAffected() != 1 {
		return 0, ErrClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("SecondBox runner-loss local generation queue commit: %w", err)
	}
	return nextGeneration, nil
}
