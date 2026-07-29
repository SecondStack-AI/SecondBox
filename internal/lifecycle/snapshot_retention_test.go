package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestSnapshotRetentionWorkerQueuesTypedLocalDelete(t *testing.T) {
	store := &recordingSnapshotRetentionStore{queued: true}
	worker := SnapshotRetentionWorker{
		Store: store, PollInterval: time.Second,
		NewID: func(prefix string) string { return prefix + "-retention" },
		NewFencingToken: func() ([]byte, error) {
			return []byte("01234567890123456789012345678901"), nil
		},
	}
	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	queued, err := worker.RunOnce(t.Context(), now)
	if err != nil || !queued {
		t.Fatalf("Snapshot retention run = queued %t, error %v", queued, err)
	}
	if store.input.OperationID != "op-retention" ||
		store.input.EffectID != "effect-retention" ||
		store.input.CommandID != "command-retention" ||
		store.input.RequestID != "request-retention" ||
		store.input.Now != now ||
		len(store.input.FencingToken) != 32 {
		t.Fatalf("Snapshot retention input = %#v", store.input)
	}
}

type recordingSnapshotRetentionStore struct {
	input  ports.SnapshotRetentionInput
	queued bool
}

func (store *recordingSnapshotRetentionStore) QueueExpiredSnapshotDelete(
	_ context.Context,
	input ports.SnapshotRetentionInput,
) (bool, error) {
	store.input = input
	return store.queued, nil
}
