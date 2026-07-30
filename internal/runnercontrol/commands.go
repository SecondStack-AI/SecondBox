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
	ID        string
	Kind      string
	CreatedAt time.Time
	Message   *runnerv1.ControlPlaneToRunner
}

// ClaimCommand binds one pending command to the active connection and assigns its sequence.
func (store *PostgresStateStore) ClaimCommand(
	ctx context.Context,
	runnerID string,
	connectionID string,
	now time.Time,
) (CommandDelivery, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command claim transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var storedRunnerID, state string
	var lastControlSequence int64
	if err := tx.QueryRow(ctx, `
		SELECT connection.runner_id,connection.state,connection.last_control_sequence
		FROM secondbox.runner_connections AS connection
		WHERE connection.id=$1 FOR UPDATE OF connection`, connectionID,
	).Scan(&storedRunnerID, &state, &lastControlSequence); err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command connection lookup: %w", err)
	}
	if storedRunnerID != runnerID || state != "active" {
		return CommandDelivery{}, false, errors.New("SecondBox runner command connection is inactive")
	}
	var delivery CommandDelivery
	var payload []byte
	err = tx.QueryRow(ctx, `
		SELECT id,kind,created_at,payload FROM secondbox.runner_commands
		WHERE runner_id=$1 AND state='pending'
		ORDER BY (id LIKE 'workspace-reconcile-%') DESC,created_at,id
		FOR UPDATE SKIP LOCKED LIMIT 1`, runnerID,
	).Scan(&delivery.ID, &delivery.Kind, &delivery.CreatedAt, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return CommandDelivery{}, false, fmt.Errorf("SecondBox empty runner command claim commit: %w", err)
		}
		return CommandDelivery{}, false, nil
	}
	if err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command lookup: %w", err)
	}
	delivery.Message = &runnerv1.ControlPlaneToRunner{}
	if err := proto.Unmarshal(payload, delivery.Message); err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command payload decoding: %w", err)
	}
	sequence := uint64(lastControlSequence + 1)
	if err := setControlCommandEnvelope(delivery.ID, sequence, delivery.Message); err != nil {
		return CommandDelivery{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='delivering',target_connection_id=$2,delivery_count=delivery_count+1,
			updated_at=$3 WHERE id=$1`,
		delivery.ID, connectionID, now.UTC(),
	); err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command delivery claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_connections
		SET last_control_sequence=$2 WHERE id=$1`, connectionID, sequence,
	); err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command sequence update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CommandDelivery{}, false, fmt.Errorf("SecondBox runner command claim commit: %w", err)
	}
	return delivery, true, nil
}

// MarkCommandDelivered records successful stream delivery for reconnect reconciliation.
func (store *PostgresStateStore) MarkCommandDelivered(
	ctx context.Context,
	commandID string,
	connectionID string,
	now time.Time,
) error {
	command, err := store.pool.Exec(ctx, `
		UPDATE secondbox.runner_commands
		SET state='delivered',delivered_at=$3,updated_at=$3
		WHERE id=$1 AND target_connection_id=$2 AND state='delivering'`,
		commandID, connectionID, now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner command delivered update: %w", err)
	}
	if command.RowsAffected() != 1 {
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
