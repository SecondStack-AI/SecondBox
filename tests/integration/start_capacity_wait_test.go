package integration_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestStartCapacityWaitExpiryReleasesTheWorkspaceWithoutPlacing covers a start
// that its caller bounded and its home Runner never placed. While deferred it
// holds the Workspace mutation slot, which also refuses stop and delete, so
// only expiry can end it once the caller has gone. Expiry must fail the
// Operation with a typed retryable problem, release the slot, and leave the
// Sandbox wanted stopped, so that the Runner returning later boots nothing.
func TestStartCapacityWaitExpiryReleasesTheWorkspaceWithoutPlacing(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "capacity-wait")
	const capacityWait = 1500
	request := contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: capacityWait}
	key := "capacity-wait-start-" + sandboxID
	admittedAt := time.Now().UTC()
	operation, replayed, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, key, stopped.Revision, request,
	)
	if err != nil || replayed {
		t.Fatalf("bounded start replayed=%t error=%v", replayed, err)
	}
	// The bound is part of the canonical request.
	if _, replayed, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, key, stopped.Revision, request,
	); err != nil || !replayed {
		t.Fatalf("identical bounded start replayed=%t error=%v", replayed, err)
	}
	changed := request
	changed.CapacityWaitMilliseconds = capacityWait + 1
	if _, _, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, key, stopped.Revision, changed,
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("start reusing the key with another capacity wait error = %v, want idempotency conflict", err)
	}

	setFixtureRunnerState(t, fixture, "offline")
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID)
	if !scheduled {
		t.Fatal("deferred bounded start left the Sandbox unscheduled")
	}
	deadline := admittedAt.Add(capacityWait * time.Millisecond)
	if dueAt.After(deadline.Add(time.Second)) {
		t.Fatalf("deferred bounded start is due at %s, after its capacity wait ends near %s", dueAt, deadline)
	}
	deferred, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	// The deferred start's accepted running intent reserves compute quota.
	deferredUsage, err := fixture.controlPlane.GetSubjectCapacity(t.Context(), fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.StopSandbox(
		t.Context(), fixture.principal, sandboxID, "capacity-wait-stop-"+sandboxID, deferred.Revision,
	); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("stop during a deferred start error = %v, want the Workspace mutation conflict", err)
	}

	for passes := 0; ; passes++ {
		if passes == deferralPasses {
			t.Fatalf("bounded start was still pending after %d reconciliation passes", passes)
		}
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
		current, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contracts.OperationStatePending {
			operation = current
			break
		}
	}
	if operation.State != contracts.OperationStateFailed || operation.Error == nil ||
		operation.Error.Code != "capacity_wait_expired" || !operation.Error.Retryable {
		t.Fatalf("expired bounded start Operation = %#v error=%#v", operation, operation.Error)
	}
	if operation.CompletedAt == nil || operation.CompletedAt.Before(deadline) {
		t.Fatalf("bounded start failed at %v, before its capacity wait ended at %s", operation.CompletedAt, deadline)
	}
	expired, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != contracts.SandboxStateStopped ||
		expired.DesiredState != contracts.SandboxDesiredStateStopped ||
		expired.Revision <= deferred.Revision {
		t.Fatalf("expired start left Sandbox %s/%s at revision %d (deferred at %d)",
			expired.State, expired.DesiredState, expired.Revision, deferred.Revision)
	}
	var mutationState string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT workspace.mutation_state FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.id=$1`, sandboxID,
	).Scan(&mutationState); err != nil {
		t.Fatal(err)
	}
	if mutationState != "" {
		t.Fatalf("expired start kept the Workspace mutation %q", mutationState)
	}
	if _, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatal("expired start left the Sandbox scheduled for reconciliation")
	}
	expiredUsage, err := fixture.controlPlane.GetSubjectCapacity(t.Context(), fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if expiredUsage.Usage.ActiveInstances != deferredUsage.Usage.ActiveInstances-1 ||
		expiredUsage.Usage.VCPUCount != deferredUsage.Usage.VCPUCount-expired.Resources.VCPUCount {
		t.Fatalf("expiry left compute usage at %#v, want one Instance below the deferred %#v",
			expiredUsage.Usage, deferredUsage.Usage)
	}

	// The Runner coming back must not boot the abandoned start.
	setFixtureRunnerState(t, fixture, "ready")
	if _, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatal("returned Runner rescheduled the expired start")
	}
	if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
		t.Fatalf("expired start placed %d Assignments, want none", count)
	}

	// An unbounded retry under a new key places normally.
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID, "capacity-wait-retry-"+sandboxID,
		expired.Revision, testExecutionImage(),
	); err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateStarting {
		t.Fatalf("retried start left the Sandbox %q, want starting", started.State)
	}
}

// TestStartPlacedWithinItsCapacityWaitIsUnaffected pins the other side of the
// bound: a placeable start places on its first pass whatever bound it carries.
func TestStartPlacedWithinItsCapacityWaitIsUnaffected(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "capacity-wait-placed")
	operation, _, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, "capacity-wait-placed-"+sandboxID, stopped.Revision,
		contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateStarting || sandboxAssignmentCount(t, fixture, sandboxID) != 1 {
		t.Fatalf("bounded placeable start left the Sandbox %q", started.State)
	}
	current, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Error != nil {
		t.Fatalf("placed bounded start Operation carries error %#v", current.Error)
	}
}

// TestCreateCapacityWaitBoundsTheInitialStart covers a Profile whose initial
// state is running. Its create Operation stays pending through the Workspace
// create and the start that follows, which keeps that Operation, so the
// create's bound is what ends a start the home Runner never places.
func TestCreateCapacityWaitBoundsTheInitialStart(t *testing.T) {
	fixture := newTeardownFixture(t)
	setFixtureProfileInitialState(t, fixture, contracts.SandboxDesiredStateRunning)
	const capacityWait = 1500
	admittedAt := time.Now().UTC()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		"capacity-create-running-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
		contracts.CreateSandboxRequest{Image: testExecutionImage(),
			Profile:                  fixture.profileName,
			Metadata:                 map[string]string{"fixture": "create-capacity-wait"},
			CapacityWaitMilliseconds: capacityWait,
		},
	)
	if err != nil || !created {
		t.Fatalf("bounded create created=%t error=%v", created, err)
	}
	sandboxID := operation.SandboxID
	fixture.completeWorkspaceCreate(t, sandboxID, operation.ID)
	pending, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != contracts.OperationStatePending {
		t.Fatalf("running-initial create Operation after its Workspace = %q, want pending", pending.State)
	}

	setFixtureRunnerState(t, fixture, "offline")
	for passes := 0; ; passes++ {
		if passes == deferralPasses {
			t.Fatalf("bounded create was still pending after %d reconciliation passes", passes)
		}
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
		current, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contracts.OperationStatePending {
			operation = current
			break
		}
	}
	if operation.State != contracts.OperationStateFailed || operation.Error == nil ||
		operation.Error.Code != "capacity_wait_expired" {
		t.Fatalf("expired bounded create Operation = %#v error=%#v", operation, operation.Error)
	}
	if deadline := admittedAt.Add(capacityWait * time.Millisecond); operation.CompletedAt.Before(deadline) {
		t.Fatalf("bounded create failed at %s, before its capacity wait ended at %s", operation.CompletedAt, deadline)
	}
	expired, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != contracts.SandboxStateStopped ||
		expired.DesiredState != contracts.SandboxDesiredStateStopped ||
		expired.Workspace.State != "ready" {
		t.Fatalf("expired create left Sandbox %s/%s with Workspace %q",
			expired.State, expired.DesiredState, expired.Workspace.State)
	}
	setFixtureRunnerState(t, fixture, "ready")
	if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
		t.Fatalf("expired create placed %d Assignments, want none", count)
	}
	// The Sandbox survives the failed create and starts like any stopped one.
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID, "capacity-create-retry-"+sandboxID,
		expired.Revision, testExecutionImage(),
	); err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateStarting {
		t.Fatalf("start after the expired create left the Sandbox %q, want starting", started.State)
	}
}

// setFixtureProfileInitialState rewrites the initial state of the fixture
// Profile's current revision, which every later create in the fixture reads.
func setFixtureProfileInitialState(t *testing.T, fixture *teardownFixture, state string) {
	t.Helper()
	tag, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.profile_revisions AS revision
		SET spec_json=jsonb_set(revision.spec_json,'{lifecycle,initialState}',to_jsonb($2::text))
		FROM secondbox.profiles AS profile
		WHERE profile.name=$1 AND revision.id=profile.current_revision_id`,
		fixture.profileName, state,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("Profile %s initial state update changed %d rows", fixture.profileName, tag.RowsAffected())
	}
}

// createStoppedFixtureSandbox creates one Sandbox and completes its Workspace,
// leaving it stopped and holding no Instance.
func createStoppedFixtureSandbox(
	t *testing.T,
	fixture *teardownFixture,
	label string,
) (string, contracts.Sandbox) {
	t.Helper()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		"capacity-create-"+label+"-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
		contracts.CreateSandboxRequest{Image: testExecutionImage(),
			Profile:  fixture.profileName,
			Metadata: map[string]string{"fixture": "start-capacity-wait"},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox created=%t error=%v", created, err)
	}
	fixture.completeWorkspaceCreate(t, operation.SandboxID, operation.ID)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, operation.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contracts.SandboxStateStopped {
		t.Fatalf("created Sandbox state = %q, want stopped", current.State)
	}
	return operation.SandboxID, current
}
