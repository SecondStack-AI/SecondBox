package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestRunningCreateReceiptsWakeBackgroundLifecycleThroughImagePreparation(t *testing.T) {
	fixture := newTeardownFixture(t)
	fixture.deferOtherSandboxes(t, "")
	setFixtureProfileInitialState(t, fixture, contracts.SandboxDesiredStateRunning)
	// A long fallback proves that real commit notifications drive both steps.
	fixture.reconciler.PollInterval = time.Hour
	hub := worknotify.NewHub()
	listener, err := worknotify.NewPostgresListener(t.Context(), integrationDatabaseURL, hub)
	if err != nil {
		t.Fatal(err)
	}
	wakes, unsubscribe := hub.Subscribe(worknotify.KindLifecycle, "")
	defer unsubscribe()
	ctx, cancel := context.WithCancel(t.Context())
	listenerErrors := make(chan error, 1)
	workerErrors := make(chan error, 1)
	go func() { listenerErrors <- listener.Run(ctx) }()
	go func() {
		for {
			found, err := fixture.reconciler.RunBatch(ctx, time.Now, ports.LifecycleWakeTriggerNotify)
			if err != nil {
				if lifecycle.IsRetryableReconcileError(err) {
					continue
				}
				workerErrors <- err
				return
			}
			if found {
				continue
			}
			select {
			case <-ctx.Done():
				workerErrors <- nil
				return
			case <-wakes:
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		for _, done := range []chan error{workerErrors, listenerErrors} {
			select {
			case err := <-done:
				if err != nil && ctx.Err() == nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("background lifecycle test worker did not stop")
			}
		}
	})
	operation, _, err := fixture.controlPlane.CreateSandboxOperation(t.Context(), fixture.principal, "background-running-create", contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: fixture.profileName, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.completeWorkspaceCreate(t, operation.SandboxID, operation.ID)
	await := func(stage string, ready func() bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if ready() {
				return
			}
			select {
			case err := <-workerErrors:
				workerErrors <- err
				t.Fatalf("%s background worker stopped: %v", stage, err)
			case err := <-listenerErrors:
				listenerErrors <- err
				t.Fatalf("%s listener stopped: %v", stage, err)
			case <-timer.C:
				t.Fatalf("%s did not advance through commit notifications", stage)
			case <-tick.C:
			}
		}
	}
	await("CREATE receipt to selected-image preparation", func() bool {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND kind='prepare_image' AND state='queued'`, operation.SandboxID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	})
	runtimeComponent, toolchainComponent := selectedImageComponents()
	manifest, err := json.Marshal(map[string]any{"architecture": "amd64", "guestProtocol": map[string]int{"minimum": 1, "maximum": 1}, "runtimeBundle": runtimeComponent, "toolchainBundle": toolchainComponent})
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.nextSequence()
	fixture.recordEvent(t, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: fmt.Sprintf("background-prepare-%d", sequence), Sequence: sequence, OperationId: operation.ID, ResolvedDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Manifest: manifest}}})
	await("image terminal to Assignment", func() bool { return sandboxAssignmentCount(t, fixture, operation.SandboxID) == 1 })
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, operation.SandboxID)
	if err != nil || current.State != contracts.SandboxStateStarting {
		t.Fatalf("background admitted Sandbox=%#v error=%v", current, err)
	}
}
