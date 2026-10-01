package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var (
	ErrProfileRevisionMismatch = errors.New("SecondBox scheduler ProfileRevision mismatch")
)

// Serialization backoff bounds. The ceiling keeps a retry well inside a
// placement's deadline while still spreading a burst that all collided at once.
const (
	serializationBackoffBase     = 2 * time.Millisecond
	serializationBackoffCeiling  = 250 * time.Millisecond
	serializationBackoffShiftCap = 10
)

// PostgresStore coordinates scheduler replicas through durable transactions.
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// PostgresStoreConfig contains the scheduler's explicit durable dependencies.
type PostgresStoreConfig struct {
	DatabaseURL string
	Now         func() time.Time
}

// ScheduleRequest contains explicit immutable assignment authority and retry bounds.
type ScheduleRequest struct {
	AssignmentID            string
	AssignmentCommandID     string
	InstanceID              string
	SandboxID               string
	WorkspaceID             string
	StartMutationID         string
	ProfileRevisionID       string
	Requirements            Requirements
	AssignmentCommand       *runnerv1.AssignmentCommand
	FencingToken            []byte
	ResolvedArtifacts       map[string]string
	ClaimExpiresAt          time.Time
	OperationDeadline       time.Time
	RetryLimit              int64
	SerializationRetryLimit int
	HeartbeatTimeout        time.Duration
	Now                     time.Time
	EffectStartedAt         time.Time
	PlanReadyAt             time.Time
	// LifecycleClaimOwner is the lifecycle worker whose claim on the Sandbox
	// this placement completes. While that worker still owns the claim, a new
	// Assignment releases it and schedules the starting Sandbox at its
	// operation deadline in the same transaction.
	LifecycleClaimOwner string
}

// placementTiming records provider-neutral milestones for the successful
// placement attempt. Failed serializable attempts are represented by the gap
// between scheduleStartedAt and attemptStartedAt, so attribution adds no
// database round trips to the path being measured.
type placementTiming struct {
	scheduleStartedAt   time.Time
	attemptStartedAt    time.Time
	sandboxLockedAt     time.Time
	assignmentCheckedAt time.Time
	candidatesLockedAt  time.Time
	candidateSelectedAt time.Time
}

// DurableAssignment contains every private runner and backend authority field.
type DurableAssignment struct {
	ID                      string
	SandboxID               string
	InstanceID              string
	RunnerID                string
	ProfileRevisionID       string
	BackendKind             string
	BackendReference        string
	Generation              int64
	FencingToken            []byte
	State                   string
	CapabilitySnapshot      map[string]string
	ResolvedArtifacts       map[string]string
	ReleaseProof            map[string]string
	FailureClass            string
	RetryCount              int64
	RetryLimit              int64
	OperationDeadline       time.Time
	ClaimExpiresAt          time.Time
	ReconcileOwner          string
	ReconcileClaimExpiresAt time.Time
	NextReconcileAt         time.Time
	Revision                int64
	CreatedAt               time.Time
	UpdatedAt               time.Time
	EgressContext           *string
}

// NewPostgresStore connects the scheduler to PostgreSQL authority.
func NewPostgresStore(
	ctx context.Context,
	config PostgresStoreConfig,
) (*PostgresStore, error) {
	if config.DatabaseURL == "" || config.Now == nil {
		return nil, errors.New("SecondBox scheduler PostgreSQL database and clock are required")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox scheduler PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox scheduler PostgreSQL readiness: %w", err)
	}
	return &PostgresStore{pool: pool, now: config.Now}, nil
}

func (store *PostgresStore) Close() {
	store.pool.Close()
}

// Schedule serializes one Sandbox generation and reserves capacity on one compatible Runner.
func (store *PostgresStore) Schedule(
	ctx context.Context,
	request ScheduleRequest,
) (DurableAssignment, bool, error) {
	if err := validateScheduleRequest(request); err != nil {
		return DurableAssignment{}, false, err
	}
	scheduleStartedAt, err := store.observeAtOrAfter(request.PlanReadyAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	increases, ledgerForced := rowlock.QuotaTransition(startIncreasesQuota), false
	for attempt := 0; ; attempt++ {
		attemptStartedAt, err := store.observeAtOrAfter(scheduleStartedAt)
		if err != nil {
			return DurableAssignment{}, false, err
		}
		timing := placementTiming{
			scheduleStartedAt: scheduleStartedAt,
			attemptStartedAt:  attemptStartedAt,
		}
		assignment, created, err := store.scheduleOnce(ctx, request, timing, increases)
		if errors.Is(err, rowlock.ErrQuotaLedgerRequired) && !ledgerForced {
			// The Sandbox reached a state the unlocked read did not show, from
			// which starting it increases quota usage. Retry holding the ledgers;
			// a forced attempt cannot report this again.
			increases, ledgerForced = rowlock.AlwaysQuotaLedger, true
			continue
		}
		if !isSerializationFailure(err) {
			return assignment, created, err
		}
		if attempt >= request.SerializationRetryLimit {
			// Report contention as contention. A caller that receives the raw
			// PostgreSQL error cannot distinguish "try again" from "this is
			// broken", and the reconciler treated it as the latter.
			return assignment, created, fmt.Errorf(
				"%w after %d attempts: %v",
				ports.ErrSerializationContention, attempt+1, err,
			)
		}
		// Retrying immediately makes contention worse: every loser wakes at the
		// same instant and collides again. Back off with full jitter so a burst
		// spreads out instead of resonating.
		if !sleepWithContext(ctx, serializationBackoff(attempt)) {
			return assignment, created, ctx.Err()
		}
	}
}

// serializationBackoff returns a full-jitter delay: a uniform draw from
// [0, base*2^attempt] capped at serializationBackoffCeiling. Full jitter spreads
// a colliding cohort more effectively than a fixed or purely exponential delay,
// because two losers of the same race draw independently.
func serializationBackoff(attempt int) time.Duration {
	window := serializationBackoffBase << min(attempt, serializationBackoffShiftCap)
	if window > serializationBackoffCeiling {
		window = serializationBackoffCeiling
	}
	return time.Duration(rand.Int64N(int64(window) + 1))
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// startIncreasesQuota reports whether committing `starting` adds to quota usage.
// Starting a Sandbox admitted to run does not; restarting a failed one does.
func startIncreasesQuota(state, desiredState string) bool {
	return rowlock.QuotaUsageIncreases(state, desiredState, contracts.SandboxStateStarting, desiredState)
}

func (store *PostgresStore) scheduleOnce(
	ctx context.Context,
	request ScheduleRequest,
	timing placementTiming,
	increases rowlock.QuotaTransition,
) (DurableAssignment, bool, error) {
	// Placement runs at READ COMMITTED. Every row it decides on is locked
	// before it is read: the per-Sandbox advisory lock guards Assignment
	// existence, the rowlock order covers the Sandbox and Workspace (and the
	// quota ledgers when starting increases usage), and the home Runner row
	// guards reserved capacity. Profile and
	// Subject policy are read unlocked; their writers read no placement state,
	// so a concurrent policy change orders before or after this placement.
	// Serializable isolation added no protection here and aborted most
	// placements in a burst, on the shared Runner row and on predicate locks
	// between unrelated Sandboxes.
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"secondbox-assignment\x1f"+request.SandboxID,
	); err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler Sandbox lock: %w", err)
	}
	locked, err := rowlock.SandboxWorkspaceForTransition(ctx, tx, request.SandboxID, increases)
	if errors.Is(err, rowlock.ErrQuotaLedgerRequired) {
		return DurableAssignment{}, false, err
	}
	if err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler Sandbox/Workspace lookup: %w", err)
	}
	timing.sandboxLockedAt, err = store.observeAtOrAfter(timing.attemptStartedAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	if locked.WorkspaceID != request.WorkspaceID {
		return DurableAssignment{}, false, ErrProfileRevisionMismatch
	}
	generation := locked.Generation
	pinnedProfileRevisionID := locked.ProfileRevisionID
	currentInstanceID := locked.CurrentInstanceID
	workspace := locked.Workspace
	homeRunnerID := workspace.HomeRunnerID
	if homeRunnerID == "" {
		return DurableAssignment{}, false, ErrHomeRunnerUnavailable
	}
	if workspace.Generation != generation ||
		(workspace.State != "creating" && workspace.State != "ready") {
		return DurableAssignment{}, false, ErrProfileRevisionMismatch
	}
	if !equalEgressContext(locked.EgressContext, request.Requirements.EgressContext) {
		return DurableAssignment{}, false, ErrProfileRevisionMismatch
	}
	if pinnedProfileRevisionID != request.ProfileRevisionID {
		return DurableAssignment{}, false, ErrProfileRevisionMismatch
	}
	if int64(request.AssignmentCommand.Fence.SandboxGeneration) != generation {
		return DurableAssignment{}, false, ErrProfileRevisionMismatch
	}
	existing, err := readAssignment(ctx, tx, request.SandboxID, generation)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler existing assignment commit: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DurableAssignment{}, false, err
	}
	if workspace.Mutation.State != "" &&
		(workspace.Mutation.ID != request.StartMutationID || workspace.Mutation.Kind != "start") {
		return DurableAssignment{}, false, errors.New("SecondBox scheduler Workspace has a conflicting local mutation")
	}
	if currentInstanceID != "" {
		return DurableAssignment{}, false, errors.New("SecondBox scheduler Sandbox has an Instance without durable assignment")
	}
	timing.assignmentCheckedAt, err = store.observeAtOrAfter(timing.sandboxLockedAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	runners, err := lockHomeRunner(ctx, tx, request.Requirements.PoolName, homeRunnerID)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	timing.candidatesLockedAt, err = store.observeAtOrAfter(timing.assignmentCheckedAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	selected, err := SelectHomeRunner(
		homeRunnerID, request.Requirements, runners,
		request.Now.UTC(), request.HeartbeatTimeout,
	)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	timing.candidateSelectedAt, err = store.observeAtOrAfter(timing.candidatesLockedAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	capabilitySnapshot := capabilitySnapshot(selected)
	capabilitiesJSON, err := json.Marshal(capabilitySnapshot)
	if err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler capability snapshot encoding: %w", err)
	}
	artifactsJSON, err := json.Marshal(request.ResolvedArtifacts)
	if err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler resolved artifacts encoding: %w", err)
	}
	controlMessage := &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Assignment{
			Assignment: proto.Clone(request.AssignmentCommand).(*runnerv1.AssignmentCommand),
		},
	}
	controlMessage.GetAssignment().MessageId = ""
	if permission := controlMessage.GetAssignment().AttributedExecutionPermission; permission != nil {
		if err := resolveAttributedConnections(ctx, tx, locked, permission); err != nil {
			return DurableAssignment{}, false, err
		}
	}
	controlMessage.GetAssignment().Sequence = 0
	controlMessage.GetAssignment().Correlation.SandboxId = request.SandboxID
	controlMessage.GetAssignment().Correlation.InstanceId = request.InstanceID
	controlMessage.GetAssignment().Correlation.SandboxGeneration = uint64(generation)
	controlMessage.GetAssignment().Correlation.AssignmentId = request.AssignmentID
	controlMessage.GetAssignment().Correlation.RunnerId = selected.ID
	commandPayload, err := proto.Marshal(controlMessage)
	if err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler Assignment command encoding: %w", err)
	}
	reserved := addCapacity(selected.Reserved, request.Requirements.Capacity)
	reservedJSON, err := encodeCapacity(reserved)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	placementAt, err := store.observeAtOrAfter(timing.candidateSelectedAt)
	if err != nil {
		return DurableAssignment{}, false, err
	}
	assignment := DurableAssignment{
		ID: request.AssignmentID, SandboxID: request.SandboxID, InstanceID: request.InstanceID,
		RunnerID: selected.ID, ProfileRevisionID: request.ProfileRevisionID,
		BackendKind: selected.BackendKind, Generation: generation,
		FencingToken: append([]byte(nil), request.FencingToken...), State: "assigned",
		CapabilitySnapshot: capabilitySnapshot, ResolvedArtifacts: request.ResolvedArtifacts,
		ReleaseProof: map[string]string{}, RetryLimit: request.RetryLimit,
		OperationDeadline: request.OperationDeadline.UTC(), ClaimExpiresAt: request.ClaimExpiresAt.UTC(),
		NextReconcileAt: request.OperationDeadline.UTC(),
		EgressContext:   cloneEgressContext(request.Requirements.EgressContext),
		Revision:        1, CreatedAt: placementAt, UpdatedAt: placementAt,
	}
	orderedWrites := &pgx.Batch{}
	workspaceMutationError := "SecondBox scheduler Workspace start mutation acquisition"
	if workspace.Mutation.State == "" {
		orderedWrites.Queue(`
			UPDATE secondbox.workspaces
			SET mutation_kind='start',mutation_id=$2,mutation_effect_id=$3,
			    mutation_operation_id=$4,mutation_expected_generation=$5,
			    mutation_target_generation=$5,mutation_state='assigned',updated_at=$6
			WHERE id=$1`,
			request.WorkspaceID, request.StartMutationID, request.AssignmentCommandID,
			request.AssignmentCommand.Correlation.OperationId, generation, placementAt,
		)
	} else {
		workspaceMutationError = "SecondBox scheduler Workspace start mutation adoption"
		orderedWrites.Queue(`
			UPDATE secondbox.workspaces
			SET mutation_effect_id=$2,mutation_state='assigned',updated_at=$3
			WHERE id=$1 AND mutation_kind='start' AND mutation_id=$4`,
			request.WorkspaceID, request.AssignmentCommandID, placementAt, request.StartMutationID,
		)
	}
	orderedWrites.Queue(`
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,created_at,updated_at,
			ready_at,stopped_at,requested_image_reference,resolved_image_digest
		) VALUES ($1,$2,$3,'starting','starting','',$4,$4,NULL,NULL,$5,'')`,
		assignment.InstanceID, assignment.SandboxID, generation, placementAt,
		request.AssignmentCommand.GetExecutionImage().GetReference(),
	)
	orderedWrites.Queue(`
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at,egress_context
		) VALUES (
			$1,$2,$3,$4,$5,$6,'',$7,$8,$9,$10,$11,'{}','',0,$12,$13,$14,'',$15,$13,1,$15,$15,$16
		)`,
		assignment.ID, assignment.SandboxID, assignment.InstanceID, assignment.RunnerID,
		assignment.ProfileRevisionID, assignment.BackendKind, assignment.Generation,
		assignment.FencingToken, assignment.State, capabilitiesJSON, artifactsJSON,
		assignment.RetryLimit, assignment.OperationDeadline, assignment.ClaimExpiresAt, placementAt,
		request.Requirements.EgressContext,
	)
	// The command is always queued pending. Assigning its stream sequence here
	// meant locking the runner's single runner_connections row inside every
	// placement transaction, so concurrent placements for one runner serialised
	// on one row under serializable isolation and lost the race. The connection
	// owner assigns the sequence when it claims the command; it is the only
	// writer of that counter, so it needs no lock at all.
	const commandState = "pending"
	const targetConnectionID = ""
	const deliveryCount = int64(0)
	orderedWrites.Queue(`
		WITH inserted_command AS (
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'assignment',$4,$5,$6,$7,$8,$8,NULL)
			RETURNING 1
		)
		INSERT INTO secondbox.operation_stage_timings (
			operation_id,sandbox_id,stage,observed_at
		)
		SELECT $9,$10,timing.stage,timing.observed_at
		FROM inserted_command
		CROSS JOIN (VALUES
			('placement_reconcile_started',$11::timestamptz),
			('placement_effect_started',$12::timestamptz),
			('placement_plan_ready',$13::timestamptz),
			('placement_schedule_started',$14::timestamptz),
			('placement_attempt_started',$15::timestamptz),
			('placement_sandbox_locked',$16::timestamptz),
			('placement_assignment_checked',$17::timestamptz),
			('placement_candidates_locked',$18::timestamptz),
			('placement_candidate_selected',$19::timestamptz),
			('placement_ready',$8::timestamptz)
		) AS timing(stage,observed_at)
		ON CONFLICT (operation_id,stage) DO NOTHING`,
		request.AssignmentCommandID, assignment.RunnerID, assignment.ID, commandPayload,
		commandState, targetConnectionID, deliveryCount, placementAt,
		request.AssignmentCommand.Correlation.OperationId,
		request.SandboxID,
		request.Now.UTC(), request.EffectStartedAt.UTC(), request.PlanReadyAt.UTC(),
		timing.scheduleStartedAt, timing.attemptStartedAt, timing.sandboxLockedAt,
		timing.assignmentCheckedAt, timing.candidatesLockedAt, timing.candidateSelectedAt,
	)
	orderedWrites.Queue(`
		UPDATE secondbox.runners
		SET reserved_capacity_json=$2,revision=revision+1,updated_at=$3 WHERE id=$1`,
		selected.ID, reservedJSON, placementAt,
	)
	// The starting Sandbox waits for Runner evidence, which wakes it; its
	// Assignment's operation deadline is only the recovery bound. A stop or
	// delete intent that committed after the claim's start plan still wants
	// reconciliation now, so the released claim leaves it due immediately.
	orderedWrites.Queue(`
		UPDATE secondbox.sandboxes
		SET state='starting',current_instance_id=$2,revision=revision+1,updated_at=$3,
		    lifecycle_action=CASE WHEN reconcile_owner=$4 THEN 'start_instance' ELSE lifecycle_action END,
		    next_reconcile_at=CASE
		      WHEN reconcile_owner=$4 AND desired_state='running' THEN $5
		      WHEN reconcile_owner=$4 THEN $3
		      ELSE next_reconcile_at
		    END,
		    reconcile_claim_expires_at=CASE WHEN reconcile_owner=$4 THEN NULL ELSE reconcile_claim_expires_at END,
		    reconcile_owner=CASE WHEN reconcile_owner=$4 THEN '' ELSE reconcile_owner END
		WHERE id=$1`,
		request.SandboxID, request.InstanceID, placementAt,
		request.LifecycleClaimOwner, request.OperationDeadline.UTC(),
	)
	results := tx.SendBatch(ctx, orderedWrites)
	var writeErr error
	workspaceMutation, err := results.Exec()
	if err != nil {
		writeErr = fmt.Errorf("%s: %w", workspaceMutationError, err)
	} else if workspaceMutation.RowsAffected() != 1 {
		writeErr = errors.New("SecondBox scheduler Workspace start mutation changed before assignment")
	}
	for _, errorPrefix := range []string{
		"SecondBox scheduler Instance insert",
		"SecondBox scheduler Assignment insert",
	} {
		if _, err := results.Exec(); err != nil && writeErr == nil {
			writeErr = fmt.Errorf("%s: %w", errorPrefix, err)
		}
	}
	for _, errorPrefix := range []string{
		"SecondBox scheduler Assignment command insert",
		"SecondBox scheduler Runner reservation update",
		"SecondBox scheduler Sandbox assignment update",
	} {
		if _, err := results.Exec(); err != nil && writeErr == nil {
			writeErr = fmt.Errorf("%s: %w", errorPrefix, err)
		}
	}
	if err := results.Close(); err != nil {
		writeErr = errors.Join(
			writeErr,
			fmt.Errorf("SecondBox scheduler ordered assignment writes close: %w", err),
		)
	}
	if writeErr != nil {
		return DurableAssignment{}, false, writeErr
	}
	if err := tx.Commit(ctx); err != nil {
		return DurableAssignment{}, false, fmt.Errorf("SecondBox scheduler commit: %w", err)
	}
	return assignment, true, nil
}

func validateScheduleRequest(request ScheduleRequest) error {
	if request.AssignmentID == "" || request.AssignmentCommandID == "" ||
		request.InstanceID == "" || request.SandboxID == "" ||
		request.WorkspaceID == "" || request.StartMutationID == "" ||
		request.ProfileRevisionID == "" || request.Requirements.PoolName == "" ||
		request.Requirements.Architecture == "" ||
		request.LifecycleClaimOwner == "" ||
		len(request.FencingToken) < 32 || request.ClaimExpiresAt.IsZero() ||
		request.OperationDeadline.IsZero() || request.HeartbeatTimeout <= 0 ||
		request.RetryLimit < 0 || request.SerializationRetryLimit < 0 || request.Now.IsZero() {
		return errors.New("SecondBox scheduler request requires complete identity, profile, fence, deadline, and retry bounds")
	}
	if request.EffectStartedAt.IsZero() || request.PlanReadyAt.IsZero() ||
		request.EffectStartedAt.Before(request.Now) ||
		request.PlanReadyAt.Before(request.EffectStartedAt) {
		return errors.New("SecondBox scheduler request requires ordered placement timing authority")
	}
	command := request.AssignmentCommand
	if command == nil || command.Fence == nil ||
		command.Fence.AssignmentId != request.AssignmentID ||
		command.Fence.SandboxId != request.SandboxID ||
		command.Fence.InstanceId != request.InstanceID ||
		command.Fence.SandboxGeneration == 0 ||
		!bytes.Equal(command.Fence.FencingToken, request.FencingToken) ||
		command.ProfileRevisionId != request.ProfileRevisionID ||
		command.WorkspaceId != request.WorkspaceID ||
		command.Requirements == nil ||
		command.Correlation == nil ||
		command.Correlation.RequestId == "" ||
		command.Correlation.OperationId == "" ||
		command.Correlation.SandboxId != request.SandboxID ||
		command.Correlation.SandboxGeneration != command.Fence.SandboxGeneration ||
		command.DeadlineUnixMs == 0 {
		return errors.New("SecondBox scheduler Assignment command does not match durable assignment authority")
	}
	if command.ExecutionImage != nil {
		if err := (contracts.ExecutionImage{Reference: command.ExecutionImage.Reference}).Validate(); err != nil {
			return fmt.Errorf("SecondBox scheduler Assignment execution image is invalid: %w", err)
		}
		if !strings.Contains(command.ExecutionImage.Reference, "@sha256:") {
			return errors.New("SecondBox assignment image must be resolved before scheduling")
		}
	}
	if request.Requirements.EgressContext == nil {
		if command.Requirements.RequiresTenantEgressContext || command.EgressContext != "" {
			return errors.New("SecondBox scheduler context-free placement carries an Assignment egress context")
		}
	} else if !command.Requirements.RequiresTenantEgressContext ||
		command.EgressContext != *request.Requirements.EgressContext {
		return errors.New("SecondBox scheduler Assignment egress context does not match placement requirements")
	}
	if request.Requirements.EgressContext != nil {
		if err := contracts.ValidateEgressContextName(*request.Requirements.EgressContext); err != nil {
			return fmt.Errorf("SecondBox scheduler placement egress context is invalid: %w", err)
		}
	}
	return nil
}

func equalEgressContext(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneEgressContext(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (store *PostgresStore) observeAtOrAfter(previous time.Time) (time.Time, error) {
	observedAt := store.now().UTC()
	if observedAt.IsZero() {
		return time.Time{}, errors.New("SecondBox scheduler clock returned zero time")
	}
	if observedAt.Before(previous) {
		return previous.UTC(), nil
	}
	return observedAt, nil
}

func isSerializationFailure(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		(postgresError.Code == "40001" || postgresError.Code == "40P01")
}

// lockHomeRunner locks only the Sandbox's home Runner row. Ordinary lifecycle
// placement never considers another Runner, so locking the rest of the pool
// would serialize every concurrent start in the pool on rows it cannot use.
func lockHomeRunner(
	ctx context.Context,
	tx pgx.Tx,
	poolName string,
	homeRunnerID string,
) ([]RunnerSnapshot, error) {
	rows, err := tx.Query(ctx, `
		SELECT id,pool_name,architectures_json,capabilities_json,capacity_json,
			reserved_capacity_json,drain_phase,last_seen_at,artifact_cache_json,
			guest_protocol_minimum,guest_protocol_maximum,backend_kind,
			supported_egress_contexts_json
		FROM secondbox.runners
		WHERE id=$1 AND pool_name=$2 AND state='ready'
		FOR UPDATE`, homeRunnerID, poolName)
	if err != nil {
		return nil, fmt.Errorf("SecondBox scheduler home Runner lock: %w", err)
	}
	defer rows.Close()
	runners := make([]RunnerSnapshot, 0)
	for rows.Next() {
		var runner RunnerSnapshot
		var architecturesJSON, capabilitiesJSON, allocatableJSON, reservedJSON, cacheJSON, egressContextsJSON []byte
		if err := rows.Scan(
			&runner.ID, &runner.PoolName, &architecturesJSON, &capabilitiesJSON,
			&allocatableJSON, &reservedJSON, &runner.DrainPhase, &runner.LastHeartbeatAt,
			&cacheJSON, &runner.GuestProtocolMinimum, &runner.GuestProtocolMaximum, &runner.BackendKind,
			&egressContextsJSON,
		); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler home Runner scan: %w", err)
		}
		var architectures []string
		if err := json.Unmarshal(architecturesJSON, &architectures); err != nil || len(architectures) != 1 {
			return nil, fmt.Errorf("SecondBox scheduler Runner architecture evidence is invalid")
		}
		runner.Architecture = architectures[0]
		var capabilities []string
		if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler Runner capabilities decoding: %w", err)
		}
		runner.Capabilities = make(map[string]bool, len(capabilities))
		for _, capability := range capabilities {
			runner.Capabilities[capability] = true
		}
		if err := json.Unmarshal(allocatableJSON, &runner.Allocatable); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler Runner capacity decoding: %w", err)
		}
		if err := json.Unmarshal(reservedJSON, &runner.Reserved); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler Runner reservation decoding: %w", err)
		}
		var cacheEvidence struct {
			Materializations []MaterializationSnapshot `json:"materializations"`
		}
		if err := json.Unmarshal(cacheJSON, &cacheEvidence); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler Runner cache decoding: %w", err)
		}
		runner.Materializations = cacheEvidence.Materializations
		if err := json.Unmarshal(egressContextsJSON, &runner.SupportedEgressContexts); err != nil {
			return nil, fmt.Errorf("SecondBox scheduler Runner egress-context decoding: %w", err)
		}
		runners = append(runners, runner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox scheduler home Runner iteration: %w", err)
	}
	return runners, nil
}

func readAssignment(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
	generation int64,
) (DurableAssignment, error) {
	var assignment DurableAssignment
	var capabilityJSON, artifactsJSON, releaseJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at,egress_context
		FROM secondbox.assignments WHERE sandbox_id=$1 AND generation=$2`,
		sandboxID, generation,
	).Scan(
		&assignment.ID, &assignment.SandboxID, &assignment.InstanceID, &assignment.RunnerID,
		&assignment.ProfileRevisionID, &assignment.BackendKind, &assignment.BackendReference,
		&assignment.Generation, &assignment.FencingToken, &assignment.State, &capabilityJSON,
		&artifactsJSON, &releaseJSON, &assignment.FailureClass, &assignment.RetryCount,
		&assignment.RetryLimit, &assignment.OperationDeadline, &assignment.ClaimExpiresAt,
		&assignment.ReconcileOwner, &assignment.ReconcileClaimExpiresAt,
		&assignment.NextReconcileAt,
		&assignment.Revision, &assignment.CreatedAt, &assignment.UpdatedAt, &assignment.EgressContext,
	)
	if err != nil {
		return DurableAssignment{}, err
	}
	if err := json.Unmarshal(capabilityJSON, &assignment.CapabilitySnapshot); err != nil {
		return DurableAssignment{}, fmt.Errorf("SecondBox scheduler Assignment capability decoding: %w", err)
	}
	if err := json.Unmarshal(artifactsJSON, &assignment.ResolvedArtifacts); err != nil {
		return DurableAssignment{}, fmt.Errorf("SecondBox scheduler Assignment artifacts decoding: %w", err)
	}
	if err := json.Unmarshal(releaseJSON, &assignment.ReleaseProof); err != nil {
		return DurableAssignment{}, fmt.Errorf("SecondBox scheduler Assignment release proof decoding: %w", err)
	}
	return assignment, nil
}

func capabilitySnapshot(runner RunnerSnapshot) map[string]string {
	snapshot := map[string]string{
		"architecture":         runner.Architecture,
		"pool":                 runner.PoolName,
		"guestProtocolMinimum": fmt.Sprintf("%d", runner.GuestProtocolMinimum),
		"guestProtocolMaximum": fmt.Sprintf("%d", runner.GuestProtocolMaximum),
	}
	for capability, ready := range runner.Capabilities {
		if ready {
			snapshot["capability."+capability] = "true"
		}
	}
	return snapshot
}

func addCapacity(left, right Capacity) Capacity {
	return Capacity{
		VCPUCount:   left.VCPUCount + right.VCPUCount,
		MemoryBytes: left.MemoryBytes + right.MemoryBytes,
		DiskBytes:   left.DiskBytes + right.DiskBytes,
		Instances:   left.Instances + right.Instances,
		Operations:  left.Operations + right.Operations,
	}
}

func encodeCapacity(capacity Capacity) ([]byte, error) {
	encoded, err := json.Marshal(capacity)
	if err != nil {
		return nil, fmt.Errorf("SecondBox scheduler capacity encoding: %w", err)
	}
	return encoded, nil
}
