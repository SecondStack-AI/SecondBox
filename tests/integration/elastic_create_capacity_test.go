package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type elasticCreateArchiveProvider struct{}

func (elasticCreateArchiveProvider) Transfer(context.Context, string, string, *runnerv1.WorkspaceArchiveDescriptor) (*runnerv1.WorkspaceArchiveTransfer, error) {
	return nil, errors.New("unexpected archive transfer in initial capacity qualification")
}
func (elasticCreateArchiveProvider) Delete(context.Context, string) error {
	return errors.New("unexpected archive deletion in initial capacity qualification")
}

func TestElasticCreateWaitsForAnEmptyDeclaredPoolThenPlacesAfterRegistration(t *testing.T) {
	fixture := newTeardownFixture(t)
	notifications := worknotify.NewHub()
	listener, err := worknotify.NewPostgresListener(t.Context(), integrationDatabaseURL, notifications)
	if err != nil {
		t.Fatal(err)
	}
	listenerContext, cancelListener := context.WithCancel(t.Context())
	listenerErrors := make(chan error, 1)
	go func() { listenerErrors <- listener.Run(listenerContext) }()
	t.Cleanup(func() {
		cancelListener()
		select {
		case err := <-listenerErrors:
			if err != nil {
				t.Errorf("work listener failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("work listener did not stop")
		}
	})
	barrierKey := "elastic-notification-barrier-" + fixture.runnerID
	barrierWake, unsubscribeBarrier := notifications.Subscribe(worknotify.KindRunnerCommand, barrierKey)
	defer unsubscribeBarrier()
	waitForNotificationBarrier := func() {
		t.Helper()
		if _, err := fixture.pool.Exec(t.Context(), `SELECT pg_notify('secondbox_work',json_build_object('kind','runner_command','key',$1::text)::text)`, barrierKey); err != nil {
			t.Fatal(err)
		}
		select {
		case <-barrierWake:
		case err := <-listenerErrors:
			listenerErrors <- err
			t.Fatalf("unhomed create stopped the background work listener: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("background listener did not process the notification barrier")
		}
	}
	fixture.store.ConfigureWorkspaceArchives(elasticCreateArchiveProvider{})
	setFixtureProfileInitialState(t, fixture, contracts.SandboxDesiredStateRunning)
	if _, err := fixture.pool.Exec(t.Context(), `DELETE FROM secondbox.runners WHERE id=$1`, fixture.runnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET ready_runner_count=0 WHERE name=$1`, fixture.poolName); err != nil {
		t.Fatal(err)
	}
	request := contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: fixture.profileName, Metadata: map[string]string{}, CapacityWaitMilliseconds: 60_000}
	response := quiescenceHTTPRequest(t, fixture, http.MethodPost, "/v1/sandboxes", "elastic-zero-pool-create", 0, request)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("empty declared pool create status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	if err := json.NewDecoder(response.Body).Decode(&operation); err != nil {
		t.Fatal(err)
	}
	var home string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT home_runner_id FROM secondbox.workspaces WHERE sandbox_id=$1`, operation.SandboxID).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != "" {
		t.Fatalf("unplaced create acquired home=%q", home)
	}
	observation, err := fixture.store.ReadRunnerFleet(t.Context(), fixture.poolName, time.Now().UTC())
	if err != nil || observation.Demand.Instances != 1 || len(observation.Runners) != 0 {
		t.Fatalf("zero-node demand=%#v err=%v", observation, err)
	}
	if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fixture.pendingLocalWorkspaceCommand(t, operation.SandboxID).Correlation.RunnerId; got != "" {
		t.Fatalf("unavailable capacity dispatched to %q", got)
	}
	waitForNotificationBarrier()

	now := time.Now().UTC()
	registration := task4Registration(fixture.runnerID, fixture.connectionID, fixture.poolName)
	registration.Capabilities.ClientSelectedImageReady = true
	if _, err := fixture.stateStore.RecordRegistration(t.Context(), registration, now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), task4Heartbeat(fixture.runnerID, fixture.connectionID, "elastic-heartbeat", 2, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE), now); err != nil {
		t.Fatal(err)
	}
	task4CompleteWorkspaceReconciliation(t, fixture.stateStore, fixture.runnerID, fixture.connectionID, 3, nil, now)
	fixture.sequence = 3
	waitForNotificationBarrier()
	runnerWake, unsubscribeRunner := notifications.Subscribe(worknotify.KindRunnerCommand, fixture.runnerID)
	defer unsubscribeRunner()
	if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runnerWake:
	case err := <-listenerErrors:
		listenerErrors <- err
		t.Fatalf("placing unhomed create stopped the work listener: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("binding the pending create command did not wake its Runner")
	}
	if got := fixture.pendingLocalWorkspaceCommand(t, operation.SandboxID).Correlation.RunnerId; got != fixture.runnerID {
		t.Fatalf("registered Runner command target=%q want=%q", got, fixture.runnerID)
	}
	fixture.completeWorkspaceCreate(t, operation.SandboxID, operation.ID)
	fixture.runLifecycle(t, operation.SandboxID, lifecycle.ActionStartInstance)
	fixture.completeAssignmentReady(t, operation.SandboxID)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, operation.SandboxID)
	if err != nil || current.State != contracts.SandboxStateReady {
		t.Fatalf("placed Sandbox=%#v err=%v", current, err)
	}
}

func TestElasticCreateStillRefusesIncompatibleOrUnavailableDeclaredPools(t *testing.T) {
	for _, failure := range []string{"offline", "architecture", "capability", "unbounded"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			fixture.store.ConfigureWorkspaceArchives(elasticCreateArchiveProvider{})
			if _, err := fixture.pool.Exec(t.Context(), `DELETE FROM secondbox.runners WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET ready_runner_count=0 WHERE name=$1`, fixture.poolName); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "offline":
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET state='offline' WHERE name=$1`, fixture.poolName); err != nil {
					t.Fatal(err)
				}
			case "architecture":
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET architectures_json='["arm64"]' WHERE name=$1`, fixture.poolName); err != nil {
					t.Fatal(err)
				}
			case "capability":
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET capabilities_json='["compute"]' WHERE name=$1`, fixture.poolName); err != nil {
					t.Fatal(err)
				}
			}
			wait := contracts.CapacityWaitBound(60000)
			if failure == "unbounded" {
				wait = 0
			}
			response := quiescenceHTTPRequest(t, fixture, http.MethodPost, "/v1/sandboxes", "elastic-refuse-"+failure, 0, contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: fixture.profileName, Metadata: map[string]string{}, CapacityWaitMilliseconds: wait})
			defer response.Body.Close()
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("incompatible pool create status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
		})
	}
}

func TestElasticCreateCancellationAndExpiryLeaveNoWorkForAbsentRunner(t *testing.T) {
	for _, action := range []string{"stop", "delete", "expiry"} {
		t.Run(action, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			fixture.store.ConfigureWorkspaceArchives(elasticCreateArchiveProvider{})
			setFixtureProfileInitialState(t, fixture, contracts.SandboxDesiredStateRunning)
			if _, err := fixture.pool.Exec(t.Context(), `DELETE FROM secondbox.runners WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET ready_runner_count=0 WHERE name=$1`, fixture.poolName); err != nil {
				t.Fatal(err)
			}
			response := quiescenceHTTPRequest(t, fixture, http.MethodPost, "/v1/sandboxes", "elastic-empty-create-"+action, 0, contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: fixture.profileName, Metadata: map[string]string{}, CapacityWaitMilliseconds: 60_000})
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("create status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var created contracts.Operation
			if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if action == "expiry" {
				if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC().Add(2*time.Minute), nil); err != nil {
					t.Fatal(err)
				}
			} else if action == "stop" {
				response := quiescenceHTTPRequest(t, fixture, http.MethodPost, "/v1/sandboxes/"+created.SandboxID+":stop", "elastic-empty-stop", 1, nil)
				if response.StatusCode != http.StatusAccepted {
					t.Fatalf("stop status=%d body=%s", response.StatusCode, readResponse(t, response))
				}
				var stopped contracts.Operation
				if err := json.NewDecoder(response.Body).Decode(&stopped); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if stopped.State != contracts.OperationStateSucceeded {
					t.Fatalf("never-placed stop=%#v", stopped)
				}
			}
			current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, created.SandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if action != "delete" {
				var pending int
				if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND state NOT IN ('failed','runner_failed','succeeded')`, created.SandboxID).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if current.State != contracts.SandboxStateFailed || pending != 0 {
					t.Fatalf("%s left never-placed create state=%q pendingEffects=%d", action, current.State, pending)
				}
				observation, err := fixture.store.ReadRunnerFleet(t.Context(), fixture.poolName, time.Now().UTC())
				if err != nil || observation.Demand.Instances != 0 {
					t.Fatalf("%s left fleet demand=%#v err=%v", action, observation.Demand, err)
				}
			}
			response = quiescenceHTTPRequest(t, fixture, http.MethodDelete, "/v1/sandboxes/"+created.SandboxID, "elastic-empty-delete", current.Revision, nil)
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("delete status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var deleted contracts.Operation
			if err := json.NewDecoder(response.Body).Decode(&deleted); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if deleted.State != contracts.OperationStateSucceeded {
				t.Fatalf("never-placed delete=%#v", deleted)
			}
			operation, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := contracts.OperationStateCancelled
			if action == "expiry" {
				want = contracts.OperationStateFailed
			}
			if operation.State != want {
				t.Fatalf("create after %s=%#v", action, operation)
			}
			if action == "expiry" && (operation.Error == nil || operation.Error.Code != "capacity_wait_expired" || !operation.Error.Retryable) {
				t.Fatalf("expired never-placed create lacks retryable deadline evidence: %#v", operation)
			}
			var state, workspaceState, mutation string
			var pendingEffects, pendingCommands int
			if err := fixture.pool.QueryRow(t.Context(), `SELECT sandbox.state,workspace.state,workspace.mutation_kind,(SELECT count(*) FROM secondbox.lifecycle_effects effect WHERE effect.sandbox_id=sandbox.id AND effect.state NOT IN ('failed','runner_failed','succeeded')),(SELECT count(*) FROM secondbox.runner_commands command JOIN secondbox.lifecycle_effects effect ON effect.id=command.assignment_id WHERE effect.sandbox_id=sandbox.id AND command.state IN ('pending','delivering','delivered')) FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id WHERE sandbox.id=$1`, created.SandboxID).Scan(&state, &workspaceState, &mutation, &pendingEffects, &pendingCommands); err != nil {
				t.Fatal(err)
			}
			if state != "deleted" || workspaceState != "deleted" || mutation != "" || pendingEffects != 0 || pendingCommands != 0 {
				t.Fatalf("cleanup state=%q workspace=%q mutation=%q pendingEffects=%d pendingCommands=%d", state, workspaceState, mutation, pendingEffects, pendingCommands)
			}
			observation, err := fixture.store.ReadRunnerFleet(t.Context(), fixture.poolName, time.Now().UTC())
			if err != nil || observation.Demand.Instances != 0 {
				t.Fatalf("terminal create demand=%#v err=%v", observation.Demand, err)
			}
		})
	}
}

func TestElasticRunnerNotificationsPreserveEagerDelivery(t *testing.T) {
	fixture := newTeardownFixture(t)
	hub := worknotify.NewHub()
	listener, err := worknotify.NewPostgresListener(t.Context(), integrationDatabaseURL, hub)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	completed := make(chan error, 1)
	go func() { completed <- listener.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-completed:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("eager-delivery notification listener did not stop")
		}
	})
	wake, unsubscribe := hub.Subscribe(worknotify.KindRunnerCommand, "eager-"+fixture.runnerID)
	defer unsubscribe()
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at) VALUES($1,$1,$1,'assignment','','delivering','eager-test',0,$2,$2)`, "eager-"+fixture.runnerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	case err := <-completed:
		completed <- err
		t.Fatalf("eager dispatch stopped notification listener: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("already-delivering command insertion did not wake its Runner")
	}
}
