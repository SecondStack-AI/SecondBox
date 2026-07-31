package runnercontrol

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
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
	reconcileID := "workspace-reconcile-" + connectionID
	reconcilePayload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
			LocalWorkspace: &runnerv1.LocalWorkspaceCommand{
				CommandVersion: 1,
				Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
				OperationId:    reconcileID,
				EffectId:       reconcileID,
				Correlation: &runnerv1.Correlation{
					RequestId:   reconcileID,
					OperationId: reconcileID,
					RunnerId:    identity.RunnerID,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation command encoding: %w", err)
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
		WHERE runner_id=$1 AND state IN ('delivering','delivered')`,
		identity.RunnerID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner unacknowledged command recovery: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='expired',target_connection_id='',updated_at=$2
		WHERE runner_id=$1
		  AND assignment_id LIKE 'workspace-reconcile-%'
		  AND state IN ('pending','delivering','delivered')`,
		identity.RunnerID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner prior Workspace reconciliation expiry: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$1,'local-workspace',$3,'pending','',0,$4,$4,NULL)`,
		reconcileID, identity.RunnerID, reconcilePayload, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation command insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox runner connection commit: %w", err)
	}
	return nil
}

// CloseConnection makes the currently active runner immediately unschedulable.
func (store *PostgresStateStore) CloseConnection(
	ctx context.Context,
	runnerID string,
	connectionID string,
	now time.Time,
) error {
	if runnerID == "" || connectionID == "" {
		return errors.New("SecondBox runner connection close requires runner and connection identifiers")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox runner connection close transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	now = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_connections
		SET state='disconnected',disconnected_at=$3,last_seen_at=$3
		WHERE id=$1 AND runner_id=$2 AND state='active'`,
		connectionID, runnerID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner connection close: %w", err)
	}
	var poolName string
	err = tx.QueryRow(ctx, `
		UPDATE secondbox.runners
		SET state='offline',revision=revision+1,updated_at=$3
		WHERE id=$1 AND active_connection_id=$2
		RETURNING pool_name`,
		runnerID, connectionID, now,
	).Scan(&poolName)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox runner offline transition: %w", err)
	}
	if err == nil {
		if err := refreshReadyRunnerCount(ctx, tx, poolName, now); err != nil {
			return err
		}
		if err := failDisconnectedRunnerDataPlaneSessions(
			ctx,
			tx,
			runnerID,
			now,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox runner connection close commit: %w", err)
	}
	return nil
}

func failDisconnectedRunnerDataPlaneSessions(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions AS activity
		SET state='closed',closed_at=$2,last_activity_at=$2,updated_at=$2
		FROM secondbox.data_plane_sessions AS session
		WHERE activity.id=session.id
		  AND session.runner_id=$1
		  AND session.state IN ('pending','running','cancelling')
		  AND activity.state='active'`,
		runnerID,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox disconnected runner activity closure: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions AS port
		SET state='closed',closed_at=$2,updated_at=$2
		FROM secondbox.data_plane_sessions AS session
		WHERE port.data_plane_session_id=session.id
		  AND session.runner_id=$1
		  AND session.state IN ('pending','running','cancelling')
		  AND port.state IN ('open','closing')`,
		runnerID,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox disconnected runner PortSession closure: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state='failed',
		    terminal_kind=CASE
		      WHEN kind IN ('exec','terminal') THEN $2
		      WHEN kind='port' THEN $3
		      ELSE $4
		    END,
		    terminal_detail='Execution node connection was lost',
		    infrastructure_failure_reason=$5,
		    retryable=true,
		    terminal_message='Execution node connection was lost',
		    completed_at=$6,updated_at=$6,retain_until=GREATEST(retain_until,$6)
		WHERE runner_id=$1 AND state IN ('pending','running','cancelling')`,
		runnerID,
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED.String(),
		runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE.String(),
		runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_FAILED.String(),
		runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE.String(),
		now,
	); err != nil {
		return fmt.Errorf("SecondBox disconnected runner data-plane failure: %w", err)
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
	prerequisites := map[string]bool{
		"firecracker":    registration.Capabilities != nil && registration.Capabilities.FirecrackerVersion != "",
		"kvm":            registration.Capabilities != nil && registration.Capabilities.KvmReady,
		"jailer":         registration.Capabilities != nil && registration.Capabilities.JailerReady,
		"cgroup":         registration.Capabilities != nil && registration.Capabilities.CgroupReady,
		"network-policy": registration.Capabilities != nil && registration.Capabilities.NetworkPolicyReady,
		"storage":        registration.Capabilities != nil && registration.Capabilities.StorageReady,
		"cleanup":        registration.Capabilities != nil && registration.Capabilities.CleanupReady,
	}
	prerequisites["local-workspace"] = prerequisites["storage"] && prerequisites["cleanup"]
	if registration.Capabilities == nil ||
		registration.Capabilities.GuestProtocolGenerations == nil ||
		registration.Capabilities.GuestProtocolGenerations.Minimum == 0 ||
		registration.Capabilities.GuestProtocolGenerations.Minimum >
			registration.Capabilities.GuestProtocolGenerations.Maximum ||
		!allPrerequisitesReady(prerequisites) {
		return false, ErrRunnerPrerequisites
	}
	capabilities := []string{
		"compute", "network-policy", "storage", "cleanup", "local-workspace",
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
	versionsJSON, err := publicProtocolVersionsJSON(registration.ProtocolVersion)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner protocol versions encoding: %w", err)
	}
	cache := struct {
		ArtifactDigests []string `json:"artifactDigests"`
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
	startCount, startP95Milliseconds, err := protocolStartupTiming(registration.StartupTiming)
	if err != nil {
		return false, err
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
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES ($1,$2,$1,'connected','[]','{}','{}','[]',0,0,'',$3,0,'active','{}','[]',0,0,NULL,1,$4,$4)
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
			artifact_cache_json=$12,sandbox_start_sample_count=$13,
			sandbox_start_p95_milliseconds=$14,last_seen_at=$15,
			revision=revision+1,updated_at=$15
		WHERE id=$1 AND pool_name=$2 AND active_connection_id=$16`,
		registration.RunnerId, registration.RunnerPoolId, architecturesJSON, capabilitiesJSON,
		allocatableJSON, versionsJSON,
		registration.Capabilities.GuestProtocolGenerations.Minimum,
		registration.Capabilities.GuestProtocolGenerations.Maximum,
		registration.SoftwareVersion, registration.Sequence,
		reservedJSON, cacheJSON, startCount, startP95Milliseconds,
		now.UTC(), registration.ConnectionId,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Registration update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("SecondBox runner Registration connection is no longer active")
	}
	if err := refreshReadyRunnerCount(
		ctx,
		tx,
		registration.RunnerPoolId,
		now.UTC(),
	); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner Registration commit: %w", err)
	}
	return false, nil
}

func publicProtocolVersionsJSON(protocolVersion uint32) ([]byte, error) {
	return json.Marshal([]string{strconv.FormatUint(uint64(protocolVersion), 10)})
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
	reportedReserved, err := runnerCapacityFromProtocol(heartbeat.Reserved)
	if err != nil {
		return false, err
	}
	drainPhase, runnerState, err := protocolDrainState(heartbeat.DrainPhase)
	if err != nil {
		return false, err
	}
	startCount, startP95Milliseconds, err := protocolStartupTiming(heartbeat.StartupTiming)
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
	if err := lockRunnerReservation(
		ctx,
		tx,
		heartbeat.RunnerId,
		heartbeat.ConnectionId,
	); err != nil {
		return false, err
	}
	if err := reconcileRunnerActiveAssignments(
		ctx,
		tx,
		heartbeat.RunnerId,
		heartbeat.ActiveAssignments,
		now.UTC(),
	); err != nil {
		return false, err
	}
	durableReserved, err := durableRunnerReservation(
		ctx,
		tx,
		heartbeat.RunnerId,
	)
	if err != nil {
		return false, err
	}
	reservedJSON, err := encodeRunnerCapacity(maxRunnerCapacity(reportedReserved, durableReserved))
	if err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runners
		SET state=$2,capacity_json=$3,reserved_capacity_json=$4,last_sequence=$5,
			drain_phase=$6,sandbox_start_sample_count=$7,
			sandbox_start_p95_milliseconds=$8,last_seen_at=$9,
			revision=revision+1,updated_at=$9
		WHERE id=$1 AND active_connection_id=$10`,
		heartbeat.RunnerId, runnerState, allocatableJSON, reservedJSON,
		heartbeat.Sequence, drainPhase, startCount, startP95Milliseconds,
		now.UTC(), heartbeat.ConnectionId,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Heartbeat update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, errors.New("SecondBox runner Heartbeat connection is no longer active")
	}
	var poolName string
	if err := tx.QueryRow(
		ctx,
		`SELECT pool_name FROM secondbox.runners WHERE id=$1`,
		heartbeat.RunnerId,
	).Scan(&poolName); err != nil {
		return false, fmt.Errorf("SecondBox runner Heartbeat pool lookup: %w", err)
	}
	if err := refreshReadyRunnerCount(ctx, tx, poolName, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner Heartbeat commit: %w", err)
	}
	return false, nil
}

func reconcileRunnerActiveAssignments(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	reported []*runnerv1.ActiveAssignmentSummary,
	now time.Time,
) error {
	reportedByID := make(map[string]*runnerv1.ActiveAssignmentSummary, len(reported))
	for _, summary := range reported {
		if summary == nil ||
			summary.AssignmentId == "" ||
			summary.SandboxId == "" ||
			summary.InstanceId == "" ||
			summary.SandboxGeneration == 0 ||
			len(summary.FencingToken) == 0 {
			return errors.New("SecondBox runner Heartbeat active Assignment evidence is incomplete")
		}
		if _, duplicate := reportedByID[summary.AssignmentId]; duplicate {
			return errors.New("SecondBox runner Heartbeat active Assignment evidence is duplicated")
		}
		reportedByID[summary.AssignmentId] = summary
	}
	rows, err := tx.Query(ctx, `
		SELECT id,sandbox_id,instance_id,generation,fencing_token
		FROM secondbox.assignments
		WHERE runner_id=$1 AND state='ready'
		ORDER BY id
		FOR UPDATE`,
		runnerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner Heartbeat active Assignment lookup: %w", err)
	}
	type activeAssignment struct {
		id           string
		sandboxID    string
		instanceID   string
		generation   int64
		fencingToken []byte
	}
	missing := make([]activeAssignment, 0)
	for rows.Next() {
		var assignment activeAssignment
		if err := rows.Scan(
			&assignment.id,
			&assignment.sandboxID,
			&assignment.instanceID,
			&assignment.generation,
			&assignment.fencingToken,
		); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox runner Heartbeat active Assignment scan: %w", err)
		}
		summary, found := reportedByID[assignment.id]
		if found {
			if summary.SandboxId != assignment.sandboxID ||
				summary.InstanceId != assignment.instanceID ||
				summary.SandboxGeneration != uint64(assignment.generation) ||
				!bytes.Equal(summary.FencingToken, assignment.fencingToken) {
				rows.Close()
				return errors.New("SecondBox runner Heartbeat active Assignment evidence conflicts with durable authority")
			}
			continue
		}
		missing = append(missing, assignment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SecondBox runner Heartbeat active Assignment iteration: %w", err)
	}
	for _, assignment := range missing {
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='uncertain',failure_class='transient',next_reconcile_at=$2,
			    reconcile_owner='',reconcile_claim_expires_at=$2,
			    revision=revision+1,updated_at=$2
			WHERE id=$1 AND state='ready'`,
			assignment.id,
			now,
		)
		if err != nil {
			return fmt.Errorf("SecondBox runner Heartbeat missing Assignment transition: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("SecondBox runner Heartbeat missing Assignment changed concurrently")
		}
	}
	return nil
}

func lockRunnerReservation(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	connectionID string,
) error {
	var lockedRunnerID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM secondbox.runners
		WHERE id=$1 AND active_connection_id=$2
		FOR UPDATE`,
		runnerID,
		connectionID,
	).Scan(&lockedRunnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("SecondBox runner Heartbeat connection is no longer active")
	}
	if err != nil {
		return fmt.Errorf("SecondBox runner Heartbeat reservation lock: %w", err)
	}
	return nil
}

func durableRunnerReservation(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
) (runnerCapacity, error) {
	var capacity runnerCapacity
	if err := tx.QueryRow(ctx, `
		SELECT
		  COALESCE(sum((revision.spec_json->'resources'->>'cpuMillis')::bigint),0),
		  COALESCE(sum((revision.spec_json->'resources'->>'memoryBytes')::bigint),0),
		  COALESCE(sum((revision.spec_json->'resources'->>'workspaceBytes')::bigint),0),
		  count(*),
		  COALESCE(sum((revision.spec_json->'resources'->>'concurrentOperations')::bigint),0)
		FROM secondbox.assignments AS assignment
		JOIN secondbox.profile_revisions AS revision
		  ON revision.id=assignment.profile_revision_id
		JOIN secondbox.sandboxes AS sandbox
		  ON sandbox.id=assignment.sandbox_id
		  AND sandbox.current_instance_id=assignment.instance_id
		  AND sandbox.generation=assignment.generation
		WHERE assignment.runner_id=$1
		  AND assignment.state IN ('assigned','accepted','starting','ready','uncertain')`,
		runnerID,
	).Scan(
		&capacity.CPUMillis,
		&capacity.MemoryBytes,
		&capacity.DiskBytes,
		&capacity.Instances,
		&capacity.Operations,
	); err != nil {
		return runnerCapacity{}, fmt.Errorf("SecondBox runner Heartbeat durable reservation lookup: %w", err)
	}
	return capacity, nil
}

func refreshReadyRunnerCount(
	ctx context.Context,
	tx pgx.Tx,
	poolName string,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_pools AS pool
		SET ready_runner_count=(
			SELECT count(*)
			FROM secondbox.runners AS runner
			WHERE runner.pool_name=pool.name AND runner.state='ready'
		),updated_at=$2
		WHERE pool.name=$1`,
		poolName,
		now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox RunnerPool ready count refresh: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("SecondBox RunnerPool ready count refresh found no pool")
	}
	return nil
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
	if err := recordDurableEvent(ctx, tx, event, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox runner event commit: %w", err)
	}
	return false, nil
}

// RecordEvents persists one bounded, same-connection event sequence atomically.
func (store *PostgresStateStore) RecordEvents(
	ctx context.Context,
	records []EventPersistenceRecord,
) error {
	if len(records) == 0 {
		return errors.New("SecondBox runner event persistence batch is empty")
	}
	if len(records) == 1 {
		_, err := store.RecordEvent(ctx, records[0].Event, records[0].ReceivedAt)
		return err
	}
	runnerID := records[0].Event.RunnerID
	connectionID := records[0].Event.ConnectionID
	messageIDs := make([]string, len(records))
	sequences := make([]uint64, len(records))
	for index, record := range records {
		if record.Event.RunnerID != runnerID ||
			record.Event.ConnectionID != connectionID {
			return errors.New("SecondBox runner event persistence batch spans connection identities")
		}
		if !durableRunnerEvent(record.Event.Kind) {
			return fmt.Errorf(
				"SecondBox runner event persistence batch contains non-durable kind %q",
				record.Event.Kind,
			)
		}
		messageID, sequence, err := runnerEnvelope(record.Event.Message)
		if err != nil {
			return err
		}
		messageIDs[index] = messageID
		sequences[index] = sequence
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox runner event batch transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var storedRunnerID, connectionState string
	var lastSequence int64
	if err := tx.QueryRow(ctx, `
		SELECT connection.runner_id,connection.state,connection.last_sequence
		FROM secondbox.runner_connections AS connection
		WHERE connection.id=$1 FOR UPDATE OF connection`, connectionID,
	).Scan(&storedRunnerID, &connectionState, &lastSequence); err != nil {
		return fmt.Errorf("SecondBox runner event batch connection ordering lookup: %w", err)
	}
	if storedRunnerID != runnerID || connectionState != "active" {
		return errors.New("SecondBox runner event batch connection identity is inactive")
	}
	var containsDuplicate bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.runner_messages
			WHERE connection_id=$1 AND message_id=ANY($2::text[])
		)`,
		connectionID,
		messageIDs,
	).Scan(&containsDuplicate); err != nil {
		return fmt.Errorf("SecondBox runner event batch duplicate lookup: %w", err)
	}
	if containsDuplicate {
		if err := tx.Rollback(ctx); err != nil {
			return fmt.Errorf("SecondBox runner event batch duplicate rollback: %w", err)
		}
		for _, record := range records {
			if _, err := store.RecordEvent(ctx, record.Event, record.ReceivedAt); err != nil {
				return err
			}
		}
		return nil
	}
	for _, sequence := range sequences {
		if sequence > uint64(^uint64(0)>>1) || int64(sequence) <= lastSequence {
			return ErrSequenceReordered
		}
		lastSequence = int64(sequence)
	}
	orderedWrites := &pgx.Batch{}
	for index, record := range records {
		orderedWrites.Queue(`
			INSERT INTO secondbox.runner_messages (
				connection_id,message_id,sequence,kind,observed_at
			) VALUES ($1,$2,$3,$4,$5)`,
			connectionID,
			messageIDs[index],
			sequences[index],
			string(record.Event.Kind),
			record.ReceivedAt.UTC(),
		)
	}
	orderedWrites.Queue(`
		UPDATE secondbox.runner_connections
		SET last_sequence=$2,last_seen_at=$3 WHERE id=$1`,
		connectionID,
		sequences[len(sequences)-1],
		records[len(records)-1].ReceivedAt.UTC(),
	)
	results := tx.SendBatch(ctx, orderedWrites)
	var writeErr error
	for range records {
		if _, err := results.Exec(); err != nil && writeErr == nil {
			writeErr = fmt.Errorf("SecondBox runner event batch message insert: %w", err)
		}
	}
	if command, err := results.Exec(); err != nil {
		writeErr = errors.Join(
			writeErr,
			fmt.Errorf("SecondBox runner event batch sequence update: %w", err),
		)
	} else if command.RowsAffected() != 1 {
		writeErr = errors.Join(
			writeErr,
			errors.New("SecondBox runner event batch sequence update found no connection"),
		)
	}
	if err := results.Close(); err != nil {
		writeErr = errors.Join(
			writeErr,
			fmt.Errorf("SecondBox runner event batch ordered writes close: %w", err),
		)
	}
	if writeErr != nil {
		return writeErr
	}
	for _, record := range records {
		if err := recordDurableEvent(
			ctx,
			tx,
			record.Event,
			record.ReceivedAt.UTC(),
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox runner event batch commit: %w", err)
	}
	return nil
}

func recordDurableEvent(
	ctx context.Context,
	tx pgx.Tx,
	event Event,
	now time.Time,
) error {
	switch event.Kind {
	case EventAssignment:
		if err := recordAssignmentEvent(ctx, tx, event.RunnerID, event.Message, now.UTC()); err != nil {
			return err
		}
	case EventFence:
		if err := recordFenceEvent(ctx, tx, event.RunnerID, event.Message.GetFenceResult(), now.UTC()); err != nil {
			return err
		}
	case EventDrain:
		if err := recordDrainEvent(ctx, tx, event.RunnerID, event.Message.GetDrainState(), now.UTC()); err != nil {
			return err
		}
	case EventEvidence:
		if err := validateRunnerEvidence(event.RunnerID, event.Message.GetEvidence()); err != nil {
			return err
		}
	case EventLocalWorkspace:
		if err := recordLocalWorkspaceResult(
			ctx, tx, event.RunnerID, event.Message.GetLocalWorkspaceResult(), now.UTC(),
		); err != nil {
			return err
		}
	case EventInstanceTerminal:
		if err := recordInstanceTerminal(
			ctx, tx, event.RunnerID, event.Message.GetInstanceTerminal(), now.UTC(),
		); err != nil {
			return err
		}
	default:
		return fmt.Errorf("SecondBox runner event kind %q is not durable", event.Kind)
	}
	return nil
}

func recordLocalWorkspaceResult(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	if err := validateLocalWorkspaceResult(runnerID, result); err != nil {
		return err
	}
	if result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE {
		return recordLocalWorkspaceReconciliation(ctx, tx, runnerID, result, now)
	}
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, result.SandboxId)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("SecondBox runner local-workspace result has no durable Sandbox")
	}
	if err != nil {
		return fmt.Errorf("SecondBox runner local-workspace Sandbox/Workspace lock: %w", err)
	}
	if locked.WorkspaceID != result.WorkspaceId {
		return errors.New("SecondBox runner local-workspace result targets the wrong Workspace")
	}
	var snapshot rowlock.Snapshot
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		snapshot, err = rowlock.SnapshotByID(ctx, tx, locked, result.SnapshotId)
		if err != nil {
			return errors.New("SecondBox runner local-workspace result has no durable Snapshot")
		}
	}
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION:
		return recordLocalGenerationAdvanceResult(ctx, tx, locked, runnerID, result, now)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE:
		return recordLocalWorkspaceDeleteResult(ctx, tx, locked, runnerID, result, now)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		return recordLocalRestoreResult(ctx, tx, locked, snapshot, runnerID, result, now)
	}
	if result.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE &&
		result.Kind !=
			runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT {
		return recordLocalSnapshotResult(ctx, tx, locked, snapshot, runnerID, result, now)
	}
	effect, err := lockLocalWorkspaceEffect(ctx, tx, result.EffectId)
	if err != nil {
		return err
	}
	workspace := locked.Workspace
	if workspace.HomeRunnerID != runnerID {
		return errors.New("SecondBox runner local-workspace result came from the wrong home runner")
	}
	if workspace.Mutation.ID != result.EffectId || workspace.Mutation.State == "" ||
		workspace.Mutation.OperationID != result.OperationId ||
		workspace.Generation != int64(result.Generation) ||
		workspace.LogicalCapacityBytes != int64(result.LogicalCapacityBytes) {
		return errors.New("SecondBox runner local-workspace result conflicts with durable authority")
	}
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE:
		if workspace.Mutation.Kind != "create" ||
			effect.kind != "local_workspace_create" ||
			effect.storageObjectID != "" ||
			result.SnapshotId != "" {
			return errors.New("SecondBox runner Workspace create result conflicts with durable effect authority")
		}
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT:
		if workspace.Mutation.Kind != "clone" ||
			effect.kind != "local_workspace_clone" ||
			effect.storageObjectID == "" ||
			effect.storageObjectID != result.SnapshotId {
			return errors.New("SecondBox runner Workspace clone result conflicts with durable Snapshot authority")
		}
	}
	if effect.state == "succeeded" || effect.state == "runner_failed" {
		return nil
	}
	if workspace.State != "creating" || effect.state != "queued" {
		return errors.New("SecondBox runner local-workspace result is reordered")
	}
	evidenceJSON, err := localWorkspaceEvidence(result, false, false)
	if err != nil {
		return err
	}
	if result.Terminal == runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED {
		if result.ReceiptRecordedAtUnixMs == 0 {
			return errors.New("SecondBox runner local-workspace success lacks a durable receipt")
		}
		keepStartMutation := locked.DesiredState == "running"
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET state='ready',local_receipt_json=$2,
			    mutation_kind=CASE WHEN $4 THEN 'start' ELSE '' END,
			    mutation_id=CASE WHEN $4 THEN $5 ELSE '' END,
			    mutation_effect_id=CASE WHEN $4 THEN $5 ELSE '' END,
			    mutation_operation_id=CASE WHEN $4 THEN $5 ELSE '' END,
			    mutation_expected_generation=CASE WHEN $4 THEN $6::bigint ELSE NULL END,
			    mutation_target_generation=CASE WHEN $4 THEN $6::bigint ELSE NULL END,
			    mutation_state=CASE WHEN $4 THEN 'queued' ELSE '' END,
			    updated_at=$3
			WHERE id=$1`,
			result.WorkspaceId, evidenceJSON, now, keepStartMutation,
			workspace.Mutation.OperationID, workspace.Generation,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace create or clone completion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET state='stopped',lifecycle_intent_kind='',
			    reconcile_owner='',reconcile_claim_expires_at=NULL,
			    next_reconcile_at=CASE
			      WHEN desired_state IN ('running','deleted') THEN $2::timestamptz
			      ELSE NULL::timestamptz
			    END,
			    revision=revision+1,updated_at=$2
			WHERE id=$1`,
			result.SandboxId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Sandbox create completion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.operation_stage_timings (
				operation_id,sandbox_id,stage,observed_at
			) VALUES ($1,$2,'workspace_ready',$3)
			ON CONFLICT (operation_id,stage) DO NOTHING`,
			workspace.Mutation.OperationID,
			result.SandboxId,
			now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace-ready timing insert: %w", err)
		}
		if !keepStartMutation {
			if err := finishPendingOperation(
				ctx, tx, workspace.Mutation.OperationID, "succeeded", "", "", now,
			); err != nil {
				return err
			}
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "succeeded", "", "", evidenceJSON, now,
		); err != nil {
			return err
		}
	} else {
		errorCode := localWorkspaceOperationErrorCode(result.Terminal)
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET state='failed',local_receipt_json=$2,mutation_state='failed',updated_at=$3
			WHERE id=$1`,
			result.WorkspaceId, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace create failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET state='failed',lifecycle_failure_class=$2,
			    lifecycle_failure_message=$3,
			    reconcile_owner='',reconcile_claim_expires_at=NULL,
			    next_reconcile_at=CASE
			      WHEN desired_state='deleted' THEN $4::timestamptz
			      ELSE next_reconcile_at
			    END,
			    revision=revision+1,updated_at=$4
			WHERE id=$1`,
			result.SandboxId, errorCode, result.SafeDetail, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Sandbox create failure: %w", err)
		}
		if err := finishPendingOperation(
			ctx, tx, workspace.Mutation.OperationID, "failed", errorCode, result.SafeDetail, now,
		); err != nil {
			return err
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "runner_failed",
			errorCode, result.SafeDetail, evidenceJSON, now,
		); err != nil {
			return err
		}
	}
	return acknowledgeRunnerCommand(ctx, tx, effect.commandID, now)
}

func recordLocalWorkspaceDeleteResult(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	effect, err := lockLocalWorkspaceEffect(ctx, tx, result.EffectId)
	if err != nil {
		return err
	}
	workspace := locked.Workspace
	if effect.state == "succeeded" {
		if workspace.HomeRunnerID == runnerID &&
			workspace.State == "deleted" &&
			locked.SandboxState == "deleted" &&
			workspace.Generation == int64(result.Generation) {
			return nil
		}
		return errors.New("SecondBox runner Workspace delete completion replay conflicts with durable authority")
	}
	if workspace.HomeRunnerID != runnerID ||
		workspace.State != "deleting" ||
		locked.SandboxState != "deleting" ||
		locked.DesiredState != "deleted" ||
		workspace.Mutation.Kind != "workspace_delete" ||
		workspace.Mutation.ID != result.EffectId ||
		workspace.Mutation.EffectID != result.EffectId ||
		workspace.Mutation.OperationID != result.OperationId ||
		workspace.Mutation.State == "" ||
		workspace.Generation != int64(result.Generation) {
		return errors.New("SecondBox runner Workspace delete result conflicts with durable authority")
	}
	if result.LogicalCapacityBytes != 0 &&
		result.LogicalCapacityBytes != uint64(workspace.LogicalCapacityBytes) {
		return errors.New("SecondBox runner Workspace delete receipt has inconsistent capacity")
	}
	if effect.state == "runner_failed" {
		return nil
	}
	if effect.state != "queued" {
		return errors.New("SecondBox runner Workspace delete result is reordered")
	}
	evidenceJSON, err := localWorkspaceEvidence(result, false, false)
	if err != nil {
		return err
	}
	succeeded := result.Terminal ==
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if succeeded && result.ReceiptRecordedAtUnixMs == 0 {
		return errors.New("SecondBox runner Workspace delete success lacks a durable receipt")
	}
	if succeeded {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.snapshots
			SET state='deleted',retention_ended_at=COALESCE(retention_ended_at,$2),updated_at=$2
			WHERE workspace_id=$1 AND state<>'deleted'`,
			result.WorkspaceId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete Snapshot finalization: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET state='deleted',local_receipt_json=$2,
			    mutation_kind='',mutation_id='',mutation_effect_id='',
			    mutation_operation_id='',mutation_expected_generation=NULL,
			    mutation_target_generation=NULL,mutation_state='',updated_at=$3
			WHERE id=$1 AND mutation_kind='workspace_delete'`,
			result.WorkspaceId, evidenceJSON, now,
		)
		if err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete completion: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("SecondBox runner Workspace delete mutation changed before completion")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET state='deleted',current_instance_id='',deleted_at=$2,
			    next_reconcile_at=NULL,reconcile_owner='',
			    reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2
			WHERE id=$1 AND state='deleting' AND desired_state='deleted'`,
			result.SandboxId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete Sandbox finalization: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.operations
			SET state='succeeded',started_at=COALESCE(started_at,$2),
			    completed_at=$2,updated_at=$2
			WHERE id=$1 AND kind='delete' AND state IN ('pending','running')`,
			result.OperationId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete Operation completion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.operations
			SET state='failed',error_code='state_conflict',
			    error_message='Sandbox deletion superseded the lifecycle operation',
			    retryable=false,started_at=COALESCE(started_at,$2),
			    completed_at=$2,updated_at=$2
			WHERE sandbox_id=$1 AND kind IN ('create','start','drain','stop')
			  AND state IN ('pending','running')`,
			result.SandboxId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete superseded Operation completion: %w", err)
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "succeeded", "", "", evidenceJSON, now,
		); err != nil {
			return err
		}
	} else {
		errorCode := localWorkspaceOperationErrorCode(result.Terminal)
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET mutation_state='failed',updated_at=$2
			WHERE id=$1 AND mutation_id=$3`,
			result.WorkspaceId, now, result.EffectId,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete mutation failure: %w", err)
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "runner_failed",
			errorCode, result.SafeDetail, evidenceJSON, now,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET next_reconcile_at=$2,reconcile_owner='',
			    reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2
			WHERE id=$1 AND state='deleting'`,
			result.SandboxId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace delete retry scheduling: %w", err)
		}
	}
	if err := acknowledgeRunnerCommand(ctx, tx, effect.commandID, now); err != nil {
		return err
	}
	return nil
}

func recordLocalSnapshotResult(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	snapshot rowlock.Snapshot,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE:
	default:
		return nil
	}
	effect, err := lockLocalWorkspaceEffect(ctx, tx, result.EffectId)
	if err != nil {
		return err
	}
	workspace := locked.Workspace
	expectedMutationKind := "snapshot_create"
	expectedSnapshotState := "creating"
	successSnapshotState := "ready"
	if result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE {
		expectedMutationKind = "snapshot_delete"
		expectedSnapshotState = "deleting"
		successSnapshotState = "deleted"
	}
	if workspace.HomeRunnerID != runnerID ||
		workspace.Mutation.Kind != expectedMutationKind ||
		workspace.Mutation.EffectID != result.EffectId ||
		workspace.Mutation.OperationID != result.OperationId ||
		workspace.Mutation.State == "" ||
		snapshot.State != expectedSnapshotState {
		return errors.New("SecondBox runner local Snapshot result conflicts with durable authority")
	}
	if effect.state == "succeeded" || effect.state == "runner_failed" {
		return nil
	}
	if effect.state != "queued" {
		return errors.New("SecondBox runner local Snapshot result is reordered")
	}
	if result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE &&
		(result.Generation != uint64(workspace.Generation) ||
			result.LogicalCapacityBytes != uint64(workspace.LogicalCapacityBytes)) {
		return errors.New("SecondBox runner local Snapshot receipt has inconsistent generation or capacity")
	}
	evidenceJSON, err := localWorkspaceEvidence(result, true, false)
	if err != nil {
		return err
	}
	succeeded := result.Terminal == runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if succeeded && result.ReceiptRecordedAtUnixMs == 0 {
		return errors.New("SecondBox runner local Snapshot success lacks a durable receipt")
	}
	if succeeded {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.snapshots
			SET state=$2,runner_receipt_json=$3,updated_at=$4,
			    retention_ended_at=CASE WHEN $2='deleted' THEN $4 ELSE retention_ended_at END
			WHERE id=$1`,
			result.SnapshotId, successSnapshotState, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner local Snapshot completion: %w", err)
		}
		if err := finishPendingOperation(
			ctx, tx, result.OperationId, "succeeded", "", "", now,
		); err != nil {
			return err
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "succeeded", "", "", evidenceJSON, now,
		); err != nil {
			return err
		}
	} else {
		errorCode := localWorkspaceOperationErrorCode(result.Terminal)
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.snapshots
			SET state='failed',runner_receipt_json=$2,updated_at=$3 WHERE id=$1`,
			result.SnapshotId, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner local Snapshot failure: %w", err)
		}
		if err := finishPendingOperation(
			ctx, tx, result.OperationId, "failed", errorCode, result.SafeDetail, now,
		); err != nil {
			return err
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "runner_failed",
			errorCode, result.SafeDetail, evidenceJSON, now,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,
		    mutation_state='',updated_at=$2 WHERE id=$1`,
		result.WorkspaceId, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local Snapshot mutation release: %w", err)
	}
	return acknowledgeRunnerCommand(ctx, tx, effect.commandID, now)
}

func recordLocalGenerationAdvanceResult(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	effect, err := lockLocalWorkspaceEffect(ctx, tx, result.EffectId)
	if err != nil {
		return err
	}
	workspace := locked.Workspace
	if workspace.HomeRunnerID != runnerID ||
		effect.kind != "stop" ||
		workspace.Mutation.Kind != "stop" ||
		workspace.Mutation.ID != result.EffectId ||
		workspace.Mutation.EffectID != result.EffectId ||
		workspace.Mutation.OperationID != result.OperationId ||
		locked.Generation != workspace.Generation {
		return errors.New("SecondBox runner generation-advance result conflicts with durable authority")
	}
	succeeded := result.Terminal ==
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if effect.state == "runner_succeeded" {
		if succeeded &&
			workspace.State == "failed" &&
			workspace.Mutation.State == "failed" &&
			locked.SandboxState == "failed" {
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces
				SET state='ready',mutation_state='runner_succeeded',updated_at=$2
				WHERE id=$1`,
				result.WorkspaceId, now,
			); err != nil {
				return fmt.Errorf("SecondBox runner generation-advance Workspace recovery: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.sandboxes
				SET state='stopping',lifecycle_failure_class='',lifecycle_failure_message='',
				    next_reconcile_at=$2,reconcile_owner='',reconcile_claim_expires_at=NULL,
				    revision=revision+1,updated_at=$2
				WHERE id=$1`,
				result.SandboxId, now,
			); err != nil {
				return fmt.Errorf("SecondBox runner generation-advance Sandbox recovery: %w", err)
			}
		}
		return nil
	}
	if effect.state == "runner_failed" && !succeeded {
		return nil
	}
	if (effect.state != "queued" && effect.state != "runner_failed") ||
		workspace.State != "ready" ||
		workspace.Mutation.State != "advancing" ||
		(locked.SandboxState != "stopping" && locked.SandboxState != "failed") {
		return errors.New("SecondBox runner generation-advance result is reordered")
	}
	evidenceJSON, err := localWorkspaceEvidence(result, false, true)
	if err != nil {
		return err
	}
	if succeeded {
		if result.ReceiptRecordedAtUnixMs == 0 ||
			result.PreviousGeneration != uint64(workspace.Generation) ||
			result.Generation != uint64(workspace.Generation+1) ||
			result.LogicalCapacityBytes != uint64(workspace.LogicalCapacityBytes) {
			return errors.New("SecondBox runner generation-advance receipt is inconsistent")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET local_receipt_json=$2,mutation_state='runner_succeeded',updated_at=$3
			WHERE id=$1`,
			result.WorkspaceId, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner generation-advance Workspace evidence: %w", err)
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "runner_succeeded", "", "", evidenceJSON, now,
		); err != nil {
			return err
		}
	} else {
		errorCode := localWorkspaceOperationErrorCode(result.Terminal)
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET mutation_state='failed',updated_at=$2 WHERE id=$1`,
			result.WorkspaceId, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner generation-advance Workspace failure: %w", err)
		}
		if err := finishLocalWorkspaceEffect(
			ctx, tx, result.EffectId, "runner_failed",
			errorCode, result.SafeDetail, evidenceJSON, now,
		); err != nil {
			return err
		}
	}
	if err := acknowledgeRunnerCommand(ctx, tx, effect.commandID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state=CASE WHEN $3 AND state='failed' THEN 'stopping' ELSE state END,
		    lifecycle_failure_class=CASE WHEN $3 THEN '' ELSE lifecycle_failure_class END,
		    lifecycle_failure_message=CASE WHEN $3 THEN '' ELSE lifecycle_failure_message END,
		    next_reconcile_at=$2,reconcile_owner='',reconcile_claim_expires_at=NULL,
		    revision=revision+1,updated_at=$2
		WHERE id=$1`,
		result.SandboxId, now, succeeded,
	); err != nil {
		return fmt.Errorf("SecondBox runner generation-advance reconciliation wake: %w", err)
	}
	return nil
}

func recordLocalWorkspaceReconciliation(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	if result.Correlation == nil ||
		result.Correlation.RunnerId != runnerID ||
		result.OperationId != result.EffectId {
		return errors.New("SecondBox runner Workspace reconciliation authority is invalid")
	}
	var commandState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.runner_commands
		WHERE id=$1 AND runner_id=$2 AND assignment_id=$1 AND kind='local-workspace'
		FOR UPDATE`,
		result.EffectId,
		runnerID,
	).Scan(&commandState); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation command lookup: %w", err)
	}
	if commandState == "acknowledged" {
		return nil
	}
	if commandState != "delivering" &&
		commandState != "delivered" &&
		commandState != "pending" {
		return errors.New("SecondBox runner Workspace reconciliation command is not active")
	}
	if result.Terminal ==
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED {
		for _, receipt := range result.Receipts {
			if _, err := replayReconciledWorkspaceReceipt(
				ctx,
				tx,
				runnerID,
				receipt,
				now,
			); err != nil {
				return err
			}
		}
	}
	inventory := make(map[string]*runnerv1.LocalWorkspaceInventoryItem, len(result.Inventory))
	for _, item := range result.Inventory {
		if _, duplicate := inventory[item.WorkspaceId]; duplicate {
			return errors.New("SecondBox runner Workspace reconciliation inventory contains duplicate identities")
		}
		inventory[item.WorkspaceId] = item
	}
	rows, err := tx.Query(ctx, `
		SELECT sandbox.id
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE workspace.home_runner_id=$1
		  AND workspace.state NOT IN ('creating','deleted')
		  AND sandbox.state<>'deleted'
		ORDER BY sandbox.id`,
		runnerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation expected-state lookup: %w", err)
	}
	var sandboxIDs []string
	for rows.Next() {
		var sandboxID string
		if err := rows.Scan(&sandboxID); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox runner Workspace reconciliation expected-state scan: %w", err)
		}
		sandboxIDs = append(sandboxIDs, sandboxID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation expected-state iteration: %w", err)
	}
	failureClass := ""
	if result.Terminal !=
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED {
		failureClass = localWorkspaceOperationErrorCode(result.Terminal)
	}
	for _, sandboxID := range sandboxIDs {
		locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
		if err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation authority lock: %w", err)
		}
		item, found := inventory[locked.WorkspaceID]
		delete(inventory, locked.WorkspaceID)
		if failureClass != "" {
			if err := failReconciledWorkspace(
				ctx,
				tx,
				locked,
				result.EffectId,
				failureClass,
				result.SafeDetail,
				now,
			); err != nil {
				return err
			}
			continue
		}
		if !found {
			if err := failReconciledWorkspace(
				ctx,
				tx,
				locked,
				result.EffectId,
				"home_workspace_missing",
				"home runner reported no local Workspace",
				now,
			); err != nil {
				return err
			}
			continue
		}
		var activeAssignmentState string
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE((
				SELECT state FROM secondbox.assignments
				WHERE sandbox_id=$1 AND generation=$2 AND runner_id=$3
				  AND state IN ('assigned','accepted','starting','ready','uncertain','fencing')
				ORDER BY id
				LIMIT 1
			)::text,'')`,
			locked.SandboxID,
			locked.Generation,
			runnerID,
		).Scan(&activeAssignmentState); err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation Assignment lookup: %w", err)
		}
		activeAssignment := activeAssignmentState != ""
		var restorePending bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM secondbox.workspace_restores
				WHERE workspace_id=$1 AND state NOT IN ('finalized','failed')
			)`,
			locked.WorkspaceID,
		).Scan(&restorePending); err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation restore lookup: %w", err)
		}
		writerMayStillBeDetached := activeAssignmentState == "assigned" ||
			activeAssignmentState == "accepted" ||
			activeAssignmentState == "starting" ||
			activeAssignmentState == "uncertain" ||
			activeAssignmentState == "fencing"
		writerMatches := item.ActiveWriter == activeAssignment ||
			writerMayStillBeDetached && !item.ActiveWriter
		expectedGeneration := locked.Workspace.Generation
		if locked.Workspace.Mutation.Kind == "stop" &&
			locked.Workspace.Mutation.State == "runner_succeeded" {
			expectedGeneration = locked.Workspace.Mutation.TargetGeneration
		}
		if item.Generation != uint64(expectedGeneration) ||
			item.LogicalCapacityBytes != uint64(locked.Workspace.LogicalCapacityBytes) ||
			!item.Formatted ||
			item.RestorePending != restorePending ||
			!writerMatches {
			if err := failReconciledWorkspace(
				ctx,
				tx,
				locked,
				result.EffectId,
				"home_workspace_conflict",
				"home runner local Workspace evidence conflicts with durable authority",
				now,
			); err != nil {
				return err
			}
			continue
		}
		if err := recoverReconciledWorkspace(
			ctx,
			tx,
			locked,
			activeAssignment,
			result.EffectId,
			now,
		); err != nil {
			return err
		}
	}
	for workspaceID := range inventory {
		if err := insertWorkspaceReconciliationAudit(
			ctx,
			tx,
			result.EffectId+"-unexpected-"+workspaceID,
			"",
			"",
			"runner.workspace_inventory_conflict",
			"runner",
			runnerID,
			"failed",
			result.Correlation.RequestId,
			map[string]string{
				"class":       "unexpected_local_workspace",
				"workspaceId": workspaceID,
			},
			now,
		); err != nil {
			return err
		}
	}
	if err := acknowledgeRunnerCommand(ctx, tx, result.EffectId, now); err != nil {
		return err
	}
	return nil
}

func replayReconciledWorkspaceReceipt(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	receipt *runnerv1.LocalWorkspaceReceiptItem,
	now time.Time,
) (bool, error) {
	if receipt == nil {
		return false, errors.New("SecondBox runner Workspace reconciliation receipt is absent")
	}
	var sandboxID string
	err := tx.QueryRow(ctx, `
		SELECT sandbox_id FROM secondbox.workspaces
		WHERE id=$1 AND home_runner_id=$2`,
		receipt.WorkspaceId,
		runnerID,
	).Scan(&sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Workspace reconciliation receipt lookup: %w", err)
	}
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner Workspace reconciliation receipt lock: %w", err)
	}
	if locked.WorkspaceID != receipt.WorkspaceId ||
		locked.Workspace.HomeRunnerID != runnerID {
		return false, errors.New("SecondBox runner Workspace reconciliation receipt has wrong authority")
	}
	effectID := ""
	snapshotID := receipt.SnapshotId
	operationID := receipt.OperationId
	switch receipt.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		var (
			storedSnapshotID string
			prepareEffectID  string
			swapEffectID     string
			finalizeEffectID string
			abortEffectID    string
		)
		err := tx.QueryRow(ctx, `
			SELECT snapshot_id,prepare_effect_id,swap_effect_id,finalize_effect_id,
			       abort_effect_id
			FROM secondbox.workspace_restores
			WHERE operation_id=$1 AND workspace_id=$2`,
			receipt.OperationId,
			receipt.WorkspaceId,
		).Scan(
			&storedSnapshotID,
			&prepareEffectID,
			&swapEffectID,
			&finalizeEffectID,
			&abortEffectID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("SecondBox runner Workspace reconciliation restore receipt lookup: %w", err)
		}
		if snapshotID != storedSnapshotID {
			return false, errors.New("SecondBox runner Workspace reconciliation restore receipt targets the wrong Snapshot")
		}
		switch receipt.Kind {
		case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:
			effectID = prepareEffectID
		case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:
			effectID = swapEffectID
		case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:
			effectID = finalizeEffectID
		case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
			effectID = abortEffectID
		}
	default:
		if locked.Workspace.Mutation.State == "" ||
			!workspaceMutationMatchesReceipt(locked.Workspace.Mutation.Kind, receipt.Kind) {
			return false, nil
		}
		if receipt.OperationId != locked.Workspace.Mutation.OperationID &&
			receipt.OperationId != locked.Workspace.Mutation.ID &&
			receipt.OperationId != locked.Workspace.Mutation.EffectID {
			return false, nil
		}
		effectID = locked.Workspace.Mutation.EffectID
		operationID = locked.Workspace.Mutation.OperationID
	}
	if effectID == "" || operationID == "" {
		return false, nil
	}
	replayed := &runnerv1.LocalWorkspaceResult{
		CommandVersion:          1,
		Kind:                    receipt.Kind,
		Terminal:                runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:             operationID,
		EffectId:                effectID,
		SandboxId:               locked.SandboxID,
		WorkspaceId:             locked.WorkspaceID,
		SnapshotId:              snapshotID,
		PreviousGeneration:      receipt.PreviousGeneration,
		Generation:              receipt.Generation,
		LogicalCapacityBytes:    receipt.LogicalCapacityBytes,
		ReceiptRecordedAtUnixMs: receipt.ReceiptRecordedAtUnixMs,
		Correlation: &runnerv1.Correlation{
			OperationId: operationID,
			SandboxId:   locked.SandboxID,
			RunnerId:    runnerID,
		},
	}
	if err := recordLocalWorkspaceResult(ctx, tx, runnerID, replayed, now); err != nil {
		return false, fmt.Errorf("SecondBox runner Workspace reconciliation receipt replay: %w", err)
	}
	return true, nil
}

func workspaceMutationMatchesReceipt(
	mutationKind string,
	receiptKind runnerv1.LocalWorkspaceCommandKind,
) bool {
	switch receiptKind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE:
		return mutationKind == "create"
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT:
		return mutationKind == "clone"
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE:
		return mutationKind == "workspace_delete"
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION:
		return mutationKind == "stop"
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE:
		return mutationKind == "snapshot_create"
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE:
		return mutationKind == "snapshot_delete"
	default:
		return false
	}
}

func failReconciledWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	reconcileID string,
	failureClass string,
	failureMessage string,
	now time.Time,
) error {
	evidenceJSON, err := json.Marshal(map[string]string{
		"reconciliationId": reconcileID,
		"class":            failureClass,
		"message":          failureMessage,
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation evidence encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET state='failed',local_receipt_json=$2,
		    mutation_state=CASE
		      WHEN mutation_state='' THEN '' ELSE 'failed'
		    END,
		    updated_at=$3
		WHERE id=$1`,
		locked.WorkspaceID,
		evidenceJSON,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation failure update: %w", err)
	}
	if locked.Workspace.Mutation.OperationID != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.operations
			SET state='failed',error_code=$2,error_message=$3,retryable=false,
			    completed_at=$4,updated_at=$4
			WHERE id=$1 AND state IN ('pending','running')`,
			locked.Workspace.Mutation.OperationID,
			failureClass,
			failureMessage,
			now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation Operation failure: %w", err)
		}
	}
	if locked.Workspace.Mutation.EffectID != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='expired',target_connection_id='',updated_at=$2
			WHERE id=(
			  SELECT command_id FROM secondbox.lifecycle_effects WHERE id=$1
			) AND state IN ('pending','delivering','delivered')`,
			locked.Workspace.Mutation.EffectID,
			now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation command expiry: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.lifecycle_effects
			SET state='runner_failed',failure_class=$2,failure_message=$3,
			    evidence_json=$4,claim_owner='',claim_expires_at=$5,updated_at=$5
			WHERE id=$1 AND state='queued'`,
			locked.Workspace.Mutation.EffectID,
			failureClass,
			failureMessage,
			evidenceJSON,
			now,
		); err != nil {
			return fmt.Errorf("SecondBox runner Workspace reconciliation effect failure: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state='failed',lifecycle_failure_class=$2,lifecycle_failure_message=$3,
		    reconcile_owner='',reconcile_claim_expires_at=NULL,
		    next_reconcile_at=NULL,
		    revision=revision+1,updated_at=$4
		WHERE id=$1 AND state<>'deleted'`,
		locked.SandboxID,
		failureClass,
		failureMessage,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation Sandbox failure: %w", err)
	}
	return insertWorkspaceReconciliationAudit(
		ctx,
		tx,
		reconcileID+"-failed-"+locked.WorkspaceID,
		locked.TenantRef,
		locked.SubjectRef,
		"runner.workspace_reconciliation",
		"sandbox",
		locked.SandboxID,
		"failed",
		reconcileID,
		map[string]string{
			"class":       failureClass,
			"workspaceId": locked.WorkspaceID,
		},
		now,
	)
}

func recoverReconciledWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	activeWriter bool,
	reconcileID string,
	now time.Time,
) error {
	if locked.Workspace.State != "failed" ||
		locked.Workspace.Mutation.State != "" ||
		!strings.HasPrefix(locked.SandboxState, "failed") {
		return nil
	}
	var failureClass string
	if err := tx.QueryRow(ctx, `
		SELECT lifecycle_failure_class FROM secondbox.sandboxes WHERE id=$1`,
		locked.SandboxID,
	).Scan(&failureClass); err != nil {
		return fmt.Errorf("SecondBox runner Workspace recovery failure lookup: %w", err)
	}
	if !strings.HasPrefix(failureClass, "home_workspace_") {
		return nil
	}
	nextSandboxState := "stopped"
	if activeWriter {
		nextSandboxState = "ready"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET state='ready',local_receipt_json=$2,updated_at=$3 WHERE id=$1`,
		locked.WorkspaceID,
		[]byte(`{"reconciled":true}`),
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation recovery update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state=$2,lifecycle_failure_class='',lifecycle_failure_message='',
		    revision=revision+1,updated_at=$3 WHERE id=$1`,
		locked.SandboxID,
		nextSandboxState,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation Sandbox recovery: %w", err)
	}
	return insertWorkspaceReconciliationAudit(
		ctx,
		tx,
		reconcileID+"-recovered-"+locked.WorkspaceID,
		locked.TenantRef,
		locked.SubjectRef,
		"runner.workspace_reconciliation",
		"sandbox",
		locked.SandboxID,
		"succeeded",
		reconcileID,
		map[string]string{
			"class":       "home_workspace_recovered",
			"workspaceId": locked.WorkspaceID,
		},
		now,
	)
}

func insertWorkspaceReconciliationAudit(
	ctx context.Context,
	tx pgx.Tx,
	id string,
	tenantRef string,
	subjectRef string,
	action string,
	resourceKind string,
	resourceID string,
	outcome string,
	requestID string,
	details map[string]string,
	now time.Time,
) error {
	if tenantRef == "" {
		tenantRef = "secondbox"
	}
	if subjectRef == "" {
		subjectRef = "runner-reconciler"
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation audit encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.audit_events (
			id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,
			resource_id,outcome,request_id,details_json,created_at
		) VALUES (
			$1,$2,$3,'system','runner-reconciler',$4,$5,$6,$7,$8,$9,$10
		) ON CONFLICT (id) DO NOTHING`,
		id,
		tenantRef,
		subjectRef,
		action,
		resourceKind,
		resourceID,
		outcome,
		requestID,
		detailsJSON,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Workspace reconciliation audit insert: %w", err)
	}
	return nil
}

type localRestoreAuthority struct {
	restoreID, sandboxID, workspaceID, snapshotID, homeRunnerID, operationID string
	tenantRef, subjectRef                                                    string
	prepareEffectID, swapEffectID, finalizeEffectID, abortEffectID           string
	prepareCommandID, swapCommandID, finalizeCommandID, abortCommandID       string
	restoreState, failureClass, failureMessage                               string
	mutationID, mutationKind, mutationEffectID, mutationOperationID          string
	mutationState, sandboxState, requestID, effectState, effectCommandID     string
	expectedGeneration, targetGeneration, workspaceGeneration                int64
	sandboxGeneration, capacity, effectRetryCount                            int64
	fencingToken                                                             []byte
}

type localRestorePhase struct {
	effectID      string
	commandID     string
	expectedState string
	effectKind    string
}

func recordLocalRestoreResult(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	snapshot rowlock.Snapshot,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	authority := localRestoreAuthority{
		sandboxID:           locked.SandboxID,
		workspaceID:         locked.WorkspaceID,
		snapshotID:          snapshot.ID,
		mutationID:          locked.Workspace.Mutation.ID,
		mutationKind:        locked.Workspace.Mutation.Kind,
		mutationEffectID:    locked.Workspace.Mutation.EffectID,
		mutationOperationID: locked.Workspace.Mutation.OperationID,
		mutationState:       locked.Workspace.Mutation.State,
		workspaceGeneration: locked.Workspace.Generation,
		sandboxState:        locked.SandboxState,
		sandboxGeneration:   locked.Generation,
		capacity:            locked.Workspace.LogicalCapacityBytes,
	}
	err := tx.QueryRow(ctx, `
		SELECT restore.id,restore.home_runner_id,restore.operation_id,
		       restore.tenant_ref,restore.subject_ref,
		       restore.prepare_effect_id,restore.swap_effect_id,
		       restore.finalize_effect_id,restore.abort_effect_id,
		       restore.prepare_command_id,restore.swap_command_id,
		       restore.finalize_command_id,restore.abort_command_id,
		       restore.state,restore.failure_class,restore.failure_message,
		       restore.expected_generation,restore.target_generation,
		       operation.request_id
		FROM secondbox.workspace_restores AS restore
		JOIN secondbox.operations AS operation ON operation.id=restore.operation_id
		WHERE restore.operation_id=$1 AND restore.sandbox_id=$2
		  AND restore.workspace_id=$3 AND restore.snapshot_id=$4
		FOR UPDATE OF restore`,
		result.OperationId, result.SandboxId, result.WorkspaceId, result.SnapshotId,
	).Scan(
		&authority.restoreID, &authority.homeRunnerID, &authority.operationID,
		&authority.tenantRef, &authority.subjectRef,
		&authority.prepareEffectID, &authority.swapEffectID,
		&authority.finalizeEffectID, &authority.abortEffectID,
		&authority.prepareCommandID, &authority.swapCommandID,
		&authority.finalizeCommandID, &authority.abortCommandID,
		&authority.restoreState, &authority.failureClass, &authority.failureMessage,
		&authority.expectedGeneration, &authority.targetGeneration,
		&authority.requestID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("SecondBox runner local restore result has no durable authority")
	}
	if err != nil {
		return fmt.Errorf("SecondBox runner local restore authority lookup: %w", err)
	}
	effect, err := lockLocalWorkspaceEffect(ctx, tx, result.EffectId)
	if err != nil {
		return err
	}
	authority.effectState = effect.state
	authority.effectCommandID = effect.commandID
	authority.effectRetryCount = effect.retryCount
	authority.fencingToken = effect.fencingToken
	phase, err := restorePhaseForResult(authority, result.Kind)
	if err != nil {
		return err
	}
	if authority.homeRunnerID != runnerID ||
		authority.operationID != result.OperationId ||
		authority.snapshotID != result.SnapshotId ||
		phase.effectID != result.EffectId {
		return errors.New("SecondBox runner local restore result conflicts with durable authority")
	}
	if authority.effectState == "succeeded" || authority.effectState == "runner_failed" {
		return nil
	}
	if authority.effectState != "queued" || authority.restoreState != phase.expectedState {
		return errors.New("SecondBox runner local restore result is reordered")
	}
	if result.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE {
		if authority.mutationID != authority.restoreID ||
			authority.mutationKind != "snapshot_restore" ||
			authority.mutationOperationID != authority.operationID {
			return errors.New("SecondBox runner local restore result lacks the durable Workspace mutation")
		}
		if authority.mutationEffectID != result.EffectId {
			return errors.New("SecondBox runner local restore effect is not the active Workspace mutation")
		}
	} else if authority.workspaceGeneration != authority.targetGeneration ||
		authority.sandboxGeneration != authority.targetGeneration {
		return errors.New("SecondBox runner local restore finalize precedes the database commit")
	}
	evidenceJSON, err := localWorkspaceEvidence(result, true, true)
	if err != nil {
		return err
	}
	succeeded := result.Terminal ==
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if succeeded {
		if result.ReceiptRecordedAtUnixMs == 0 {
			return errors.New("SecondBox runner local restore success lacks a durable receipt")
		}
		if err := validateLocalRestoreReceipt(authority, result); err != nil {
			return err
		}
		return completeLocalRestorePhase(ctx, tx, authority, phase, result, evidenceJSON, now)
	}
	if result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE {
		return beginLocalRestoreAbort(ctx, tx, authority, result, evidenceJSON, now)
	}
	return retryLocalRestorePhase(ctx, tx, authority, phase, result, evidenceJSON, now)
}

func restorePhaseForResult(
	authority localRestoreAuthority,
	kind runnerv1.LocalWorkspaceCommandKind,
) (localRestorePhase, error) {
	switch kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:
		return localRestorePhase{
			effectID: authority.prepareEffectID, commandID: authority.prepareCommandID,
			expectedState: "requested", effectKind: "local_snapshot_restore_prepare",
		}, nil
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:
		return localRestorePhase{
			effectID: authority.swapEffectID, commandID: authority.swapCommandID,
			expectedState: "prepared", effectKind: "local_snapshot_restore_swap",
		}, nil
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:
		return localRestorePhase{
			effectID: authority.finalizeEffectID, commandID: authority.finalizeCommandID,
			expectedState: "database_committed", effectKind: "local_snapshot_restore_finalize",
		}, nil
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		return localRestorePhase{
			effectID: authority.abortEffectID, commandID: authority.abortCommandID,
			expectedState: "aborting", effectKind: "local_snapshot_restore_abort",
		}, nil
	default:
		return localRestorePhase{}, errors.New("SecondBox runner local restore kind is unsupported")
	}
}

func validateLocalRestoreReceipt(
	authority localRestoreAuthority,
	result *runnerv1.LocalWorkspaceResult,
) error {
	if result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT &&
		result.PreviousGeneration == 0 && result.Generation == 0 &&
		result.LogicalCapacityBytes == 0 {
		return nil
	}
	if result.PreviousGeneration != uint64(authority.expectedGeneration) ||
		result.Generation != uint64(authority.targetGeneration) ||
		result.LogicalCapacityBytes != uint64(authority.capacity) {
		return errors.New("SecondBox runner local restore receipt has inconsistent generation or capacity")
	}
	return nil
}

func completeLocalRestorePhase(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	phase localRestorePhase,
	result *runnerv1.LocalWorkspaceResult,
	evidenceJSON []byte,
	now time.Time,
) error {
	if err := finishLocalWorkspaceEffect(
		ctx, tx, result.EffectId, "succeeded", "", "", evidenceJSON, now,
	); err != nil {
		return err
	}
	if err := acknowledgeRunnerCommand(ctx, tx, authority.effectCommandID, now); err != nil {
		return err
	}
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_restores
			SET state='prepared',prepare_receipt_json=$2,updated_at=$3 WHERE id=$1`,
			authority.restoreID, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner local restore prepare completion: %w", err)
		}
		if err := advanceRestoreWorkspaceMutation(
			ctx, tx, authority, authority.swapEffectID, "queued", now,
		); err != nil {
			return err
		}
		return queueRestorePhase(
			ctx, tx, authority, authority.swapEffectID, authority.swapCommandID,
			runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
			"local_snapshot_restore_swap", authority.expectedGeneration, now,
		)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:
		return commitLocalRestore(ctx, tx, authority, evidenceJSON, now)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_restores
			SET state='finalized',finalize_receipt_json=$2,finalized_at=$3,updated_at=$3
			WHERE id=$1`,
			authority.restoreID, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner local restore finalize completion: %w", err)
		}
		return nil
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		errorCode := authority.failureClass
		if errorCode == "" {
			errorCode = "workspace_restore_failed"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_restores
			SET state='failed',abort_receipt_json=$2,failed_at=$3,updated_at=$3
			WHERE id=$1`,
			authority.restoreID, evidenceJSON, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner local restore abort completion: %w", err)
		}
		if err := finishPendingOperation(
			ctx, tx, authority.operationID, "failed",
			errorCode, authority.failureMessage, now,
		); err != nil {
			return err
		}
		return releaseRestoreWorkspaceMutation(ctx, tx, authority.workspaceID, now)
	default:
		return errors.New("SecondBox runner local restore completion kind is unsupported")
	}
}

func commitLocalRestore(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	evidenceJSON []byte,
	now time.Time,
) error {
	if authority.sandboxState != "stopped" ||
		authority.sandboxGeneration != authority.expectedGeneration ||
		authority.workspaceGeneration != authority.expectedGeneration {
		return errors.New("SecondBox runner local restore swap conflicts with current generation")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET generation=$2,revision=revision+1,updated_at=$3
		WHERE id=$1 AND generation=$4 AND state='stopped'`,
		authority.sandboxID, authority.targetGeneration, now, authority.expectedGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore Sandbox commit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET generation=$2,local_receipt_json=$3,
		    mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',updated_at=$4
		WHERE id=$1 AND generation=$5`,
		authority.workspaceID, authority.targetGeneration, evidenceJSON, now,
		authority.expectedGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore Workspace commit: %w", err)
	}
	if err := fenceGenerationAuthority(
		ctx, tx, authority.sandboxID, authority.operationID,
		authority.expectedGeneration, now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_restores
		SET state='database_committed',swap_receipt_json=$2,
		    database_committed_at=$3,updated_at=$3 WHERE id=$1`,
		authority.restoreID, evidenceJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore database commit evidence: %w", err)
	}
	if err := finishPendingOperation(
		ctx, tx, authority.operationID, "succeeded", "", "", now,
	); err != nil {
		return err
	}
	auditDetails, err := json.Marshal(map[string]string{
		"restoreId":          authority.restoreID,
		"snapshotId":         authority.snapshotID,
		"previousGeneration": fmt.Sprintf("%d", authority.expectedGeneration),
		"generation":         fmt.Sprintf("%d", authority.targetGeneration),
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner local restore audit encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.audit_events (
			id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,
			resource_id,outcome,request_id,details_json,created_at
		) VALUES (
			$1,$2,$3,'system','runner-reconciler','snapshot.restore_committed',
			'sandbox',$4,'succeeded',$5,$6,$7
		)`,
		"audit_snapshot_restore_commit_"+authority.restoreID,
		authority.tenantRef,
		authority.subjectRef,
		authority.sandboxID,
		authority.requestID,
		auditDetails,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore audit insert: %w", err)
	}
	return queueRestorePhase(
		ctx, tx, authority, authority.finalizeEffectID, authority.finalizeCommandID,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		"local_snapshot_restore_finalize", authority.targetGeneration, now,
	)
}

func beginLocalRestoreAbort(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	result *runnerv1.LocalWorkspaceResult,
	evidenceJSON []byte,
	now time.Time,
) error {
	errorCode := localWorkspaceOperationErrorCode(result.Terminal)
	if err := finishLocalWorkspaceEffect(
		ctx, tx, result.EffectId, "runner_failed",
		errorCode, result.SafeDetail, evidenceJSON, now,
	); err != nil {
		return err
	}
	if err := acknowledgeRunnerCommand(ctx, tx, authority.effectCommandID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_restores
		SET state='aborting',failure_class=$2,failure_message=$3,updated_at=$4
		WHERE id=$1`,
		authority.restoreID, errorCode, result.SafeDetail, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore abort transition: %w", err)
	}
	if err := advanceRestoreWorkspaceMutation(
		ctx, tx, authority, authority.abortEffectID, "queued", now,
	); err != nil {
		return err
	}
	return queueRestorePhase(
		ctx, tx, authority, authority.abortEffectID, authority.abortCommandID,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		"local_snapshot_restore_abort", authority.expectedGeneration, now,
	)
}

func retryLocalRestorePhase(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	phase localRestorePhase,
	result *runnerv1.LocalWorkspaceResult,
	evidenceJSON []byte,
	now time.Time,
) error {
	if err := acknowledgeRunnerCommand(ctx, tx, authority.effectCommandID, now); err != nil {
		return err
	}
	nextRetry := authority.effectRetryCount + 1
	retryCommandID := fmt.Sprintf("%s-retry-%d", phase.commandID, nextRetry)
	commandKind := result.Kind
	_, payload, err := restoreCommandPayload(
		authority, phase.effectID, retryCommandID, commandKind,
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
		retryCommandID, authority.homeRunnerID, phase.effectID, payload, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore retry command insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state='queued',command_id=$2,retry_count=$3,
		    effect_deadline=$4,failure_class=$5,failure_message=$6,
		    evidence_json=$7,updated_at=$8 WHERE id=$1`,
		phase.effectID, retryCommandID, nextRetry, now.Add(10*time.Minute),
		localWorkspaceOperationErrorCode(result.Terminal), result.SafeDetail,
		evidenceJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore retry effect update: %w", err)
	}
	if result.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE {
		if err := advanceRestoreWorkspaceMutation(
			ctx, tx, authority, phase.effectID, "queued", now,
		); err != nil {
			return err
		}
	}
	return nil
}

func advanceRestoreWorkspaceMutation(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	effectID string,
	state string,
	now time.Time,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_effect_id=$2,mutation_state=$3,updated_at=$4
		WHERE id=$1 AND mutation_id=$5 AND mutation_kind='snapshot_restore'
		  AND mutation_operation_id=$6`,
		authority.workspaceID, effectID, state, now,
		authority.restoreID, authority.operationID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner local restore Workspace mutation advance: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("SecondBox runner local restore Workspace mutation was lost")
	}
	return nil
}

func releaseRestoreWorkspaceMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,
		    mutation_state='',updated_at=$2 WHERE id=$1`,
		workspaceID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore Workspace mutation release: %w", err)
	}
	return nil
}

func queueRestorePhase(
	ctx context.Context,
	tx pgx.Tx,
	authority localRestoreAuthority,
	effectID string,
	commandID string,
	kind runnerv1.LocalWorkspaceCommandKind,
	effectKind string,
	generation int64,
	now time.Time,
) error {
	_, payload, err := restoreCommandPayload(authority, effectID, commandID, kind)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			$1,$2,$3,$4,'queued','','',$5,$6,$7,$8,0,8,$9,'',$10,'','','{}','{}',$10,$10
		)`,
		effectID, authority.sandboxID, generation, effectKind, authority.homeRunnerID,
		commandID, authority.snapshotID, authority.fencingToken,
		now.Add(10*time.Minute), now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore effect queue: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
		commandID, authority.homeRunnerID, effectID, payload, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local restore command queue: %w", err)
	}
	return nil
}

func restoreCommandPayload(
	authority localRestoreAuthority,
	effectID string,
	commandID string,
	kind runnerv1.LocalWorkspaceCommandKind,
) (*runnerv1.LocalWorkspaceCommand, []byte, error) {
	correlationGeneration := authority.expectedGeneration
	if kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE {
		correlationGeneration = authority.targetGeneration
	}
	command := &runnerv1.LocalWorkspaceCommand{
		MessageId: commandID, CommandVersion: 1, Kind: kind,
		OperationId: authority.operationID, EffectId: effectID,
		SandboxId: authority.sandboxID, WorkspaceId: authority.workspaceID,
		SnapshotId:           authority.snapshotID,
		ExpectedGeneration:   uint64(authority.expectedGeneration),
		NextGeneration:       uint64(authority.targetGeneration),
		LogicalCapacityBytes: uint64(authority.capacity),
		FencingToken:         append([]byte(nil), authority.fencingToken...),
		Correlation: &runnerv1.Correlation{
			RequestId: authority.requestID, OperationId: authority.operationID,
			SandboxId:         authority.sandboxID,
			SandboxGeneration: uint64(correlationGeneration),
			RunnerId:          authority.homeRunnerID,
		},
	}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
			LocalWorkspace: command,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox runner local restore command encoding: %w", err)
	}
	return command, payload, nil
}

type localWorkspaceEffect struct {
	kind            string
	state           string
	commandID       string
	storageObjectID string
	retryCount      int64
	fencingToken    []byte
}

func lockLocalWorkspaceEffect(
	ctx context.Context,
	tx pgx.Tx,
	effectID string,
) (localWorkspaceEffect, error) {
	var effect localWorkspaceEffect
	err := tx.QueryRow(ctx, `
		SELECT kind,state,command_id,storage_object_id,retry_count,fencing_token
		FROM secondbox.lifecycle_effects WHERE id=$1 FOR UPDATE`,
		effectID,
	).Scan(
		&effect.kind, &effect.state, &effect.commandID, &effect.storageObjectID,
		&effect.retryCount, &effect.fencingToken,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return localWorkspaceEffect{}, errors.New(
			"SecondBox runner local Workspace result has no durable effect",
		)
	}
	if err != nil {
		return localWorkspaceEffect{}, fmt.Errorf(
			"SecondBox runner local Workspace effect lookup: %w",
			err,
		)
	}
	return effect, nil
}

func localWorkspaceEvidence(
	result *runnerv1.LocalWorkspaceResult,
	includeSnapshot bool,
	includePreviousGeneration bool,
) ([]byte, error) {
	evidence := map[string]any{
		"effectId":          result.EffectId,
		"generation":        result.Generation,
		"logicalCapacity":   result.LogicalCapacityBytes,
		"receiptRecordedAt": result.ReceiptRecordedAtUnixMs,
		"terminal":          result.Terminal.String(),
	}
	if includeSnapshot {
		evidence["snapshotId"] = result.SnapshotId
	}
	if includePreviousGeneration {
		evidence["previousGeneration"] = result.PreviousGeneration
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner local Workspace evidence encoding: %w", err)
	}
	return encoded, nil
}

func finishLocalWorkspaceEffect(
	ctx context.Context,
	tx pgx.Tx,
	effectID string,
	state string,
	failureClass string,
	failureMessage string,
	evidence []byte,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state=$2,failure_class=$3,failure_message=$4,evidence_json=$5,updated_at=$6
		WHERE id=$1`,
		effectID, state, failureClass, failureMessage, evidence, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local Workspace effect transition: %w", err)
	}
	return nil
}

func finishPendingOperation(
	ctx context.Context,
	tx pgx.Tx,
	operationID string,
	state string,
	errorCode string,
	errorMessage string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state=$2,error_code=$3,error_message=$4,retryable=false,
		    completed_at=$5,updated_at=$5
		WHERE id=$1 AND state='pending'`,
		operationID, state, errorCode, errorMessage, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local Workspace Operation transition: %w", err)
	}
	return nil
}

func acknowledgeRunnerCommand(
	ctx context.Context,
	tx pgx.Tx,
	commandID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='acknowledged',updated_at=$2 WHERE id=$1`,
		commandID, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner local Workspace command acknowledgement: %w", err)
	}
	return nil
}

func fenceGenerationAuthority(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
	restoreOperationID string,
	generation int64,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET state='fenced',reconcile_owner='',reconcile_claim_expires_at=$3,
		    failure_class=CASE
		      WHEN failure_class='' THEN 'fencing' ELSE failure_class
		    END,
		    revision=revision+1,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2
		  AND state NOT IN ('fenced','released')`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation Assignment fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.instances
		SET state='stopped',guest_liveness='lost',
		    termination_reason=CASE
		      WHEN termination_reason='' THEN 'fenced' ELSE termination_reason
		    END,
		    stopped_at=COALESCE(stopped_at,$3),updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state<>'stopped'`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation Instance fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='expired',target_connection_id='',updated_at=$3
		WHERE state IN ('pending','delivering','delivered')
		  AND (
		    assignment_id IN (
		      SELECT id FROM secondbox.assignments
		      WHERE sandbox_id=$1 AND generation=$2
		    )
		    OR assignment_id IN (
		      SELECT id FROM secondbox.lifecycle_effects
		      WHERE sandbox_id=$1 AND generation=$2 AND state='queued'
		    )
		  )`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation Runner command fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state='runner_failed',failure_class='generation_fenced',
		    failure_message='Sandbox generation was fenced by Snapshot restore',
		    claim_owner='',claim_expires_at=$3,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state='queued'`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation lifecycle effect fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state='failed',error_code='generation_fenced',
		    error_message='Sandbox generation was fenced by Snapshot restore',
		    retryable=false,completed_at=$3,updated_at=$3
		WHERE sandbox_id=$1 AND id<>$2
		  AND state IN ('pending','running')`,
		sandboxID, restoreOperationID, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation Operation fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases
		SET state='fenced',revision=revision+1,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation Lease fence: %w", err)
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
		return fmt.Errorf("SecondBox restored generation data-plane fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET state='fenced',closed_at=$3,updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state IN ('open','closing')`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation PortSession fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=COALESCE(closed_at,$3),updated_at=$3
		WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
		sandboxID, generation, now,
	); err != nil {
		return fmt.Errorf("SecondBox restored generation activity fence: %w", err)
	}
	return nil
}

type localWorkspaceFailure struct {
	code   string
	detail string
}

var localWorkspaceFailures = map[runnerv1.LocalWorkspaceTerminalKind]localWorkspaceFailure{
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT:    {"workspace_local_data_absent", "local workspace data is absent"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER:        {"workspace_active_writer", "workspace has an active writer"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE: {"workspace_storage_incompatible", "local workspace storage is incompatible"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE:   {"workspace_insufficient_space", "local workspace storage has insufficient space"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CORRUPT_RECEIPT:      {"workspace_receipt_corrupt", "local workspace receipt is corrupt"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY:   {"workspace_replay_conflict", "local workspace operation conflicts with its durable receipt"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_WRONG_HOME_RUNNER:    {"home_runner_mismatch", "workspace is not owned by this runner"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SNAPSHOT_IN_USE:      {"snapshot_in_use", "snapshot is in use"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RESTORE_PENDING:      {"snapshot_restore_pending", "workspace restore is pending"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED:        {"workspace_runner_failed", "local workspace operation failed"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION:     {"workspace_authority_stale", "workspace generation is stale"},
	runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE:          {"workspace_authority_stale", "workspace fencing authority is stale"},
}

func localWorkspaceOperationErrorCode(terminal runnerv1.LocalWorkspaceTerminalKind) string {
	if failure, ok := localWorkspaceFailures[terminal]; ok {
		return failure.code
	}
	return "workspace_operation_failed"
}

func validateLocalWorkspaceResult(
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
) error {
	if strings.TrimSpace(runnerID) == "" ||
		result == nil ||
		result.CommandVersion != 1 ||
		result.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_UNSPECIFIED ||
		result.Terminal == runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_UNSPECIFIED ||
		strings.TrimSpace(result.EffectId) == "" {
		return errors.New("SecondBox runner local-workspace result is incomplete")
	}
	if result.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE &&
		(strings.TrimSpace(result.SandboxId) == "" ||
			strings.TrimSpace(result.WorkspaceId) == "") {
		return errors.New("SecondBox runner local-workspace result identity is incomplete")
	}
	for _, item := range result.Inventory {
		if item == nil || strings.TrimSpace(item.WorkspaceId) == "" ||
			item.Generation == 0 ||
			item.LogicalCapacityBytes == 0 ||
			item.LogicalCapacityBytes > uint64(^uint64(0)>>1) {
			return errors.New("SecondBox runner local-workspace inventory is incomplete")
		}
	}
	for _, receipt := range result.Receipts {
		if receipt == nil ||
			receipt.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_UNSPECIFIED ||
			receipt.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT ||
			receipt.Kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE ||
			strings.TrimSpace(receipt.OperationId) == "" ||
			strings.TrimSpace(receipt.WorkspaceId) == "" ||
			receipt.ReceiptRecordedAtUnixMs == 0 {
			return errors.New("SecondBox runner local-workspace receipt inventory is incomplete")
		}
		if receipt.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT &&
			(receipt.Generation == 0 ||
				receipt.LogicalCapacityBytes == 0 ||
				receipt.LogicalCapacityBytes > uint64(^uint64(0)>>1)) {
			return errors.New("SecondBox runner local-workspace receipt authority is incomplete")
		}
	}
	if result.Terminal == runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED {
		if result.SafeDetail != "" {
			return errors.New("SecondBox runner local-workspace success contains failure detail")
		}
	} else if result.SafeDetail != expectedLocalWorkspaceSafeDetail(result.Terminal) {
		return errors.New("SecondBox runner local-workspace failure detail is not canonical")
	}
	return nil
}

func expectedLocalWorkspaceSafeDetail(terminal runnerv1.LocalWorkspaceTerminalKind) string {
	if failure, ok := localWorkspaceFailures[terminal]; ok {
		return failure.detail
	}
	return "local workspace operation failed"
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

type runnerCapacity struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
	Instances   int64
	Operations  int64
}

func runnerCapacityFromProtocol(capacity *runnerv1.Capacity) (runnerCapacity, error) {
	if capacity == nil {
		return runnerCapacity{}, errors.New("SecondBox runner capacity is required")
	}
	return runnerCapacity{
		CPUMillis: int64(capacity.VcpuMillis), MemoryBytes: int64(capacity.MemoryBytes),
		DiskBytes: int64(capacity.DiskBytes), Instances: int64(capacity.Instances),
		Operations: int64(capacity.Operations),
	}, nil
}

func encodeProtocolCapacity(capacity *runnerv1.Capacity) ([]byte, error) {
	converted, err := runnerCapacityFromProtocol(capacity)
	if err != nil {
		return nil, err
	}
	return encodeRunnerCapacity(converted)
}

func encodeRunnerCapacity(capacity runnerCapacity) ([]byte, error) {
	encoded, err := json.Marshal(capacity)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner capacity encoding: %w", err)
	}
	return encoded, nil
}

func maxRunnerCapacity(left runnerCapacity, right runnerCapacity) runnerCapacity {
	return runnerCapacity{
		CPUMillis:   max(left.CPUMillis, right.CPUMillis),
		MemoryBytes: max(left.MemoryBytes, right.MemoryBytes),
		DiskBytes:   max(left.DiskBytes, right.DiskBytes),
		Instances:   max(left.Instances, right.Instances),
		Operations:  max(left.Operations, right.Operations),
	}
}

func protocolStartupTiming(timing *runnerv1.StartupTiming) (int64, int64, error) {
	if timing == nil ||
		timing.SampleCount > uint64(^uint64(0)>>1) ||
		timing.P95Milliseconds > uint64(^uint64(0)>>1) {
		return 0, 0, errors.New("SecondBox runner startup timing evidence is required")
	}
	return int64(timing.SampleCount), int64(timing.P95Milliseconds), nil
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
		state, err := assignmentEvidenceState(ctx, tx, ack.Fence.AssignmentId)
		if err != nil {
			return err
		}
		if ack.Decision == runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED &&
			(state == "accepted" || state == "starting" || state == "ready") {
			return nil
		}
		if err := validateAssignmentEvidenceState(
			ctx, tx, ack.Fence.AssignmentId, "assigned",
		); err != nil {
			return err
		}
		nextState := "accepted"
		failureClass := ""
		if ack.Decision != runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED {
			nextState = "failed"
			failureClass = assignmentDecisionFailureClass(ack.Decision)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state=$2,failure_class=$3,
			    next_reconcile_at=CASE WHEN $2='failed' THEN $4 ELSE next_reconcile_at END,
			    reconcile_owner=CASE WHEN $2='failed' THEN '' ELSE reconcile_owner END,
			    reconcile_claim_expires_at=CASE
			      WHEN $2='failed' THEN $4
			      ELSE reconcile_claim_expires_at
			    END,
			    revision=revision+1,updated_at=$4
			WHERE id=$1`,
			ack.Fence.AssignmentId, nextState, failureClass, now,
		); err != nil {
			return fmt.Errorf("SecondBox runner AssignmentAck update: %w", err)
		}
		if nextState == "failed" {
			if err := wakeSandboxLifecycle(ctx, tx, ack.Fence, now); err != nil {
				return err
			}
			return acknowledgeAssignmentCommands(
				ctx,
				tx,
				ack.Fence.AssignmentId,
				now,
			)
		}
	case message.GetAssignmentProgress() != nil:
		progress := message.GetAssignmentProgress()
		if err := lockAndValidateFence(ctx, tx, runnerID, progress.Fence); err != nil {
			return err
		}
		if err := validateOperationCorrelation(runnerID, progress.Fence, progress.Correlation); err != nil {
			return err
		}
		state, err := assignmentEvidenceState(ctx, tx, progress.Fence.AssignmentId)
		if err != nil {
			return err
		}
		if state == "ready" {
			return nil
		}
		if err := validateAssignmentEvidenceState(
			ctx, tx, progress.Fence.AssignmentId, "assigned", "accepted", "starting",
		); err != nil {
			return err
		}
		stage, err := assignmentProgressStageName(progress.Stage)
		if err != nil {
			return err
		}
		if progress.ObservedAtUnixMs == 0 ||
			progress.ObservedAtUnixMs > uint64(^uint64(0)>>1) ||
			progress.ObservedAtUnixNs > uint64(^uint64(0)>>1) {
			return errors.New("SecondBox runner AssignmentProgress observed time is invalid")
		}
		observedAt := time.UnixMilli(int64(progress.ObservedAtUnixMs)).UTC()
		if progress.ObservedAtUnixNs != 0 {
			observedAt = time.Unix(0, int64(progress.ObservedAtUnixNs)).UTC()
			if observedAt.UnixMilli() != int64(progress.ObservedAtUnixMs) {
				return errors.New("SecondBox runner AssignmentProgress observed times disagree")
			}
		}
		inserted, err := tx.Exec(ctx, `
			INSERT INTO secondbox.assignment_stage_timings (
				assignment_id,operation_id,sandbox_id,stage,observed_at,received_at
			) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (assignment_id,stage) DO NOTHING`,
			progress.Fence.AssignmentId, progress.Correlation.OperationId,
			progress.Fence.SandboxId, stage, observedAt, now,
		)
		if err != nil {
			return fmt.Errorf("SecondBox runner AssignmentProgress timing insert: %w", err)
		}
		if inserted.RowsAffected() == 0 {
			var persistedOperationID, persistedSandboxID string
			var persistedObservedAt time.Time
			if err := tx.QueryRow(ctx, `
				SELECT operation_id,sandbox_id,observed_at
				FROM secondbox.assignment_stage_timings
				WHERE assignment_id=$1 AND stage=$2`,
				progress.Fence.AssignmentId, stage,
			).Scan(&persistedOperationID, &persistedSandboxID, &persistedObservedAt); err != nil {
				return fmt.Errorf("SecondBox runner AssignmentProgress timing replay lookup: %w", err)
			}
			if persistedOperationID != progress.Correlation.OperationId ||
				persistedSandboxID != progress.Fence.SandboxId ||
				!persistedObservedAt.Equal(observedAt) {
				return errors.New("SecondBox runner AssignmentProgress stage was repeated with different evidence")
			}
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
		state, err := assignmentEvidenceState(ctx, tx, result.Fence.AssignmentId)
		if err != nil {
			return err
		}
		if state == "ready" {
			if result.Terminal != runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY ||
				result.BackendKind == "" || result.BackendReference == "" {
				return ErrStaleAssignmentEvidence
			}
			var backendKind, backendReference string
			if err := tx.QueryRow(ctx, `
				SELECT backend_kind,backend_reference
				FROM secondbox.assignments WHERE id=$1`,
				result.Fence.AssignmentId,
			).Scan(&backendKind, &backendReference); err != nil {
				return fmt.Errorf("SecondBox runner ready AssignmentResult replay lookup: %w", err)
			}
			if backendKind != result.BackendKind || backendReference != result.BackendReference {
				return errors.New("SecondBox runner ready AssignmentResult replay changed backend evidence")
			}
			return acknowledgeAssignmentCommands(
				ctx,
				tx,
				result.Fence.AssignmentId,
				now,
			)
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
				SET state='ready',guest_liveness='ready',ready_at=$2,
				    guest_heartbeat_at=$2,updated_at=$2 WHERE id=$1`,
				result.Fence.InstanceId, now,
			); err != nil {
				return fmt.Errorf("SecondBox runner ready Instance update: %w", err)
			}
			command, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces AS workspace
				SET mutation_kind='',mutation_id='',mutation_effect_id='',
				    mutation_operation_id='',mutation_expected_generation=NULL,
				    mutation_target_generation=NULL,mutation_state='',updated_at=$1
				FROM secondbox.sandboxes AS sandbox
				WHERE sandbox.id=$2 AND sandbox.workspace_id=workspace.id
				  AND workspace.home_runner_id=$3
				  AND workspace.mutation_kind='start'
				  AND workspace.mutation_operation_id=$4
				  AND workspace.mutation_expected_generation=$5`,
				now, result.Fence.SandboxId, runnerID,
				result.Correlation.OperationId, result.Fence.SandboxGeneration,
			)
			if err != nil {
				return fmt.Errorf("SecondBox runner ready Workspace start release: %w", err)
			}
			if command.RowsAffected() != 1 {
				return errors.New("SecondBox runner ready AssignmentResult lacks the durable Workspace start mutation")
			}
			if err := wakeSandboxLifecycle(ctx, tx, result.Fence, now); err != nil {
				return err
			}
			return acknowledgeAssignmentCommands(
				ctx,
				tx,
				result.Fence.AssignmentId,
				now,
			)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.assignments
			SET state='failed',failure_class=$2,next_reconcile_at=$3,
			    reconcile_owner='',reconcile_claim_expires_at=$3,
			    revision=revision+1,updated_at=$3
			WHERE id=$1`,
			result.Fence.AssignmentId, assignmentTerminalFailureClass(result.Terminal), now,
		); err != nil {
			return fmt.Errorf("SecondBox runner failed AssignmentResult update: %w", err)
		}
		if err := wakeSandboxLifecycle(ctx, tx, result.Fence, now); err != nil {
			return err
		}
		return acknowledgeAssignmentCommands(
			ctx,
			tx,
			result.Fence.AssignmentId,
			now,
		)
	default:
		return ErrRunnerMessage
	}
	return nil
}

func wakeSandboxLifecycle(
	ctx context.Context,
	tx pgx.Tx,
	fence *runnerv1.AssignmentFence,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=$4,reconcile_owner='',
		    reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$4
		WHERE id=$1 AND generation=$2 AND current_instance_id=$3`,
		fence.SandboxId,
		fence.SandboxGeneration,
		fence.InstanceId,
		now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner Assignment result lifecycle wakeup: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrStaleAssignmentEvidence
	}
	return nil
}

func assignmentProgressStageName(stage runnerv1.AssignmentProgressStage) (string, error) {
	switch stage {
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION:
		return "runner_admission", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY:
		return "artifact_verify", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_ATTACH:
		return "workspace_attach", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP:
		return "network_setup", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_FIRECRACKER_LAUNCH:
		return "compute_launch", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION:
		return "guest_negotiation", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY:
		return "ready", nil
	case runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_TEARDOWN:
		return "teardown", nil
	default:
		return "", errors.New("SecondBox runner AssignmentProgress stage is unspecified")
	}
}

func acknowledgeAssignmentCommands(
	ctx context.Context,
	tx pgx.Tx,
	assignmentID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='acknowledged',updated_at=$2
		WHERE assignment_id=$1
		  AND kind='assignment'
		  AND state IN ('pending','delivering','delivered')`,
		assignmentID,
		now,
	); err != nil {
		return fmt.Errorf("SecondBox runner Assignment command acknowledgement: %w", err)
	}
	return nil
}

func validateAssignmentEvidenceState(
	ctx context.Context,
	tx pgx.Tx,
	assignmentID string,
	allowed ...string,
) error {
	state, err := assignmentEvidenceState(ctx, tx, assignmentID)
	if err != nil {
		return err
	}
	for _, candidate := range allowed {
		if state == candidate {
			return nil
		}
	}
	return ErrStaleAssignmentEvidence
}

func assignmentEvidenceState(
	ctx context.Context,
	tx pgx.Tx,
	assignmentID string,
) (string, error) {
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.assignments WHERE id=$1 FOR UPDATE`,
		assignmentID,
	).Scan(&state); err != nil {
		return "", fmt.Errorf("SecondBox runner Assignment state lookup: %w", err)
	}
	return state, nil
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
		if errors.Is(err, ErrStaleAssignmentEvidence) {
			acknowledged, acknowledgeErr := acknowledgeRedundantFenceResult(
				ctx,
				tx,
				runnerID,
				result,
				now,
			)
			if acknowledgeErr != nil {
				return acknowledgeErr
			}
			if acknowledged {
				return nil
			}
		}
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
	var (
		stopEffectID, stopCommandID, workspaceID, homeRunnerID    string
		mutationKind, mutationID, mutationEffectID, mutationState string
		workspaceGeneration, capacity                             int64
	)
	stopAuthorityErr := tx.QueryRow(ctx, `
		SELECT effect.id,effect.command_id,workspace.id,workspace.home_runner_id,
		       workspace.mutation_kind,workspace.mutation_id,
		       workspace.mutation_effect_id,workspace.mutation_state,
		       workspace.generation,workspace.logical_capacity_bytes
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=effect.sandbox_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE effect.assignment_id=$1 AND effect.kind='stop' AND effect.state='queued'
		FOR UPDATE OF effect,workspace`,
		result.Fence.AssignmentId,
	).Scan(
		&stopEffectID, &stopCommandID, &workspaceID, &homeRunnerID,
		&mutationKind, &mutationID, &mutationEffectID, &mutationState,
		&workspaceGeneration, &capacity,
	)
	hasStopAuthority := stopAuthorityErr == nil
	if stopAuthorityErr != nil && !errors.Is(stopAuthorityErr, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox runner stop Workspace authority lookup: %w", stopAuthorityErr)
	}
	if hasStopAuthority {
		if homeRunnerID != runnerID ||
			mutationKind != "stop" ||
			mutationID != stopEffectID ||
			mutationEffectID != stopEffectID ||
			mutationState == "" ||
			workspaceGeneration != int64(result.Fence.SandboxGeneration) {
			return errors.New("SecondBox runner stop Workspace mutation conflicts with durable authority")
		}
	} else {
		var failureClass string
		if err := tx.QueryRow(ctx, `
			SELECT failure_class FROM secondbox.assignments WHERE id=$1`,
			result.Fence.AssignmentId,
		).Scan(&failureClass); err != nil {
			return fmt.Errorf("SecondBox runner-loss Assignment classification lookup: %w", err)
		}
		if failureClass != "fencing" && failureClass != "startup_timeout" {
			return errors.New("SecondBox runner FenceResult lacks a durable stop effect")
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.assignments
		SET state='fenced',release_proof_json=$2,revision=revision+1,updated_at=$3
		WHERE id=$1`, result.Fence.AssignmentId, proofJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner successful FenceResult update: %w", err)
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
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=$2,last_activity_at=$2
		WHERE sandbox_id=$1 AND generation=$3 AND state='active'`,
		result.Fence.SandboxId, now, result.Fence.SandboxGeneration,
	); err != nil {
		return fmt.Errorf("SecondBox runner fenced activity session closure: %w", err)
	}
	if !hasStopAuthority {
		if _, err := acknowledgeMatchingFenceCommand(
			ctx,
			tx,
			runnerID,
			result,
			now,
		); err != nil {
			return err
		}
		return nil
	}
	localCommandID := stopEffectID + "-generation-advance"
	// A runner can report both STOPPED and the deterministic ALREADY_STOPPED
	// replay for the same fence. The stop effect remains queued while its
	// generation-advance command is outstanding, so treat that command as the
	// durable idempotency marker instead of acknowledging and reinserting it.
	if stopCommandID == localCommandID {
		return nil
	}
	localCommand := &runnerv1.LocalWorkspaceCommand{
		MessageId: localCommandID, CommandVersion: 1,
		Kind:        runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		OperationId: stopEffectID, EffectId: stopEffectID,
		SandboxId: result.Fence.SandboxId, WorkspaceId: workspaceID,
		ExpectedGeneration:   result.Fence.SandboxGeneration,
		NextGeneration:       result.Fence.SandboxGeneration + 1,
		LogicalCapacityBytes: uint64(capacity),
		FencingToken:         append([]byte(nil), result.Fence.FencingToken...),
		Correlation: &runnerv1.Correlation{
			RequestId: result.Correlation.RequestId, OperationId: stopEffectID,
			SandboxId:         result.Fence.SandboxId,
			SandboxGeneration: result.Fence.SandboxGeneration,
			RunnerId:          runnerID,
		},
	}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
			LocalWorkspace: localCommand,
		},
	})
	if err != nil {
		return fmt.Errorf("SecondBox runner stop generation-advance command encoding: %w", err)
	}
	if err := acknowledgeRunnerCommand(ctx, tx, stopCommandID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
		localCommandID, runnerID, stopEffectID, payload, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner stop generation-advance command queue: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET command_id=$2,state='queued',evidence_json=$3,updated_at=$4
		WHERE id=$1`,
		stopEffectID, localCommandID, proofJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner stop generation-advance effect update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_state='advancing',updated_at=$2
		WHERE id=$1 AND mutation_id=$3`,
		workspaceID, now, stopEffectID,
	); err != nil {
		return fmt.Errorf("SecondBox runner stop generation-advance mutation update: %w", err)
	}
	return nil
}

func acknowledgeRedundantFenceResult(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	result *runnerv1.FenceResult,
	now time.Time,
) (bool, error) {
	if result == nil || result.Fence == nil ||
		(result.Result != runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED &&
			result.Result != runnerv1.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED) ||
		result.TerminationEvidenceDigest == "" {
		return false, nil
	}
	var (
		storedSandboxID, storedInstanceID, storedRunnerID, state string
		storedGeneration                                         int64
		storedToken, releaseProofJSON                            []byte
	)
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id,instance_id,runner_id,generation,fencing_token,state,
		       release_proof_json
		FROM secondbox.assignments WHERE id=$1 FOR UPDATE`,
		result.Fence.AssignmentId,
	).Scan(
		&storedSandboxID,
		&storedInstanceID,
		&storedRunnerID,
		&storedGeneration,
		&storedToken,
		&state,
		&releaseProofJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("SecondBox redundant FenceResult Assignment lookup: %w", err)
	}
	if storedSandboxID != result.Fence.SandboxId ||
		storedInstanceID != result.Fence.InstanceId ||
		storedRunnerID != runnerID ||
		storedGeneration != int64(result.Fence.SandboxGeneration) ||
		!bytes.Equal(storedToken, result.Fence.FencingToken) ||
		(state != "fenced" && state != "released" && state != "failed_terminal") {
		return false, nil
	}
	var proof map[string]string
	if err := json.Unmarshal(releaseProofJSON, &proof); err != nil {
		return false, fmt.Errorf("SecondBox redundant FenceResult proof decoding: %w", err)
	}
	if proof["terminationEvidenceDigest"] != result.TerminationEvidenceDigest {
		return false, nil
	}
	return acknowledgeMatchingFenceCommand(ctx, tx, runnerID, result, now)
}

func acknowledgeMatchingFenceCommand(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	result *runnerv1.FenceResult,
	now time.Time,
) (bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT id,payload
		FROM secondbox.runner_commands
		WHERE runner_id=$1 AND assignment_id=$2 AND kind='fence'
		  AND state IN ('pending','delivering','delivered')
		FOR UPDATE`,
		runnerID,
		result.Fence.AssignmentId,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox redundant Fence command lookup: %w", err)
	}
	var matchingCommandID string
	for rows.Next() {
		var commandID string
		var payload []byte
		if err := rows.Scan(&commandID, &payload); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox redundant Fence command scan: %w", err)
		}
		message := &runnerv1.ControlPlaneToRunner{}
		if err := proto.Unmarshal(payload, message); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox redundant Fence command decoding: %w", err)
		}
		command := message.GetFence()
		if command != nil &&
			proto.Equal(command.Fence, result.Fence) &&
			proto.Equal(command.Correlation, result.Correlation) {
			matchingCommandID = commandID
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("SecondBox redundant Fence command iteration: %w", err)
	}
	if matchingCommandID == "" {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='acknowledged',target_connection_id='',updated_at=$2
		WHERE id=$1`,
		matchingCommandID,
		now,
	); err != nil {
		return false, fmt.Errorf("SecondBox redundant Fence command acknowledgement: %w", err)
	}
	return true, nil
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
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, fence.SandboxId)
	if err != nil {
		return fmt.Errorf("SecondBox runner Sandbox/Workspace fence lookup: %w", err)
	}
	if locked.Generation != int64(fence.SandboxGeneration) ||
		locked.CurrentInstanceID != fence.InstanceId {
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
