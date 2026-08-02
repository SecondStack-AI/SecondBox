package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

// SnapshotRetentionStore admits due local Snapshot deletions without involving
// object storage or retained-byte accounting.
type SnapshotRetentionStore interface {
	QueueExpiredSnapshotDelete(
		context.Context,
		ports.SnapshotRetentionInput,
	) (bool, error)
}

// SnapshotRetentionWorker queues at most one expired local Snapshot deletion.
type SnapshotRetentionWorker struct {
	Store           SnapshotRetentionStore
	PollInterval    time.Duration
	NewID           func(string) string
	NewFencingToken func() ([]byte, error)
}

func (worker SnapshotRetentionWorker) RunOnce(
	ctx context.Context,
	now time.Time,
) (bool, error) {
	if worker.Store == nil ||
		worker.PollInterval <= 0 ||
		worker.NewID == nil ||
		worker.NewFencingToken == nil {
		return false, errors.New(
			"SecondBox Snapshot retention worker dependencies and interval are required",
		)
	}
	token, err := worker.NewFencingToken()
	if err != nil {
		return false, err
	}
	if len(token) < 32 {
		return false, errors.New(
			"SecondBox Snapshot retention worker fencing token is too short",
		)
	}
	return worker.Store.QueueExpiredSnapshotDelete(ctx, ports.SnapshotRetentionInput{
		OperationID:  worker.NewID("op"),
		EffectID:     worker.NewID("effect"),
		CommandID:    worker.NewID("command"),
		RequestID:    worker.NewID("request"),
		FencingToken: token,
		Now:          now.UTC(),
	})
}
