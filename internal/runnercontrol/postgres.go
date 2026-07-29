package runnercontrol

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStateStore persists registration, heartbeat, capacity, cache, and message ordering.
type PostgresStateStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStateStore connects runner protocol state to PostgreSQL authority.
func NewPostgresStateStore(
	ctx context.Context,
	databaseURL string,
) (*PostgresStateStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner control PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox runner control PostgreSQL readiness: %w", err)
	}
	return &PostgresStateStore{pool: pool}, nil
}

func (store *PostgresStateStore) Close() {
	store.pool.Close()
}

// OpenConnection binds a verified certificate serial to one new protocol connection.
func (store *PostgresStateStore) OpenConnection(
	ctx context.Context,
	identity RunnerIdentity,
	connectionID string,
	protocolVersion uint32,
	now time.Time,
) error {
	if identity.RunnerID == "" || identity.CredentialSerial == "" ||
		connectionID == "" || protocolVersion == 0 {
		return errors.New("SecondBox runner connection requires credential identity, connection, and protocol version")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox runner connection transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	now = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_connections
		SET state='superseded',disconnected_at=$2,last_seen_at=$2
		WHERE runner_id=$1 AND state='active'`, identity.RunnerID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner previous connection supersede: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_connections (
			id,runner_id,credential_serial,protocol_version,state,last_sequence,
			last_control_sequence,connected_at,last_seen_at,disconnected_at
		) VALUES ($1,$2,$3,$4,'active',0,0,$5,$5,NULL)`,
		connectionID, identity.RunnerID, identity.CredentialSerial, protocolVersion, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner connection insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='pending',target_connection_id='',updated_at=$2
		WHERE runner_id=$1 AND state='delivering'`,
		identity.RunnerID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner pending command recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox runner connection commit: %w", err)
	}
	return nil
}

// RecordRegistration durably records schedulable capability evidence exactly once.
func (store *PostgresStateStore) RecordRegistration(
	ctx context.Context,
	registration *runnerv1.RunnerRegistration,
	now time.Time,
) (bool, error) {
	if registration == nil {
		return false, errors.New("SecondBox runner Registration is required")
	}
	if len(registration.ReadinessFailures) != 0 {
		return false, ErrRunnerPrerequisites
	}
	capabilities := map[string]bool{
		"firecracker":    registration.Capabilities != nil && registration.Capabilities.FirecrackerVersion != "",
		"kvm":            registration.Capabilities != nil && registration.Capabilities.KvmReady,
		"jailer":         registration.Capabilities != nil && registration.Capabilities.JailerReady,
		"cgroup":         registration.Capabilities != nil && registration.Capabilities.CgroupReady,
		"network-policy": registration.Capabilities != nil && registration.Capabilities.NetworkPolicyReady,
		"storage":        registration.Capabilities != nil && registration.Capabilities.StorageReady,
		"cleanup":        registration.Capabilities != nil && registration.Capabilities.CleanupReady,
	}
	capabilities["checkpoint"] = capabilities["storage"] && capabilities["cleanup"]
	if registration.Capabilities == nil ||
		registration.Capabilities.GuestProtocolGenerations == nil ||
		registration.Capabilities.GuestProtocolGenerations.Minimum == 0 ||
		registration.Capabilities.GuestProtocolGenerations.Minimum >
			registration.Capabilities.GuestProtocolGenerations.Maximum ||
		!allPrerequisitesReady(capabilities) {
		return false, ErrRunnerPrerequisites
	}
	architecturesJSON, err := json.Marshal([]string{registration.Capabilities.Architecture})
	if err != nil {
		return false, fmt.Errorf("SecondBox runner architecture encoding: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(capabilities)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner capabilities encoding: %w", err)
	}
	allocatableJSON, err := encodeProtocolCapacity(registration.Allocatable)
	if err != nil {
		return false, err
	}
	reservedJSON, err := encodeProtocolCapacity(registration.Reserved)
	if err != nil {
		return false, err
	}
	versionsJSON, err := json.Marshal([]uint32{registration.ProtocolVersion})
	if err != nil {
		return false, fmt.Errorf("SecondBox runner protocol versions encoding: %w", err)
	}
	cache := struct {
		ArtifactDigests      []string `json:"artifactDigests"`
		WorkspaceCheckpoints []string `json:"workspaceCheckpoints"`
	}{ArtifactDigests: make([]string, 0, len(registration.ArtifactCache))}
	for _, evidence := range registration.ArtifactCache {
		if evidence != nil {
			cache.ArtifactDigests = append(cache.ArtifactDigests, evidence.ManifestDigest)
		}
	}
	cacheJSON, err := json.Marshal(cache)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner cache evidence encoding: %w", err)
	}
	tx, duplicate, err := store.beginOrderedMessage(
		ctx, registration.RunnerId, registration.ConnectionId,
		registration.MessageId, registration.Sequence, "registration", now,
	)
	if err != nil || duplicate {
		return duplicate, err
	}
	defer tx.Rollback(ctx)
	var poolState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.runner_pools WHERE name=$1`,
		registration.RunnerPoolId,
	).Scan(&poolState); err != nil {
		return false, fmt.Errorf("SecondBox runner Registration pool lookup: %w", err)
	}
	if poolState != "ready" {
		return false, errors.New("SecondBox RunnerPool is not accepting runners")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,last_seen_at,revision,created_at,updated_at
		) VALUES ($1,$2,$1,'connected','[]','{}','{}','[]',0,0,'',$3,0,'active','{}','[]',NULL,1,$4,$4)
		ON CONFLICT (id) DO UPDATE SET
			pool_name=EXCLUDED.pool_name,active_connection_id=EXCLUDED.active_connection_id,
			state='connected',last_sequence=0,revision=secondbox.runners.revision+1,
			updated_at=EXCLUDED.updated_at`,
		registration.RunnerId, registration.RunnerPoolId, registration.ConnectionId, now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox runner Registration identity upsert: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runners
		SET state='ready',architectures_json=$3,capabilities_json=$4,
			capacity_json=$5,protocol_versions_json=$6,
			guest_protocol_minimum=$7,guest_protocol_maximum=$8,software_version=$9,
			last_sequence=$10,drain_phase='active',reserved_capacity_json=$11,
			artifact_cache_json=$12,last_seen_at=$13,revision=revision+1,updated_at=$13
		WHERE id=$1 AND pool_name=$2 AND active_connection_id=$14`,
		registration.RunnerId, registration.RunnerPoolId, architecturesJSON, capabilitiesJSON,
		allocatableJSON, versionsJSON,
		registration.Capabilities.GuestProtocolGenerations.Minimum,
		registration.Capabilities.GuestProtocolGenerations.Maximum,
		registration.SoftwareVersion, registration.Sequence,
		reservedJSON, cacheJSON, now.UTC(), registration.ConnectionId,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Registration update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("SecondBox runner Registration connection is no longer active")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner Registration commit: %w", err)
	}
	return false, nil
}

// RecordHeartbeat persists current liveness, capacity, assignments, and drain state.
func (store *PostgresStateStore) RecordHeartbeat(
	ctx context.Context,
	heartbeat *runnerv1.RunnerHeartbeat,
	now time.Time,
) (bool, error) {
	if heartbeat == nil || heartbeat.Allocatable == nil || heartbeat.Reserved == nil {
		return false, errors.New("SecondBox runner Heartbeat capacity evidence is required")
	}
	allocatableJSON, err := encodeProtocolCapacity(heartbeat.Allocatable)
	if err != nil {
		return false, err
	}
	reservedJSON, err := encodeProtocolCapacity(heartbeat.Reserved)
	if err != nil {
		return false, err
	}
	drainPhase, runnerState, err := protocolDrainState(heartbeat.DrainPhase)
	if err != nil {
		return false, err
	}
	tx, duplicate, err := store.beginOrderedMessage(
		ctx, heartbeat.RunnerId, heartbeat.ConnectionId,
		heartbeat.MessageId, heartbeat.Sequence, "heartbeat", now,
	)
	if err != nil || duplicate {
		return duplicate, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runners
		SET state=$2,capacity_json=$3,reserved_capacity_json=$4,last_sequence=$5,
			drain_phase=$6,last_seen_at=$7,revision=revision+1,updated_at=$7
		WHERE id=$1 AND active_connection_id=$8`,
		heartbeat.RunnerId, runnerState, allocatableJSON, reservedJSON,
		heartbeat.Sequence, drainPhase, now.UTC(), heartbeat.ConnectionId,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Heartbeat update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("SecondBox runner Heartbeat connection is no longer active")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner Heartbeat commit: %w", err)
	}
	return false, nil
}

// RecordEvent persists assignment, fencing, drain, or evidence results with fence validation.
func (store *PostgresStateStore) RecordEvent(
	ctx context.Context,
	event Event,
	now time.Time,
) (bool, error) {
	messageID, sequence, err := runnerEnvelope(event.Message)
	if err != nil {
		return false, err
	}
	tx, duplicate, err := store.beginOrderedMessage(
		ctx, event.RunnerID, event.ConnectionID, messageID, sequence,
		string(event.Kind), now,
	)
	if err != nil {
		return false, err
	}
	if duplicate {
		if event.Kind == EventInstanceTerminal {
			if err := store.validatePersistedInstanceTerminal(
				ctx, event.RunnerID, event.Message.GetInstanceTerminal(),
			); err != nil {
				return false, err
			}
		}
		return duplicate, err
	}
	defer tx.Rollback(ctx)
	switch event.Kind {
	case EventAssignment:
		if err := recordAssignmentEvent(ctx, tx, event.RunnerID, event.Message, now.UTC()); err != nil {
			return false, err
		}
	case EventFence:
		if err := recordFenceEvent(ctx, tx, event.RunnerID, event.Message.GetFenceResult(), now.UTC()); err != nil {
			return false, err
		}
	case EventDrain:
		if err := recordDrainEvent(ctx, tx, event.RunnerID, event.Message.GetDrainState(), now.UTC()); err != nil {
			return false, err
		}
	case EventEvidence:
		if err := validateRunnerEvidence(event.RunnerID, event.Message.GetEvidence()); err != nil {
			return false, err
		}
	case EventCheckpoint:
		if err := recordCheckpointEvent(ctx, tx, event.RunnerID, event.Message, now.UTC()); err != nil {
			return false, err
		}
	case EventInstanceTerminal:
		if err := recordInstanceTerminal(
			ctx, tx, event.RunnerID, event.Message.GetInstanceTerminal(), now.UTC(),
		); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("SecondBox runner event kind %q is not durable", event.Kind)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner event commit: %w", err)
	}
	return false, nil
}

func (store *PostgresStateStore) validatePersistedInstanceTerminal(
	ctx context.Context,
	runnerID string,
	terminal *runnerv1.InstanceTerminal,
) error {
	if terminal == nil || terminal.Fence == nil || terminal.Correlation == nil {
		return errors.New("SecondBox duplicate Runner instance terminal evidence is incomplete")
	}
	reason, err := observedInstanceTerminationReason(terminal.Reason)
	if err != nil {
		return err
	}
	var (
		sandboxID, assignmentID, storedRunnerID, storedReason, storedDigest string
		requestID, operationID, leaseID                                     string
		generation                                                          int64
		observedAt                                                          time.Time
	)
	if err := store.pool.QueryRow(ctx, `
		SELECT sandbox_id,generation,assignment_id,runner_id,reason,evidence_digest,
		       observed_at,request_id,operation_id,lease_id
		FROM secondbox.instance_terminal_events WHERE instance_id=$1`,
		terminal.Fence.InstanceId,
	).Scan(
		&sandboxID, &generation, &assignmentID, &storedRunnerID,
		&storedReason, &storedDigest, &observedAt, &requestID, &operationID, &leaseID,
	); err != nil {
		return fmt.Errorf("SecondBox duplicate Runner instance terminal evidence lookup: %w", err)
	}
	correlation := terminal.Correlation
	if sandboxID != terminal.Fence.SandboxId ||
		generation != int64(terminal.Fence.SandboxGeneration) ||
		assignmentID != terminal.Fence.AssignmentId ||
		storedRunnerID != runnerID ||
		storedReason != reason ||
		storedDigest != terminal.TerminationEvidenceDigest ||
		!observedAt.Equal(time.UnixMilli(int64(terminal.ObservedAtUnixMs)).UTC()) ||
		requestID != correlation.RequestId ||
		operationID != correlation.OperationId ||
		leaseID != correlation.LeaseId {
		return errors.New("SecondBox duplicate Runner instance terminal evidence changed")
	}
	return nil
}

func recordInstanceTerminal(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	terminal *runnerv1.InstanceTerminal,
	now time.Time,
) error {
	if terminal == nil ||
		terminal.ObservedAtUnixMs == 0 ||
		len(terminal.TerminationEvidenceDigest) != len("sha256:")+64 ||
		!strings.HasPrefix(terminal.TerminationEvidenceDigest, "sha256:") {
		return errors.New("SecondBox Runner instance terminal evidence is incomplete")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(
		terminal.TerminationEvidenceDigest, "sha256:",
	)); err != nil {
		return errors.New("SecondBox Runner instance terminal digest is invalid")
	}
	reason, err := observedInstanceTerminationReason(terminal.Reason)
	if err != nil {
		return err
	}
	if err := lockAndValidateFence(ctx, tx, runnerID, terminal.Fence); err != nil {
		return err
	}
	if err := validateOperationCorrelation(runnerID, terminal.Fence, terminal.Correlation); err != nil {
		return err
	}
	var (
		assignmentState   string
		instanceState     string
		currentInstance   string
		sandboxGeneration int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT assignment.state,instance.state,sandbox.current_instance_id,sandbox.generation
		FROM secondbox.assignments AS assignment
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		WHERE assignment.id=$1
		FOR UPDATE OF instance,sandbox`,
		terminal.Fence.AssignmentId,
	).Scan(
		&assignmentState, &instanceState, &currentInstance, &sandboxGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox Runner instance terminal authority lookup: %w", err)
	}
	if assignmentState != "ready" ||
		currentInstance != terminal.Fence.InstanceId ||
		sandboxGeneration != int64(terminal.Fence.SandboxGeneration) {
		return ErrStaleAssignmentEvidence
	}
	correlation := terminal.Correlation
	var (
		storedSandboxID, storedAssignmentID, storedRunnerID string
		storedReason, storedDigest, storedRequestID         string
		storedOperationID, storedLeaseID                    string
		storedGeneration                                    int64
		storedObservedAt                                    time.Time
	)
	lookupErr := tx.QueryRow(ctx, `
		SELECT sandbox_id,generation,assignment_id,runner_id,reason,evidence_digest,
		       observed_at,request_id,operation_id,lease_id
		FROM secondbox.instance_terminal_events
		WHERE instance_id=$1 FOR UPDATE`,
		terminal.Fence.InstanceId,
	).Scan(
		&storedSandboxID, &storedGeneration, &storedAssignmentID, &storedRunnerID,
		&storedReason, &storedDigest, &storedObservedAt, &storedRequestID,
		&storedOperationID, &storedLeaseID,
	)
	observedAt := time.UnixMilli(int64(terminal.ObservedAtUnixMs)).UTC()
	if lookupErr == nil {
		if storedSandboxID != terminal.Fence.SandboxId ||
			storedGeneration != int64(terminal.Fence.SandboxGeneration) ||
			storedAssignmentID != terminal.Fence.AssignmentId ||
			storedRunnerID != runnerID ||
			storedReason != reason ||
			storedDigest != terminal.TerminationEvidenceDigest ||
			!storedObservedAt.Equal(observedAt) ||
			storedRequestID != correlation.RequestId ||
			storedOperationID != correlation.OperationId ||
			storedLeaseID != correlation.LeaseId {
			return errors.New("SecondBox Runner instance terminal evidence changed after persistence")
		}
		return nil
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox Runner instance terminal evidence lookup: %w", lookupErr)
	}
	if instanceState != "ready" {
		return ErrStaleAssignmentEvidence
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.instance_terminal_events (
			instance_id,sandbox_id,generation,assignment_id,runner_id,reason,evidence_digest,
			observed_at,request_id,operation_id,lease_id,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		terminal.Fence.InstanceId, terminal.Fence.SandboxId,
		terminal.Fence.SandboxGeneration, terminal.Fence.AssignmentId, runnerID,
		reason, terminal.TerminationEvidenceDigest, observedAt,
		correlation.RequestId, correlation.OperationId, correlation.LeaseId, now,
	); err != nil {
		return fmt.Errorf("SecondBox Runner instance terminal evidence insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='stopped',
		    termination_reason=CASE WHEN termination_reason='' THEN $2 ELSE termination_reason END,
		    stopped_at=$3,updated_at=$3
		WHERE id=$1`,
		terminal.Fence.InstanceId, reason, now,
	); err != nil {
		return fmt.Errorf("SecondBox Runner terminal Instance update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=$2,revision=revision+1,updated_at=$2
		WHERE id=$1 AND current_instance_id=$3 AND generation=$4`,
		terminal.Fence.SandboxId, now, terminal.Fence.InstanceId,
		terminal.Fence.SandboxGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox Runner terminal Sandbox wake: %w", err)
	}
	return nil
}

func observedInstanceTerminationReason(
	reason runnerv1.InstanceObservedTerminationReason,
) (string, error) {
	switch reason {
	case runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN:
		return "guest_shutdown", nil
	case runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_RESOURCE_EXHAUSTION:
		return "resource_exhaustion", nil
	case runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE:
		return "internal_failure", nil
	default:
		return "", errors.New("SecondBox Runner instance terminal reason is unsupported")
	}
}

func recordCheckpointEvent(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	message *runnerv1.RunnerToControlPlane,
	now time.Time,
) error {
	var (
		fence           *runnerv1.AssignmentFence
		checkpointID    string
		storageObjectID string
	)
	switch {
	case message.GetCheckpointChunk() != nil:
		chunk := message.GetCheckpointChunk()
		fence, checkpointID, storageObjectID = chunk.Fence, chunk.CheckpointId, chunk.StorageObjectId
	case message.GetCheckpointResult() != nil:
		result := message.GetCheckpointResult()
		fence, checkpointID, storageObjectID = result.Fence, result.CheckpointId, result.StorageObjectId
	default:
		return ErrRunnerMessage
	}
	if checkpointID == "" || storageObjectID == "" {
		return errors.New("SecondBox runner checkpoint identity is incomplete")
	}
	if err := lockAndValidateFence(ctx, tx, runnerID, fence); err != nil {
		return err
	}
	var effectState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.lifecycle_effects
		WHERE checkpoint_id=$1 AND storage_object_id=$2 AND assignment_id=$3
		  AND runner_id=$4 AND generation=$5 FOR UPDATE`,
		checkpointID, storageObjectID, fence.AssignmentId, runnerID, fence.SandboxGeneration,
	).Scan(&effectState); err != nil {
		return fmt.Errorf("SecondBox runner checkpoint effect lookup: %w", err)
	}
	if effectState == "published" {
		return nil
	}
	result := message.GetCheckpointResult()
	if result == nil {
		return nil
	}
	if err := validateOperationCorrelation(runnerID, result.Fence, result.Correlation); err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(map[string]any{
		"terminal": result.Terminal.String(), "sha256": result.Sha256,
		"sizeBytes": result.SizeBytes, "compatibility": result.Compatibility,
		"terminationEvidenceDigest": result.TerminationEvidenceDigest,
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner CheckpointResult evidence encoding: %w", err)
	}
	state := "runner_failed"
	if result.Terminal == runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED {
		if result.Sha256 == "" || result.SizeBytes == 0 || len(result.Compatibility) == 0 {
			return errors.New("SecondBox runner created CheckpointResult lacks integrity evidence")
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state=$2,evidence_json=$3,updated_at=$4
		WHERE checkpoint_id=$1`,
		checkpointID, state, evidenceJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner CheckpointResult update: %w", err)
	}
	return nil
}

func (store *PostgresStateStore) beginOrderedMessage(
	ctx context.Context,
	runnerID string,
	connectionID string,
	messageID string,
	sequence uint64,
	kind string,
	now time.Time,
) (pgx.Tx, bool, error) {
	if runnerID == "" || connectionID == "" || messageID == "" || sequence == 0 {
		return nil, false, fmt.Errorf("%w: runner message envelope is incomplete", ErrRunnerMessage)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("SecondBox runner message transaction: %w", err)
	}
	var storedRunnerID, connectionState string
	var lastSequence int64
	if err := tx.QueryRow(ctx, `
		SELECT connection.runner_id,connection.state,connection.last_sequence
		FROM secondbox.runner_connections AS connection
		WHERE connection.id=$1 FOR UPDATE OF connection`, connectionID,
	).Scan(&storedRunnerID, &connectionState, &lastSequence); err != nil {
		tx.Rollback(ctx)
		return nil, false, fmt.Errorf("SecondBox runner connection ordering lookup: %w", err)
	}
	if storedRunnerID != runnerID || connectionState != "active" {
		tx.Rollback(ctx)
		return nil, false, errors.New("SecondBox runner message connection identity is inactive")
	}
	var priorSequence int64
	err = tx.QueryRow(ctx, `
		SELECT sequence FROM secondbox.runner_messages
		WHERE connection_id=$1 AND message_id=$2`, connectionID, messageID,
	).Scan(&priorSequence)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("SecondBox duplicate runner message commit: %w", err)
		}
		return nil, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return nil, false, fmt.Errorf("SecondBox runner message duplicate lookup: %w", err)
	}
	if int64(sequence) <= lastSequence {
		tx.Rollback(ctx)
		return nil, false, ErrSequenceReordered
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_messages (
			connection_id,message_id,sequence,kind,observed_at
		) VALUES ($1,$2,$3,$4,$5)`,
		connectionID, messageID, sequence, kind, now.UTC(),
	); err != nil {
		tx.Rollback(ctx)
		return nil, false, fmt.Errorf("SecondBox runner message insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_connections
		SET last_sequence=$2,last_seen_at=$3 WHERE id=$1`,
		connectionID, sequence, now.UTC(),
	); err != nil {
		tx.Rollback(ctx)
		return nil, false, fmt.Errorf("SecondBox runner message sequence update: %w", err)
	}
	return tx, false, nil
}

func allPrerequisitesReady(capabilities map[string]bool) bool {
	for _, name := range []string{
		"firecracker", "kvm", "jailer", "cgroup", "network-policy", "storage", "cleanup",
	} {
		if !capabilities[name] {
			return false
		}
	}
	return true
}

func encodeProtocolCapacity(capacity *runnerv1.Capacity) ([]byte, error) {
	if capacity == nil {
		return nil, errors.New("SecondBox runner capacity is required")
	}
	encoded, err := json.Marshal(struct {
		CPUMillis   int64
		MemoryBytes int64
		DiskBytes   int64
		Instances   int64
		Operations  int64
	}{
		CPUMillis: int64(capacity.VcpuMillis), MemoryBytes: int64(capacity.MemoryBytes),
		DiskBytes: int64(capacity.DiskBytes), Instances: int64(capacity.Instances),
		Operations: int64(capacity.Operations),
	})
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner capacity encoding: %w", err)
	}
	return encoded, nil
}

func protocolDrainState(phase runnerv1.DrainPhase) (string, string, error) {
	switch phase {
	case runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE:
		return "active", "ready", nil
	case runnerv1.DrainPhase_DRAIN_PHASE_DRAINING:
		return "draining", "draining", nil
	case runnerv1.DrainPhase_DRAIN_PHASE_DRAINED:
		return "drained", "drained", nil
	default:
		return "", "", errors.New("SecondBox runner Heartbeat drain phase is unspecified")
	}
}

func recordAssignmentEvent(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	message *runnerv1.RunnerToControlPlane,
	now time.Time,
) error {
	switch {
	case message.GetAssignmentAck() != nil:
		ack := message.GetAssignmentAck()
		if err := lockAndValidateFence(ctx, tx, runnerID, ack.Fence); err != nil {
			return err
		}
		if err := validateAssignmentEvidenceState(
			ctx, tx, ack.Fence.AssignmentId, "assigned",
		); err != nil {
			return err
		}
		state := "accepted"
		failureClass := ""
		if ack.Decision != runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED {
			state = "failed"
			failureClass = assignmentDecisionFailureClass(ack.Decision)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state=$2,failure_class=$3,revision=revision+1,updated_at=$4 WHERE id=$1`,
			ack.Fence.AssignmentId, state, failureClass, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner AssignmentAck update: %w", err)
		}
	case message.GetAssignmentProgress() != nil:
		progress := message.GetAssignmentProgress()
		if err := lockAndValidateFence(ctx, tx, runnerID, progress.Fence); err != nil {
			return err
		}
		if err := validateOperationCorrelation(runnerID, progress.Fence, progress.Correlation); err != nil {
			return err
		}
		if err := validateAssignmentEvidenceState(
			ctx, tx, progress.Fence.AssignmentId, "assigned", "accepted", "starting",
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='starting',revision=revision+1,updated_at=$2 WHERE id=$1`,
			progress.Fence.AssignmentId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner AssignmentProgress update: %w", err)
		}
	case message.GetAssignmentResult() != nil:
		result := message.GetAssignmentResult()
		if err := lockAndValidateFence(ctx, tx, runnerID, result.Fence); err != nil {
			return err
		}
		if err := validateOperationCorrelation(runnerID, result.Fence, result.Correlation); err != nil {
			return err
		}
		if err := validateAssignmentEvidenceState(
			ctx, tx, result.Fence.AssignmentId, "assigned", "accepted", "starting",
		); err != nil {
			return err
		}
		if result.Terminal == runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY {
			if result.BackendKind == "" || result.BackendReference == "" {
				return errors.New("SecondBox runner ready AssignmentResult requires backend evidence")
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.assignments
				SET state='ready',backend_kind=$2,backend_reference=$3,
					revision=revision+1,updated_at=$4 WHERE id=$1`,
				result.Fence.AssignmentId, result.BackendKind, result.BackendReference, now,
			); err != nil {
				return fmt.Errorf("SecondBox runner ready AssignmentResult update: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.instances
				SET state='ready',guest_liveness='ready',ready_at=$2,updated_at=$2 WHERE id=$1`,
				result.Fence.InstanceId, now,
			); err != nil {
				return fmt.Errorf("SecondBox runner ready Instance update: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.workspace_materializations
				SET state='ready',revision=revision+1,updated_at=$2
				WHERE assignment_id=$1 AND generation=$3 AND state IN ('preparing','ready')`,
				result.Fence.AssignmentId, now, result.Fence.SandboxGeneration,
			); err != nil {
				return fmt.Errorf("SecondBox runner ready Workspace materialization update: %w", err)
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='failed',failure_class=$2,revision=revision+1,updated_at=$3 WHERE id=$1`,
			result.Fence.AssignmentId, assignmentTerminalFailureClass(result.Terminal), now,
		); err != nil {
			return fmt.Errorf("SecondBox runner failed AssignmentResult update: %w", err)
		}
	default:
		return ErrRunnerMessage
	}
	return nil
}

func validateAssignmentEvidenceState(
	ctx context.Context,
	tx pgx.Tx,
	assignmentID string,
	allowed ...string,
) error {
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.assignments WHERE id=$1 FOR UPDATE`,
		assignmentID,
	).Scan(&state); err != nil {
		return fmt.Errorf("SecondBox runner Assignment state lookup: %w", err)
	}
	for _, candidate := range allowed {
		if state == candidate {
			return nil
		}
	}
	return ErrStaleAssignmentEvidence
}

func recordFenceEvent(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	result *runnerv1.FenceResult,
	now time.Time,
) error {
	if result == nil {
		return ErrRunnerMessage
	}
	if err := lockAndValidateFence(ctx, tx, runnerID, result.Fence); err != nil {
		return err
	}
	if err := validateOperationCorrelation(runnerID, result.Fence, result.Correlation); err != nil {
		return err
	}
	if result.Result != runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED &&
		result.Result != runnerv1.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='uncertain',failure_class='fencing',revision=revision+1,updated_at=$2
			WHERE id=$1`, result.Fence.AssignmentId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner failed FenceResult update: %w", err)
		}
		return nil
	}
	if result.TerminationEvidenceDigest == "" {
		return errors.New("SecondBox runner successful FenceResult requires termination evidence")
	}
	proofJSON, err := json.Marshal(map[string]string{
		"terminationEvidenceDigest": result.TerminationEvidenceDigest,
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner FenceResult proof encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET state='fenced',release_proof_json=$2,revision=revision+1,updated_at=$3
		WHERE id=$1`, result.Fence.AssignmentId, proofJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner successful FenceResult update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state='runner_succeeded',evidence_json=$2,updated_at=$3
		WHERE assignment_id=$1 AND kind='stop' AND state='queued'`,
		result.Fence.AssignmentId, proofJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner successful stop effect update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.instances AS instance
		SET state='stopped',guest_liveness='stopped',
		    termination_reason=CASE
		      WHEN instance.termination_reason<>'' THEN instance.termination_reason
		      WHEN COALESCE(sandbox.lifecycle_termination_reason,'')<>''
		        THEN sandbox.lifecycle_termination_reason
		      WHEN assignment.failure_class='fencing' THEN 'runner_lost'
		      WHEN assignment.failure_class='startup_timeout' THEN 'startup_failed'
		      ELSE 'fenced'
		    END,
		    stopped_at=$2,updated_at=$2
		FROM secondbox.sandboxes AS sandbox,secondbox.assignments AS assignment
		WHERE instance.id=$1
		  AND sandbox.id=instance.sandbox_id
		  AND assignment.id=$3
		  AND assignment.instance_id=instance.id
		  AND sandbox.current_instance_id=instance.id
		  AND sandbox.generation=instance.generation`,
		result.Fence.InstanceId, now, result.Fence.AssignmentId,
	); err != nil {
		return fmt.Errorf("SecondBox runner fenced Instance update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_materializations
		SET state='released',release_proof_json=$2,revision=revision+1,
		    released_at=$3,updated_at=$3
		WHERE assignment_id=$1 AND generation=$4 AND state IN ('preparing','ready')`,
		result.Fence.AssignmentId, proofJSON, now, result.Fence.SandboxGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox runner fenced Workspace materialization release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=$2,last_activity_at=$2
		WHERE sandbox_id=$1 AND generation=$3 AND state='active'`,
		result.Fence.SandboxId, now, result.Fence.SandboxGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox runner fenced activity session closure: %w", err)
	}
	return nil
}

func validateOperationCorrelation(
	runnerID string,
	fence *runnerv1.AssignmentFence,
	correlation *runnerv1.Correlation,
) error {
	if fence == nil || correlation == nil ||
		correlation.RequestId == "" ||
		correlation.OperationId == "" ||
		correlation.SandboxId != fence.SandboxId ||
		correlation.InstanceId != fence.InstanceId ||
		correlation.SandboxGeneration != fence.SandboxGeneration ||
		correlation.AssignmentId != fence.AssignmentId ||
		correlation.RunnerId != runnerID {
		return errors.New("SecondBox runner operation correlation does not match durable authority")
	}
	return nil
}

func validateRunnerEvidence(runnerID string, evidence *runnerv1.Evidence) error {
	if evidence == nil ||
		evidence.Event == "" ||
		evidence.Outcome == "" ||
		evidence.TerminalKind == "" ||
		evidence.ObservedAtUnixMs == 0 ||
		evidence.Correlation == nil ||
		evidence.Correlation.RequestId == "" ||
		evidence.Correlation.OperationId == "" ||
		evidence.Correlation.SandboxId == "" ||
		evidence.Correlation.InstanceId == "" ||
		evidence.Correlation.SandboxGeneration == 0 ||
		evidence.Correlation.AssignmentId == "" ||
		evidence.Correlation.RunnerId != runnerID {
		return errors.New("SecondBox runner Evidence is incomplete or mismatched")
	}
	return nil
}

func recordDrainEvent(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	state *runnerv1.DrainState,
	now time.Time,
) error {
	if state == nil {
		return ErrRunnerMessage
	}
	drainPhase, runnerState, err := protocolDrainState(state.Phase)
	if err != nil {
		return err
	}
	if state.Phase == runnerv1.DrainPhase_DRAIN_PHASE_DRAINED &&
		len(state.RemainingAssignments) != 0 {
		return errors.New("SecondBox runner drained state retains active assignments")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runners
		SET state=$2,drain_phase=$3,revision=revision+1,updated_at=$4 WHERE id=$1`,
		runnerID, runnerState, drainPhase, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner DrainState update: %w", err)
	}
	return nil
}

func lockAndValidateFence(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	fence *runnerv1.AssignmentFence,
) error {
	if fence == nil || fence.AssignmentId == "" || fence.SandboxId == "" ||
		fence.InstanceId == "" || fence.SandboxGeneration == 0 ||
		len(fence.FencingToken) == 0 {
		return ErrStaleAssignmentEvidence
	}
	var storedSandboxID, storedInstanceID, storedRunnerID string
	var storedGeneration int64
	var storedToken []byte
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id,instance_id,runner_id,generation,fencing_token
		FROM secondbox.assignments WHERE id=$1 FOR UPDATE`, fence.AssignmentId,
	).Scan(
		&storedSandboxID, &storedInstanceID, &storedRunnerID,
		&storedGeneration, &storedToken,
	); err != nil {
		return fmt.Errorf("SecondBox runner Assignment fence lookup: %w", err)
	}
	if storedSandboxID != fence.SandboxId ||
		storedInstanceID != fence.InstanceId ||
		storedRunnerID != runnerID ||
		storedGeneration != int64(fence.SandboxGeneration) ||
		!bytes.Equal(storedToken, fence.FencingToken) {
		return ErrStaleAssignmentEvidence
	}
	return nil
}

var ErrStaleAssignmentEvidence = errors.New("SecondBox runner result has stale assignment fencing")

func assignmentDecisionFailureClass(decision runnerv1.AssignmentDecision) string {
	switch decision {
	case runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY:
		return "transient"
	case runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_INCOMPATIBLE_PROFILE:
		return "compatibility"
	case runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_FENCED:
		return "fencing"
	case runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_ARTIFACT:
		return "integrity"
	default:
		return "admission"
	}
}

func assignmentTerminalFailureClass(terminal runnerv1.AssignmentTerminalKind) string {
	switch terminal {
	case runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED,
		runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_RUNNER_FAILED:
		return "transient"
	case runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_FENCED:
		return "fencing"
	default:
		return "admission"
	}
}
