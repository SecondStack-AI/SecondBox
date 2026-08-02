package worknotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

const postgresChannel = "secondbox_work"

type postgresPayload struct {
	Kind Kind   `json:"kind"`
	Key  string `json:"key"`
}

// PostgresListener receives transaction-commit hints on one dedicated connection.
type PostgresListener struct {
	connection *pgx.Conn
	hub        *Hub
}

// NewPostgresListener establishes LISTEN before returning so no later startup
// operation can race worker subscription setup.
func NewPostgresListener(
	ctx context.Context,
	databaseURL string,
	hub *Hub,
) (*PostgresListener, error) {
	if databaseURL == "" || hub == nil {
		return nil, errors.New("SecondBox PostgreSQL work listener requires database and hub")
	}
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox PostgreSQL work listener connection: %w", err)
	}
	if _, err := connection.Exec(ctx, "LISTEN "+postgresChannel); err != nil {
		closeErr := connection.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(
			fmt.Errorf("SecondBox PostgreSQL work listener LISTEN: %w", err),
			closeErr,
		)
	}
	return &PostgresListener{connection: connection, hub: hub}, nil
}

// Run forwards valid hints until cancellation. A connection or payload failure
// is terminal because silently reverting to polling would hide a broken fast path.
func (listener *PostgresListener) Run(ctx context.Context) (returnError error) {
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		returnError = errors.Join(returnError, listener.connection.Close(closeContext))
	}()
	for {
		notification, err := listener.connection.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("SecondBox PostgreSQL work listener receive: %w", err)
		}
		payload, err := decodePostgresPayload(notification.Payload)
		if err != nil {
			return err
		}
		listener.hub.Publish(payload.Kind, payload.Key)
	}
}

func decodePostgresPayload(encoded string) (postgresPayload, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	var payload postgresPayload
	if err := decoder.Decode(&payload); err != nil {
		return postgresPayload{}, fmt.Errorf("SecondBox PostgreSQL work notification decoding: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return postgresPayload{}, errors.New("SecondBox PostgreSQL work notification contains trailing data")
	}
	switch payload.Kind {
	case KindLifecycle, KindAssignment:
		if payload.Key != "" {
			return postgresPayload{}, errors.New("SecondBox PostgreSQL global work notification has an unexpected key")
		}
	case KindRunnerCommand:
		if payload.Key == "" {
			return postgresPayload{}, errors.New("SecondBox PostgreSQL runner work notification requires a key")
		}
	case KindDataPlaneSession:
		if payload.Key == "" {
			return postgresPayload{}, errors.New("SecondBox PostgreSQL data-plane work notification requires a key")
		}
	default:
		return postgresPayload{}, fmt.Errorf(
			"SecondBox PostgreSQL work notification kind %q is invalid",
			payload.Kind,
		)
	}
	return payload, nil
}
