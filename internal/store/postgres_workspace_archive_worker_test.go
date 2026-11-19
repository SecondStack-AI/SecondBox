package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

type failingArchiveWorkerProvider struct {
	archiveTransferTestProvider
	signingFailure, deleteFailure   error
	signingAttempts, deleteAttempts int
}

func (provider *failingArchiveWorkerProvider) Transfer(ctx context.Context, key, method string, descriptor *runnerv1.WorkspaceArchiveDescriptor) (*runnerv1.WorkspaceArchiveTransfer, error) {
	if strings.Contains(key, "archive-aa-bad") {
		provider.signingAttempts++
		return nil, provider.signingFailure
	}
	return provider.archiveTransferTestProvider.Transfer(ctx, key, method, descriptor)
}

func (provider *failingArchiveWorkerProvider) Delete(_ context.Context, key string) error {
	if key == "workspaces/gc-bad" {
		provider.deleteAttempts++
		return provider.deleteFailure
	}
	return nil
}

func TestArchiveWorkerContinuesIndependentRecordsAndBacksOffFailures(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	provider := &failingArchiveWorkerProvider{signingFailure: errors.New("archive signing fixture failure"), deleteFailure: errors.New("archive deletion fixture failure")}
	if err := storage.ConfigureWorkspaceArchives(t.Context(), provider, nil); err != nil {
		t.Fatal(err)
	}
	var sandboxIDs []string
	cleanupArchiveLivenessSandboxes(t, storage, &sandboxIDs)
	for _, suffix := range []string{"aa-bad", "zz-good"} {
		_, sandboxID := seedLocalWorkspace(t, storage, "worker-"+suffix, now)
		sandboxIDs = append(sandboxIDs, sandboxID)
		if _, err := storage.ArchiveSandbox(t.Context(), archiveTestInput(sandboxID, suffix, now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES('workspaces/gc-bad',$1),('workspaces/gc-good',$2)`, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := storage.pool.Exec(context.WithoutCancel(t.Context()), `DELETE FROM secondbox.archive_cleanup WHERE archive_key IN ('workspaces/gc-bad','workspaces/gc-good')`)
		if err != nil {
			t.Error(err)
		}
	})
	err := storage.ReconcileWorkspaceArchives(t.Context(), now, nil)
	if !errors.Is(err, provider.signingFailure) || !errors.Is(err, provider.deleteFailure) {
		t.Fatalf("worker did not report both independent errors: %v", err)
	}
	var queued, garbage int
	if err := storage.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.runner_commands WHERE id='command-archive-zz-good' AND state='pending'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := storage.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.archive_cleanup WHERE archive_key='workspaces/gc-good'`).Scan(&garbage); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || garbage != 0 {
		t.Fatalf("independent work stalled: queued=%d garbage=%d", queued, garbage)
	}
	if err := storage.ReconcileWorkspaceArchives(t.Context(), now.Add(time.Second), nil); err != nil {
		t.Fatalf("backoff pass should skip failed records: %v", err)
	}
	if provider.signingAttempts != 1 || provider.deleteAttempts != 1 {
		t.Fatalf("failed records ignored backoff: signing=%d deletion=%d", provider.signingAttempts, provider.deleteAttempts)
	}
}
