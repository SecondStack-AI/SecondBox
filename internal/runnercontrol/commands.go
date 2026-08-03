package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// CommandDelivery is one database-claimed outbound control frame.
type CommandDelivery struct {
	ID          string
	Kind        string
	CreatedAt   time.Time
	DeliveredAt time.Time
	Message     *runnerv1.ControlPlaneToRunner
	ClaimTiming CommandClaimTiming
}

// CommandClaimTiming attributes the pooled PostgreSQL claim without exposing
// database details outside the private runner-control boundary.
type CommandClaimTiming struct {
	PoolAcquire time.Duration
	Query       time.Duration
	Decode      time.Duration
}

type rawCommandDelivery struct {
	delivery           CommandDelivery
	payload            []byte
	sequence           int64
	preclaimedEnvelope bool
}

// ClaimCommand binds one pending command to the active connection and assigns its sequence.
func (store *PostgresStateStore) ClaimCommand(
	ctx context.Context,
	runnerID string,
	connectionID string,
	now time.Time,
) (CommandDelivery, bool, error) {
	deliveries, err := store.ClaimCommands(ctx, runnerID, connectionID, 1, now)
	if err != nil || len(deliveries) == 0 {
		return CommandDelivery{}, false, err
	}
	return deliveries[0], true, nil
}

// ClaimCommands binds one ordered batch to the active connection and assigns
// contiguous control sequences in one transaction.
func (store *PostgresStateStore) ClaimCommands(
	ctx context.Context,
	runnerID string,
	connectionID string,
	limit int64,
	now time.Time,
) ([]CommandDelivery, error) {
	if limit <= 0 {
		return nil, errors.New("SecondBox runner command batch limit must be positive")
	}
	claimStartedAt := time.Now()
	connection, err := store.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner command batch connection acquisition: %w", err)
	}
	acquiredAt := time.Now()
	rows, err := connection.Query(ctx, `
		WITH active_connection AS MATERIALIZED (
			SELECT connection.id
			FROM secondbox.runner_connections AS connection
			WHERE connection.id=$2
			  AND connection.runner_id=$1
			  AND connection.state='active'
		),
		preclaimed_commands AS MATERIALIZED (
			SELECT command.id,command.kind,command.created_at,command.payload
			FROM secondbox.runner_commands AS command
			CROSS JOIN active_connection
			WHERE command.runner_id=$1
			  AND command.state='delivering'
			  AND command.target_connection_id=$2
			ORDER BY
			  (command.id LIKE 'workspace-reconcile-%') DESC,
			  command.created_at,
			  command.id
			LIMIT $3
		),
		candidate_commands AS MATERIALIZED (
			SELECT command.id
			FROM secondbox.runner_commands AS command
			CROSS JOIN active_connection
			WHERE command.runner_id=$1
			  AND command.state='pending'
			  AND NOT EXISTS (SELECT 1 FROM preclaimed_commands)
			  AND (
			    command.kind<>'assignment'
			    OR NOT EXISTS (
			      SELECT 1
			      FROM secondbox.workspaces AS workspace
			      WHERE workspace.home_runner_id=$1
			        AND workspace.state='creating'
			    )
			  )
			ORDER BY
			  (command.id LIKE 'workspace-reconcile-%') DESC,
			  command.created_at,
			  command.id
			LIMIT $3
		),
		locked_connection AS MATERIALIZED (
			SELECT connection.id,connection.last_control_sequence
			FROM secondbox.runner_connections AS connection
			CROSS JOIN (SELECT 1 FROM candidate_commands LIMIT 1) AS pending_work
			WHERE connection.id=$2
			  AND connection.runner_id=$1
			  AND connection.state='active'
			FOR UPDATE OF connection
		),
		chosen AS MATERIALIZED (
			SELECT command.id,command.kind,command.created_at,command.payload
			FROM secondbox.runner_commands AS command
			JOIN candidate_commands AS candidate ON candidate.id=command.id
			CROSS JOIN locked_connection
			WHERE command.state='pending'
			ORDER BY
			  (command.id LIKE 'workspace-reconcile-%') DESC,
			  command.created_at,
			  command.id
			FOR UPDATE OF command SKIP LOCKED
		),
		pending AS MATERIALIZED (
			SELECT
			  chosen.*,
			  row_number() OVER (
			    ORDER BY
			      (chosen.id LIKE 'workspace-reconcile-%') DESC,
			      chosen.created_at,
			      chosen.id
			  )::bigint AS sequence_offset
			FROM chosen
		),
		advanced_connection AS (
			UPDATE secondbox.runner_connections AS connection
			SET last_control_sequence=
			  locked_connection.last_control_sequence +
			  (SELECT count(*) FROM pending)
			FROM locked_connection
			WHERE connection.id=locked_connection.id
			  AND EXISTS (SELECT 1 FROM pending)
			RETURNING locked_connection.last_control_sequence AS base_sequence
		),
		claimed AS (
			UPDATE secondbox.runner_commands AS command
			SET state='delivering',
			    target_connection_id=$2,
			    delivery_count=command.delivery_count+1,
			    updated_at=$4
			FROM pending,advanced_connection
			WHERE command.id=pending.id
			  AND command.state='pending'
			RETURNING
			  command.id,
			  command.kind,
			  command.created_at,
			  command.payload,
			  advanced_connection.base_sequence + pending.sequence_offset AS sequence
		)
		SELECT
		  0 AS row_kind,
		  ''::text AS id,
		  ''::text AS kind,
		  $4::timestamptz AS created_at,
		  ''::bytea AS payload,
		  0::bigint AS sequence
		FROM active_connection
		UNION ALL
		SELECT
		  1,
		  preclaimed.id,
		  preclaimed.kind,
		  preclaimed.created_at,
		  preclaimed.payload,
		  -1::bigint
		FROM preclaimed_commands AS preclaimed
		UNION ALL
		SELECT
		  2,
		  claimed.id,
		  claimed.kind,
		  claimed.created_at,
		  claimed.payload,
		  claimed.sequence
		FROM claimed
		ORDER BY row_kind,sequence`,
		pgx.QueryExecModeExec,
		runnerID,
		connectionID,
		limit,
		now.UTC(),
	)
	if err != nil {
		connection.Release()
		return nil, fmt.Errorf("SecondBox runner command batch lookup: %w", err)
	}
	rawDeliveries := make([]rawCommandDelivery, 0, limit)
	connectionActive := false
	for rows.Next() {
		var rowKind int
		var raw rawCommandDelivery
		if err := rows.Scan(
			&rowKind,
			&raw.delivery.ID,
			&raw.delivery.Kind,
			&raw.delivery.CreatedAt,
			&raw.payload,
			&raw.sequence,
		); err != nil {
			rows.Close()
			connection.Release()
			return nil, fmt.Errorf("SecondBox runner command batch scan: %w", err)
		}
		if rowKind == 0 {
			connectionActive = true
			continue
		}
		raw.preclaimedEnvelope = rowKind == 1
		rawDeliveries = append(rawDeliveries, raw)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		connection.Release()
		return nil, fmt.Errorf("SecondBox runner command batch rows: %w", err)
	}
	rows.Close()
	connection.Release()
	queryCompletedAt := time.Now()
	if !connectionActive {
		return nil, errors.New("SecondBox runner command connection is inactive")
	}
	deliveries := make([]CommandDelivery, 0, len(rawDeliveries))
	for index := range rawDeliveries {
		raw := &rawDeliveries[index]
		raw.delivery.Message = &runnerv1.ControlPlaneToRunner{}
		if err := proto.Unmarshal(raw.payload, raw.delivery.Message); err != nil {
			return nil, fmt.Errorf("SecondBox runner command payload decoding: %w", err)
		}
		if raw.preclaimedEnvelope {
			if err := validateControlCommandEnvelope(
				raw.delivery.ID,
				raw.delivery.Message,
			); err != nil {
				return nil, err
			}
		} else {
			if err := setControlCommandEnvelope(
				raw.delivery.ID,
				uint64(raw.sequence),
				raw.delivery.Message,
			); err != nil {
				return nil, err
			}
		}
		deliveries = append(deliveries, raw.delivery)
	}
	decodedAt := time.Now()
	claimTiming := CommandClaimTiming{
		PoolAcquire: acquiredAt.Sub(claimStartedAt),
		Query:       queryCompletedAt.Sub(acquiredAt),
		Decode:      decodedAt.Sub(queryCompletedAt),
	}
	for index := range deliveries {
		deliveries[index].ClaimTiming = claimTiming
	}
	return deliveries, nil
}

// MarkCommandDelivered records successful stream delivery for reconnect reconciliation.
func (store *PostgresStateStore) MarkCommandDelivered(
	ctx context.Context,
	delivery CommandDelivery,
	connectionID string,
	now time.Time,
) error {
	delivery.DeliveredAt = now.UTC()
	return store.MarkCommandsDelivered(ctx, []CommandDelivery{delivery}, connectionID)
}

// MarkCommandsDelivered atomically persists every successfully sent command
// and its optional startup-dispatch milestone in one database statement.
func (store *PostgresStateStore) MarkCommandsDelivered(
	ctx context.Context,
	deliveries []CommandDelivery,
	connectionID string,
) error {
	if len(deliveries) == 0 {
		return nil
	}
	commandIDs := make([]string, len(deliveries))
	deliveredAts := make([]time.Time, len(deliveries))
	operationIDs := make([]string, len(deliveries))
	sandboxIDs := make([]string, len(deliveries))
	kinds := make([]string, len(deliveries))
	seen := make(map[string]struct{}, len(deliveries))
	for index, delivery := range deliveries {
		if delivery.ID == "" || delivery.DeliveredAt.IsZero() {
			return errors.New("SecondBox delivered command batch requires identity and timestamp")
		}
		if _, duplicate := seen[delivery.ID]; duplicate {
			return errors.New("SecondBox delivered command batch contains a duplicate")
		}
		seen[delivery.ID] = struct{}{}
		commandIDs[index] = delivery.ID
		deliveredAts[index] = delivery.DeliveredAt.UTC()
		kinds[index] = delivery.Kind
		if delivery.Kind == "assignment" {
			correlation := delivery.Message.GetAssignment().GetCorrelation()
			if correlation == nil || correlation.OperationId == "" || correlation.SandboxId == "" {
				return errors.New("SecondBox delivered Assignment command lacks Operation correlation")
			}
			operationIDs[index] = correlation.OperationId
			sandboxIDs[index] = correlation.SandboxId
		}
	}
	var deliveredCount, stageCount int64
	if err := store.pool.QueryRow(ctx, `
		WITH input AS MATERIALIZED (
			SELECT *
			FROM unnest(
				$1::text[],
				$2::timestamptz[],
				$3::text[],
				$4::text[],
				$5::text[]
			) AS item(command_id,delivered_at,kind,operation_id,sandbox_id)
		),
		eligible AS MATERIALIZED (
			SELECT command.id
			FROM secondbox.runner_commands AS command
			JOIN input ON input.command_id=command.id
			WHERE command.target_connection_id=$6
			  AND command.state='delivering'
			FOR UPDATE OF command
		),
		delivered AS (
			UPDATE secondbox.runner_commands
			SET state='delivered',
			    delivered_at=input.delivered_at,
			    updated_at=input.delivered_at
			FROM input
			WHERE runner_commands.id=input.command_id
			  AND runner_commands.id IN (SELECT id FROM eligible)
			  AND (SELECT count(*) FROM eligible)=cardinality($1::text[])
			RETURNING runner_commands.id
		),
		stage AS (
			INSERT INTO secondbox.operation_stage_timings (
				operation_id,sandbox_id,stage,observed_at
			)
			SELECT input.operation_id,input.sandbox_id,
			       'startup_dispatched',input.delivered_at
			FROM delivered
			JOIN input ON input.command_id=delivered.id
			WHERE input.kind='assignment'
			ON CONFLICT (operation_id,stage) DO NOTHING
			RETURNING 1
		)
		SELECT
		  (SELECT count(*) FROM delivered),
		  (SELECT count(*) FROM stage)`,
		commandIDs,
		deliveredAts,
		kinds,
		operationIDs,
		sandboxIDs,
		connectionID,
	).Scan(&deliveredCount, &stageCount); err != nil {
		return fmt.Errorf("SecondBox runner command delivered update: %w", err)
	}
	if deliveredCount != int64(len(deliveries)) {
		return errors.New("SecondBox runner command delivery claim is no longer current")
	}
	return nil
}

func setControlCommandEnvelope(
	messageID string,
	sequence uint64,
	message *runnerv1.ControlPlaneToRunner,
) error {
	switch {
	case message.GetAssignment() != nil:
		message.GetAssignment().MessageId = messageID
		message.GetAssignment().Sequence = sequence
	case message.GetFence() != nil:
		message.GetFence().MessageId = messageID
		message.GetFence().Sequence = sequence
	case message.GetDrain() != nil:
		message.GetDrain().MessageId = messageID
		message.GetDrain().Sequence = sequence
	case message.GetLocalWorkspace() != nil:
		message.GetLocalWorkspace().MessageId = messageID
		message.GetLocalWorkspace().Sequence = sequence
	case message.GetDataPlaneDirectOpen() != nil:
		message.GetDataPlaneDirectOpen().MessageId = messageID
		message.GetDataPlaneDirectOpen().Sequence = sequence
	case message.GetDataPlaneCancel() != nil:
		message.GetDataPlaneCancel().MessageId = messageID
		message.GetDataPlaneCancel().Sequence = sequence
	default:
		return errors.New("SecondBox runner command queue accepts assignment, fence, drain, local-workspace, or direct data-plane commands")
	}
	return nil
}

func validateControlCommandEnvelope(
	messageID string,
	message *runnerv1.ControlPlaneToRunner,
) error {
	var actualMessageID string
	var sequence uint64
	switch {
	case message.GetAssignment() != nil:
		actualMessageID = message.GetAssignment().MessageId
		sequence = message.GetAssignment().Sequence
	case message.GetFence() != nil:
		actualMessageID = message.GetFence().MessageId
		sequence = message.GetFence().Sequence
	case message.GetDrain() != nil:
		actualMessageID = message.GetDrain().MessageId
		sequence = message.GetDrain().Sequence
	case message.GetLocalWorkspace() != nil:
		actualMessageID = message.GetLocalWorkspace().MessageId
		sequence = message.GetLocalWorkspace().Sequence
	case message.GetDataPlaneDirectOpen() != nil:
		actualMessageID = message.GetDataPlaneDirectOpen().MessageId
		sequence = message.GetDataPlaneDirectOpen().Sequence
	case message.GetDataPlaneCancel() != nil:
		actualMessageID = message.GetDataPlaneCancel().MessageId
		sequence = message.GetDataPlaneCancel().Sequence
	default:
		return errors.New("SecondBox preclaimed runner command has an invalid payload")
	}
	if actualMessageID != messageID || sequence == 0 {
		return errors.New("SecondBox preclaimed runner command envelope does not match its claim")
	}
	return nil
}
