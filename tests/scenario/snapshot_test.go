//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioSnapshotDurabilityAndInPlaceRestore(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-snapshot-durability",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, _ := createScenarioSandbox(t, fixture, profile, "snapshot-durability")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)

	const path = "snapshot-state.txt"
	before := []byte("state captured by the Snapshot\n")
	after := []byte("state written after the Snapshot\n")
	writeScenarioFile(t, ctx, fixture.subject, handle, path, before)

	stopped := stopScenarioSandbox(t, ctx, fixture, handle, "durability-stop-before-restart")
	started := startScenarioSandbox(t, ctx, fixture, handle, "durability-start-before-snapshot")
	if started.State != contracts.SandboxStateReady {
		t.Fatalf("SecondBox scenario restarted Sandbox = %#v", started)
	}
	if got := readScenarioFile(t, ctx, fixture.subject, handle, path); !bytes.Equal(got, before) {
		t.Fatalf("SecondBox scenario ordinary stop/start content = %q, want %q", got, before)
	}
	stopped = stopScenarioSandbox(t, ctx, fixture, handle, "durability-stop-for-snapshot")

	snapshot, createOperation := createScenarioSnapshot(
		t,
		ctx,
		fixture,
		handle,
		"durability-point",
		map[string]string{"scenario": "durability"},
		true,
	)
	if snapshot.SourceGeneration != stopped.Generation ||
		snapshot.SandboxID != stopped.ID ||
		snapshot.State != "ready" {
		t.Fatalf("SecondBox scenario ready Snapshot = %#v", snapshot)
	}
	if createOperation.Kind != "snapshot_create" || createOperation.Snapshot == nil {
		t.Fatalf("SecondBox scenario Snapshot create Operation = %#v", createOperation)
	}

	started = startScenarioSandbox(t, ctx, fixture, handle, "durability-start-after-snapshot")
	assertScenarioAPIError(
		t,
		restoreScenarioSnapshot(
			ctx,
			handle,
			snapshot.ID,
			uniqueScenarioKey(t, "running-restore"),
			sandboxRevisionETag(started.Revision),
		),
		http.StatusConflict,
		"state_conflict",
	)

	otherProfile := createScenarioProfile(
		t,
		fixture,
		"scenario-snapshot-other-sandbox",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateStopped),
	)
	other, otherCreate := createScenarioSandbox(t, fixture, otherProfile, "snapshot-other-sandbox")
	otherStopped := waitForSandbox(t, ctx, other, secondboxclient.SandboxStateStopped)
	waitForScenarioOperation(t, ctx, fixture.subject, otherCreate)
	otherStopped, err := other.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertScenarioAPIError(
		t,
		restoreScenarioSnapshot(
			ctx,
			other,
			snapshot.ID,
			uniqueScenarioKey(t, "cross-sandbox-restore"),
			sandboxRevisionETag(otherStopped.Revision),
		),
		http.StatusNotFound,
		"not_found",
	)

	writeScenarioFile(t, ctx, fixture.subject, handle, path, after)
	stopped = stopScenarioSandbox(t, ctx, fixture, handle, "durability-stop-for-restore")
	previousGeneration := stopped.Generation
	staleHeaders := handle.GenerationHeaders("")

	restored, restoreOperation := restoreScenarioSnapshotWithReplay(
		t,
		ctx,
		fixture,
		handle,
		snapshot.ID,
	)
	if restoreOperation.Kind != "snapshot_restore" ||
		restoreOperation.Snapshot == nil ||
		restoreOperation.Snapshot.ID != snapshot.ID {
		t.Fatalf("SecondBox scenario Snapshot restore Operation = %#v", restoreOperation)
	}
	if restored.Generation != previousGeneration+1 ||
		restored.State != contracts.SandboxStateStopped {
		t.Fatalf(
			"SecondBox scenario restored Sandbox = %#v, want generation %d stopped",
			restored,
			previousGeneration+1,
		)
	}

	started = startScenarioSandbox(t, ctx, fixture, handle, "durability-start-restored")
	if got := readScenarioFile(t, ctx, fixture.subject, handle, path); !bytes.Equal(got, before) {
		t.Fatalf("SecondBox scenario restored content = %q, want %q", got, before)
	}
	staleHeaders.Set("Idempotency-Key", uniqueScenarioKey(t, "stale-generation"))
	var staleOutcome secondboxclient.ExecOutcome
	err = fixture.subject.RequestJSON(
		ctx,
		"executeSandboxCommand",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": started.ID},
			Headers:        staleHeaders,
			Body:           scenarioBody(t, scenarioExecRequest("printf stale", 1024)),
		},
		&staleOutcome,
	)
	assertScenarioAPIError(t, err, http.StatusConflict, "generation_fenced")

	got := scenarioJSON[contracts.Snapshot](
		t,
		ctx,
		fixture.subject,
		"getSnapshot",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"snapshotId": snapshot.ID},
		},
	)
	if got.ID != snapshot.ID || got.State != "ready" {
		t.Fatalf("SecondBox scenario get Snapshot = %#v", got)
	}
	page := scenarioJSON[contracts.SnapshotPage](
		t,
		ctx,
		fixture.subject,
		"listSandboxSnapshots",
		secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": started.ID},
			QueryParameters: url.Values{"limit": []string{"200"}},
		},
	)
	if !scenarioSnapshotPageContains(page, snapshot.ID) {
		t.Fatalf("SecondBox scenario Snapshot page = %#v", page)
	}

	stopScenarioSandbox(t, ctx, fixture, handle, "durability-stop-for-snapshot-delete")
	deleteOperation := deleteScenarioSnapshotWithReplay(
		t,
		ctx,
		fixture,
		snapshot.ID,
	)
	if deleteOperation.Kind != "snapshot_delete" ||
		deleteOperation.Snapshot == nil ||
		deleteOperation.Snapshot.ID != snapshot.ID {
		t.Fatalf("SecondBox scenario Snapshot delete Operation = %#v", deleteOperation)
	}
	page = scenarioJSON[contracts.SnapshotPage](
		t,
		ctx,
		fixture.subject,
		"listSandboxSnapshots",
		secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": started.ID},
			QueryParameters: url.Values{"limit": []string{"200"}},
		},
	)
	if scenarioSnapshotPageContains(page, snapshot.ID) {
		t.Fatalf("SecondBox scenario deleted Snapshot remains listed: %#v", page)
	}
	assertScenarioAPIError(
		t,
		getScenarioSnapshot(ctx, fixture.subject, snapshot.ID),
		http.StatusNotFound,
		"not_found",
	)
	startScenarioSandbox(t, ctx, fixture, handle, "durability-start-after-snapshot-delete")
	if got := readScenarioFile(t, ctx, fixture.subject, handle, path); !bytes.Equal(got, before) {
		t.Fatalf(
			"SecondBox scenario active workspace changed after Snapshot deletion: got %q, want %q",
			got,
			before,
		)
	}
}

func TestScenarioSnapshotLimitAndRetention(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateStopped)
	spec.Retention.SnapshotLimit = 1
	spec.Retention.SnapshotRetentionSeconds = 2
	profile := createScenarioProfile(t, fixture, "scenario-snapshot-retention", spec)
	handle, createOperation := createScenarioSandbox(t, fixture, profile, "snapshot-retention")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	waitForScenarioOperation(t, ctx, fixture.subject, createOperation)
	if _, err := handle.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	first, _ := createScenarioSnapshot(
		t,
		ctx,
		fixture,
		handle,
		"retention-first",
		map[string]string{"scenario": "retention"},
		false,
	)
	if first.RetainUntil == nil {
		t.Fatalf("SecondBox scenario retained Snapshot = %#v", first)
	}
	retention := first.RetainUntil.Sub(first.CreatedAt)
	if retention < 1900*time.Millisecond || retention > 2100*time.Millisecond {
		t.Fatalf("SecondBox scenario Snapshot retention = %s, want 2s", retention)
	}

	current, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handle.CreateSnapshot(
		ctx,
		secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, "snapshot-over-limit"),
			IfMatch:        sandboxRevisionETag(current.Revision),
		},
		contracts.CreateSnapshotRequest{
			Name:     "over-limit",
			Metadata: map[string]string{"scenario": "limit"},
		},
	)
	assertScenarioAPIError(t, err, http.StatusTooManyRequests, "quota_exceeded")

	retentionDeadline := time.Now().Add(30 * time.Second)
	for {
		err = getScenarioSnapshot(ctx, fixture.subject, first.ID)
		var apiError *secondboxclient.APIError
		if errors.As(err, &apiError) &&
			apiError.StatusCode == http.StatusNotFound &&
			apiError.Problem != nil &&
			apiError.Problem.Code == "not_found" {
			break
		}
		if err != nil {
			t.Fatalf("SecondBox scenario retained Snapshot lookup: %v", err)
		}
		if time.Now().After(retentionDeadline) {
			t.Fatalf("SecondBox scenario Snapshot %s remained readable after retention", first.ID)
		}
		time.Sleep(100 * time.Millisecond)
	}

	var second contracts.Operation
	for {
		current, err = handle.Refresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		second, err = handle.CreateSnapshot(
			ctx,
			secondboxclient.LifecycleOptions{
				IdempotencyKey: uniqueScenarioKey(t, "snapshot-after-retention"),
				IfMatch:        sandboxRevisionETag(current.Revision),
			},
			contracts.CreateSnapshotRequest{
				Name:     "after-retention",
				Metadata: map[string]string{"scenario": "retention"},
			},
		)
			if err == nil {
				break
			}
			var apiError *secondboxclient.APIError
			if !errors.As(err, &apiError) ||
				apiError.Problem == nil ||
				!((apiError.StatusCode == http.StatusTooManyRequests &&
					apiError.Problem.Code == "quota_exceeded") ||
					(apiError.StatusCode == http.StatusConflict &&
						apiError.Problem.Code == "workspace_mutation_conflict")) {
				t.Fatalf("SecondBox scenario create Snapshot after retention: %v", err)
			}
		if time.Now().After(retentionDeadline) {
			t.Fatal("SecondBox scenario Snapshot retention did not release the configured limit")
		}
		time.Sleep(100 * time.Millisecond)
	}
	terminal := waitForScenarioOperation(t, ctx, fixture.subject, second)
	if terminal.Snapshot == nil || terminal.Snapshot.State != "ready" {
		t.Fatalf("SecondBox scenario post-retention Snapshot Operation = %#v", terminal)
	}
	deleteScenarioSnapshotWithReplay(t, ctx, fixture, terminal.Snapshot.ID)
}

func createScenarioSnapshot(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	name string,
	metadata map[string]string,
	assertReplay bool,
) (contracts.Snapshot, contracts.Operation) {
	t.Helper()
	current, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatalf("SecondBox scenario refresh before Snapshot create: %v", err)
	}
	key := uniqueScenarioKey(t, "snapshot-create")
	options := secondboxclient.LifecycleOptions{
		IdempotencyKey: key,
		IfMatch:        sandboxRevisionETag(current.Revision),
	}
	request := contracts.CreateSnapshotRequest{Name: name, Metadata: metadata}
	operation, err := handle.CreateSnapshot(ctx, options, request)
	if err != nil {
		t.Fatalf("SecondBox scenario create Snapshot: %v", err)
	}
	if assertReplay {
		replay, replayErr := handle.CreateSnapshot(ctx, options, request)
		if replayErr != nil {
			t.Fatalf("SecondBox scenario replay Snapshot create: %v", replayErr)
		}
		if replay.ID != operation.ID {
			t.Fatalf(
				"SecondBox scenario Snapshot create replay Operation = %s, want %s",
				replay.ID,
				operation.ID,
			)
		}
	}
	terminal := waitForScenarioOperation(t, ctx, fixture.subject, operation)
	if terminal.Snapshot == nil || terminal.Snapshot.State != "ready" {
		t.Fatalf("SecondBox scenario Snapshot create terminal Operation = %#v", terminal)
	}
	if _, err := handle.Refresh(ctx); err != nil {
		t.Fatalf("SecondBox scenario refresh after Snapshot create: %v", err)
	}
	return *terminal.Snapshot, terminal
}

func restoreScenarioSnapshot(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	snapshotID string,
	key string,
	ifMatch string,
) error {
	_, err := handle.Restore(ctx, secondboxclient.LifecycleOptions{
		IdempotencyKey: key,
		IfMatch:        ifMatch,
	}, snapshotID)
	return err
}

func restoreScenarioSnapshotWithReplay(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	snapshotID string,
) (contracts.Sandbox, contracts.Operation) {
	t.Helper()
	current, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := uniqueScenarioKey(t, "snapshot-restore")
	options := secondboxclient.LifecycleOptions{
		IdempotencyKey: key,
		IfMatch:        sandboxRevisionETag(current.Revision),
	}
	operation, err := handle.Restore(ctx, options, snapshotID)
	if err != nil {
		t.Fatalf("SecondBox scenario restore Snapshot: %v", err)
	}
	replay, err := handle.Restore(ctx, options, snapshotID)
	if err != nil {
		t.Fatalf("SecondBox scenario replay Snapshot restore: %v", err)
	}
	if replay.ID != operation.ID {
		t.Fatalf(
			"SecondBox scenario Snapshot restore replay Operation = %s, want %s",
			replay.ID,
			operation.ID,
		)
	}
	terminal := waitForScenarioOperation(t, ctx, fixture.subject, operation)
	current, err = handle.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return current, terminal
}

func deleteScenarioSnapshotWithReplay(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	snapshotID string,
) contracts.Operation {
	t.Helper()
	key := uniqueScenarioKey(t, "snapshot-delete")
	operation, err := fixture.subject.DeleteSnapshot(ctx, snapshotID, key)
	if err != nil {
		t.Fatalf("SecondBox scenario delete Snapshot: %v", err)
	}
	replay, err := fixture.subject.DeleteSnapshot(ctx, snapshotID, key)
	if err != nil {
		t.Fatalf("SecondBox scenario replay Snapshot delete: %v", err)
	}
	if replay.ID != operation.ID {
		t.Fatalf(
			"SecondBox scenario Snapshot delete replay Operation = %s, want %s",
			replay.ID,
			operation.ID,
		)
	}
	return waitForScenarioOperation(t, ctx, fixture.subject, operation)
}

func getScenarioSnapshot(
	ctx context.Context,
	client *secondboxclient.Client,
	snapshotID string,
) error {
	var snapshot contracts.Snapshot
	return client.RequestJSON(ctx, "getSnapshot", secondboxclient.CallOptions{
		PathParameters: map[string]string{"snapshotId": snapshotID},
	}, &snapshot)
}

func scenarioSnapshotPageContains(page contracts.SnapshotPage, snapshotID string) bool {
	for _, snapshot := range page.Items {
		if snapshot.ID == snapshotID {
			return true
		}
	}
	return false
}

func assertScenarioAPIError(
	t *testing.T,
	err error,
	status int,
	code string,
) {
	t.Helper()
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) ||
		apiError.StatusCode != status ||
		apiError.Problem == nil ||
		apiError.Problem.Code != code {
		t.Fatalf(
			"SecondBox scenario API error = %#v raw=%v, want status=%d code=%s",
			apiError,
			err,
			status,
			code,
		)
	}
}
