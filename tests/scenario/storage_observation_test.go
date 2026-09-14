//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioWorkspaceStorageObservationPreservesStoppedFilesAndActivity(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(t, fixture, "scenario-storage-observation", scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning))
	handle, operation := createScenarioSandbox(t, fixture, profile, "storage-observation")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	content := []byte("retained file measured without compute activity\n")
	writeScenarioFile(t, ctx, fixture.subject, handle, "observation.txt", content)
	stopped := stopScenarioSandbox(t, ctx, fixture, handle, "storage-observation-stop")
	if stopped.LastActivityAt == nil {
		t.Fatal("file write did not establish activity")
	}
	var observed contracts.Sandbox
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		observed = scenarioJSON[contracts.Sandbox](t, ctx, fixture.subject, "getSandbox", secondboxclient.CallOptions{PathParameters: map[string]string{"sandboxId": stopped.ID}})
		storage := observed.Workspace.StorageObservation
		if storage.Status == "available" && storage.ObservedAt != nil && storage.ObservedAt.After(stopped.UpdatedAt) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Runner storage observation did not arrive: %+v", storage)
		case <-ticker.C:
		}
	}
	storage := observed.Workspace.StorageObservation
	if storage.AllocatedBytes == nil || *storage.AllocatedBytes <= 0 || observed.Instance != nil || observed.State != contracts.SandboxStateStopped || observed.Generation != stopped.Generation || observed.Revision != stopped.Revision || !observed.LastActivityAt.Equal(*stopped.LastActivityAt) {
		t.Fatalf("storage read changed stopped resource: %+v", observed)
	}
	startScenarioSandbox(t, ctx, fixture, handle, "storage-observation-resume")
	if actual := readScenarioFile(t, ctx, fixture.subject, handle, "observation.txt"); !bytes.Equal(actual, content) {
		t.Fatalf("measurement changed files: %q", actual)
	}
	stopScenarioSandbox(t, ctx, fixture, handle, "storage-observation-final-stop")
	deletedOperation := requestScenarioLifecycle(t, ctx, handle, "delete", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
		return handle.Delete(ctx, options)
	})
	waitForScenarioOperation(t, ctx, fixture.subject, deletedOperation)
	deleted := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
	if deleted.Workspace.StorageObservation.Reason != "deleted" || deleted.Workspace.StorageObservation.AllocatedBytes != nil {
		t.Fatalf("deleted storage observation = %+v", deleted.Workspace.StorageObservation)
	}
}
