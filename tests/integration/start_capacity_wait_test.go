package integration_test

import (
	"crypto/sha256"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestStartCapacityWaitExpiryReleasesTheWorkspaceWithoutPlacing covers a start
// that its caller bounded and its home Runner never placed. While deferred it
// holds the Workspace mutation slot until cancellation or expiry. When the
// caller leaves it pending, expiry must fail the
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
	// Each replay must preserve the first accepted operation and deadline.
	if _, replayed, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, key, stopped.Revision, request,
	); err != nil || !replayed {
		t.Fatalf("identical bounded start replayed=%t error=%v", replayed, err)
	}
	changed := request
	changed.CapacityWaitMilliseconds = capacityWait + 1
	if repeated, replayed, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, key, stopped.Revision, changed,
	); err != nil || !replayed || repeated.ID != operation.ID || repeated.CapacityWaitDeadline == nil || operation.CapacityWaitDeadline == nil || !repeated.CapacityWaitDeadline.Equal(operation.CapacityWaitDeadline.Truncate(time.Microsecond)) {
		t.Fatalf("start with refreshed wait replayed=%t operation=%+v err=%v", replayed, repeated, err)
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

// TestStartCapacityWaitExpiresWhileImagePreparationIsQueued covers a placeable
// home Runner whose execution image never finishes preparing. The start must
// still end at its bound rather than wait for the preparation.
func TestStartCapacityWaitExpiresWhileImagePreparationIsQueued(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "capacity-wait-preparing")
	const capacityWait = 1500
	admittedAt := time.Now().UTC()
	operation, _, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, "capacity-wait-preparing-"+sandboxID, stopped.Revision,
		contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: capacityWait},
	)
	if err != nil {
		t.Fatal(err)
	}
	for passes := 0; ; passes++ {
		if passes == deferralPasses {
			t.Fatalf("bounded start was still pending after %d reconciliation passes", passes)
		}
		fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
		current, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != contracts.OperationStatePending {
			operation = current
			break
		}
	}
	requireExpiredStartReleased(t, fixture, sandboxID, operation, admittedAt.Add(capacityWait*time.Millisecond))
}

// TestStartCapacityWaitExpiresWhenThePlacementPassCrossesTheDeadline covers a
// pass that passes the lifecycle deadline check and reaches placement after
// the deadline. The scheduler refuses the placement under the Sandbox lock,
// and lifecycle must expire the start, not leave it pending.
func TestStartCapacityWaitExpiresWhenThePlacementPassCrossesTheDeadline(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "capacity-wait-crossing")
	const capacityWait = 1500
	admittedAt := time.Now().UTC()
	operation, _, err := fixture.controlPlane.StartSandboxOperation(
		t.Context(), fixture.principal, sandboxID, "capacity-wait-crossing-"+sandboxID, stopped.Revision,
		contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: capacityWait},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The first pass queues the image preparation, which then completes.
	fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
	if !completeTestImagePreparation(t, fixture.pool, sandboxID, time.Now().UTC()) {
		t.Fatal("bounded start queued no image preparation")
	}
	deadline := admittedAt.Add(capacityWait * time.Millisecond)
	time.Sleep(time.Until(deadline.Add(200 * time.Millisecond)))
	// The pass observes a time before the deadline; placement reads the real
	// clock, which is already past it.
	fixture.reconcileWithoutPreparation(t, sandboxID, deadline.Add(-100*time.Millisecond))
	current, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireExpiredStartReleased(t, fixture, sandboxID, current, deadline)
}

// reconcileWithoutPreparation runs one lifecycle pass at now once the Sandbox
// is due, leaving any queued image preparation as it is.
func (fixture *teardownFixture) reconcileWithoutPreparation(t *testing.T, sandboxID string, now time.Time) {
	t.Helper()
	fixture.deferOtherSandboxes(t, sandboxID)
	dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID)
	if !scheduled {
		t.Fatalf("Sandbox %s is not scheduled for reconciliation", sandboxID)
	}
	if wait := time.Until(dueAt); wait > 0 {
		time.Sleep(wait)
	}
	if now.Before(dueAt) {
		if _, err := fixture.pool.Exec(t.Context(), `
			UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id=$1`, sandboxID, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	decision, found, err := fixture.reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != lifecycle.ActionStartInstance {
		t.Fatalf("lifecycle pass for %s = %#v found=%t error=%v", sandboxID, decision, found, err)
	}
}

func requireExpiredStartReleased(
	t *testing.T,
	fixture *teardownFixture,
	sandboxID string,
	operation contracts.Operation,
	deadline time.Time,
) {
	t.Helper()
	if operation.State != contracts.OperationStateFailed || operation.Error == nil ||
		operation.Error.Code != "capacity_wait_expired" {
		t.Fatalf("bounded start Operation = %#v error=%#v, want capacity_wait_expired", operation, operation.Error)
	}
	if operation.CompletedAt == nil || operation.CompletedAt.Before(deadline) {
		t.Fatalf("bounded start failed at %v, before its capacity wait ended at %s", operation.CompletedAt, deadline)
	}
	expired, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.DesiredState != contracts.SandboxDesiredStateStopped {
		t.Fatalf("expired start left the Sandbox wanted %q", expired.DesiredState)
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
	if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
		t.Fatalf("expired start placed %d Assignments, want none", count)
	}
}

func TestStopCancelsStartWaitingForRunnerCapacity(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "capacity-wait-cancel")
	start, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "capacity-wait-cancel-start-"+sandboxID, stopped.Revision, contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	setFixtureRunnerState(t, fixture, "offline")
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	waiting, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.controlPlane.GetSubjectCapacity(t.Context(), fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := fixture.controlPlane.StopSandbox(t.Context(), fixture.principal, sandboxID, "capacity-wait-cancel-stop-"+sandboxID, waiting.Revision)
	if err != nil || stop.State != contracts.OperationStateSucceeded {
		t.Fatalf("stop=%#v error=%v", stop, err)
	}
	cancelled, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, start.ID)
	if err != nil || cancelled.State != contracts.OperationStateCancelled || cancelled.Error == nil || cancelled.Error.Code != "start_cancelled" {
		t.Fatalf("start=%#v error=%v", cancelled, err)
	}
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contracts.SandboxStateStopped || current.DesiredState != contracts.SandboxDesiredStateStopped || current.Generation != waiting.Generation {
		t.Fatalf("cancelled Sandbox=%#v", current)
	}
	var mutationKind, mutationState string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT w.mutation_kind,w.mutation_state FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.workspace_id=w.id WHERE s.id=$1`, sandboxID).Scan(&mutationKind, &mutationState); err != nil {
		t.Fatal(err)
	}
	if mutationKind != "" || mutationState != "" {
		t.Fatalf("cancelled start retains mutation %q/%q", mutationKind, mutationState)
	}
	after, err := fixture.controlPlane.GetSubjectCapacity(t.Context(), fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if after.Usage.ActiveInstances != before.Usage.ActiveInstances-1 || after.Usage.VCPUCount != before.Usage.VCPUCount-current.Resources.VCPUCount {
		t.Fatalf("cancelled usage=%#v before=%#v", after.Usage, before.Usage)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionWait)
	setFixtureRunnerState(t, fixture, "ready")
	if _, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatal("Runner return rescheduled cancelled start")
	}
	if assignments := sandboxAssignmentCount(t, fixture, sandboxID); assignments != 0 {
		t.Fatalf("cancelled start placed %d Assignments", assignments)
	}
}

// The API exposes the operation ID. Its placement deadline is internal,
// so verify the persisted deadline as well as the HTTP replay response.
func TestCapacityWaitHTTPReplayPreservesAdmission(t *testing.T) {
	for _, kind := range []string{"create", "start"} {
		for _, legacy := range []bool{false, true} {
			name := fmt.Sprintf("%s-legacy-%t", kind, legacy)
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				plane, db := newControlPlaneFixture(t, generousQuota())
				admin := fixtureAdmin(t, plane)
				_, subject, credential := createProjectAccountAndCredential(t, plane, admin, "replay-"+name)
				principal := authenticateCredential(t, plane, credential)
				profile := createGrantedProfile(t, plane, db, admin, subject, "replay-"+name)
				spec := profile.CurrentRevision.Spec
				spec.RequireResourceAccount = !legacy
				if kind == "create" {
					spec.Lifecycle.InitialState = "running"
				}
				if _, _, err := plane.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "replay-policy", contracts.ReviseProfileRequest{Spec: spec}, profile.Revision); err != nil {
					t.Fatal(err)
				}
				server := contractServer(t, persistedHTTPHandler(t, plane, db))
				defer server.Close()
				pool, err := pgxpool.New(ctx, integrationDatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				key := "capacity-replay-" + name
				path, revision := "/v1/sandboxes", ""
				body := map[string]any{"capacityWaitMilliseconds": 20000}
				account := contracts.ResourceAccount{Key: "actor:first", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: 1, MaxVCPUCount: contracts.Unlimited, MaxMemoryBytes: contracts.Unlimited}}
				if !legacy {
					body["resourceAccount"] = account
				}
				var legacyCanonical string
				if kind == "create" {
					body["profile"] = profile.Name
					body["metadata"] = map[string]string{}
					legacyCanonical = fmt.Sprintf(`{"profile":%q,"metadata":{},"capacityWaitMilliseconds":20000}`, profile.Name)
				} else {
					stopped, _, err := plane.CreateSandbox(ctx, principal, "stopped-"+name, contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
					if err != nil {
						t.Fatal(err)
					}
					completeFixtureSandboxCreation(t, stopped.ID)
					stopped, err = plane.GetSandbox(ctx, principal, stopped.ID)
					if err != nil {
						t.Fatal(err)
					}
					path += "/" + stopped.ID + ":start"
					revision = strconv.FormatInt(stopped.Revision, 10)
					legacyCanonical = `{"kind":"start","metadata":{},"capacityWaitMilliseconds":20000}`
				}
				post := func() *http.Response {
					return lifecycleHTTPRequest(t, server.URL, credential, http.MethodPost, path, key, revision, "", body)
				}
				accepted := func(replayed bool) contracts.Operation {
					r := post()
					if r.StatusCode != http.StatusAccepted || r.Header.Get("Idempotency-Replayed") != strconv.FormatBool(replayed) {
						t.Fatalf("expected replay=%t, status=%d body=%s", replayed, r.StatusCode, readResponse(t, r))
					}
					var op contracts.Operation
					decodeResponseJSON(t, r, &op)
					return op
				}
				first := accepted(false)
				deadline := func() time.Time {
					var value time.Time
					if err := pool.QueryRow(ctx, `SELECT capacity_wait_deadline FROM secondbox.operations WHERE id=$1`, first.ID).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				original := deadline()
				if !original.Equal(first.CreatedAt.Truncate(time.Microsecond).Add(20 * time.Second)) {
					t.Fatalf("original deadline %s for operation created %s", original, first.CreatedAt)
				}
				if legacy {
					// The release before wait-independent hashing stored this SHA only.
					hash := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyCanonical)))
					result, err := pool.Exec(ctx, `UPDATE secondbox.idempotency_records SET request_hash=$1 WHERE tenant_ref=$2 AND subject_ref=$3 AND idempotency_key=$4`, hash, principal.TenantRef, principal.SubjectRef, key)
					if err != nil || result.RowsAffected() != 1 {
						t.Fatalf("seed pre-upgrade identity: %v %v", result, err)
					}
				}
				for _, wait := range []int{10000, 45000, 0} {
					if wait == 0 {
						delete(body, "capacityWaitMilliseconds")
					} else {
						body["capacityWaitMilliseconds"] = wait
					}
					if !legacy {
						account.Limits.MaxActiveInstances = 0
						body["resourceAccount"] = account
					}
					again := accepted(true)
					if again.ID != first.ID || !deadline().Equal(original) {
						t.Fatalf("wait %d replaced admitted operation or deadline", wait)
					}
				}
				body["image"] = testExecutionImage()
				assertProblem(t, post(), http.StatusConflict, "idempotency_conflict")
				delete(body, "image")
				account.Key = "actor:other"
				body["resourceAccount"] = account
				assertProblem(t, post(), http.StatusConflict, "idempotency_conflict")
				if !legacy {
					usage, err := plane.GetResourceAccountUsage(ctx, principal, "actor:first")
					if err != nil || usage.ActiveInstances != 1 || usage.Allocations[0].SandboxID != first.SandboxID {
						t.Fatalf("replay charge=%+v err=%v", usage, err)
					}
				}
			})
		}
	}
}
