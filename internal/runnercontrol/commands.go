package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

// CommandDelivery is one database-claimed outbound control frame.
type CommandDelivery struct {
	ID          string
	Kind        string
	CreatedAt   time.Time
	DeliveredAt time.Time
	Message     *runnerv1.ControlPlaneToRunner
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
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner command claim transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var storedRunnerID, state string
	var lastControlSequence int64
	if err := tx.QueryRow(ctx, `
		SELECT connection.runner_id,connection.state,connection.last_control_sequence
		FROM secondbox.runner_connections AS connection
		WHERE connection.id=$1 FOR UPDATE OF connection`, connectionID,
	).Scan(&storedRunnerID, &state, &lastControlSequence); err != nil {
		return nil, fmt.Errorf("SecondBox runner command connection lookup: %w", err)
	}
	if storedRunnerID != runnerID || state != "active" {
		return nil, errors.New("SecondBox runner command connection is inactive")
	}
	rows, err := tx.Query(ctx, `
		SELECT id,kind,created_at,payload FROM secondbox.runner_commands
		WHERE runner_id=$1 AND state='pending'
		ORDER BY (id LIKE 'workspace-reconcile-%') DESC,created_at,id
		FOR UPDATE SKIP LOCKED LIMIT $2`,
		runnerID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner command batch lookup: %w", err)
	}
	deliveries := make([]CommandDelivery, 0, limit)
	for rows.Next() {
		var delivery CommandDelivery
		var payload []byte
		if err := rows.Scan(
			&delivery.ID,
			&delivery.Kind,
			&delivery.CreatedAt,
			&payload,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox runner command batch scan: %w", err)
		}
		delivery.Message = &runnerv1.ControlPlaneToRunner{}
		if err := proto.Unmarshal(payload, delivery.Message); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox runner command payload decoding: %w", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("SecondBox runner command batch rows: %w", err)
	}
	rows.Close()
	if len(deliveries) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("SecondBox empty runner command claim commit: %w", err)
		}
		return []CommandDelivery{}, nil
	}
	commandIDs := make([]string, len(deliveries))
	lastSequence := uint64(lastControlSequence + int64(len(deliveries)))
	for index := range deliveries {
		commandIDs[index] = deliveries[index].ID
		sequence := uint64(lastControlSequence + int64(index) + 1)
		if err := setControlCommandEnvelope(
			deliveries[index].ID,
			sequence,
			deliveries[index].Message,
		); err != nil {
			return nil, err
		}
	}
	command, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='delivering',target_connection_id=$2,delivery_count=delivery_count+1,
			updated_at=$3
		WHERE id=ANY($1::text[]) AND state='pending'`,
		commandIDs,
		connectionID,
		now.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner command delivery batch claim: %w", err)
	}
	if command.RowsAffected() != int64(len(deliveries)) {
		return nil, errors.New("SecondBox runner command batch changed while claimed")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_connections
		SET last_control_sequence=$2 WHERE id=$1`, connectionID, lastSequence,
	); err != nil {
		return nil, fmt.Errorf("SecondBox runner command sequence update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("SecondBox runner command claim commit: %w", err)
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
	default:
		return errors.New("SecondBox runner command queue accepts assignment, fence, drain, or local-workspace commands")
	}
	return nil
}
