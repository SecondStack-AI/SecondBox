package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// quiescencePollIntervals is the number of recovery poll intervals the
// stability assertions hold a Sandbox through. Anything the reconciler would do
// on a bounded poll has had every one of them to do it.
const quiescencePollIntervals = 12

// TestStoppedSandboxParksItsLifecycleSchedule covers the steady state a stopped
// Sandbox reaches: the finish-stop commit leaves no durable deadline, the claim
// scan therefore never selects it again, and its public revision and ETag are
// byte-stable for as long as nothing external happens to it.
func TestStoppedSandboxParksItsLifecycleSchedule(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := stopSandboxToRest(t, fixture)

	if dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatalf(
			"finish-stop left Sandbox %s scheduled at %s; a Sandbox at rest carries no deadline",
			sandboxID, dueAt,
		)
	}
	restRevision, restETag := quiescenceSandboxETag(t, fixture, sandboxID)

	fixture.deferOtherSandboxes(t, sandboxID)
	for index := 0; index < quiescencePollIntervals; index++ {
		time.Sleep(teardownPollInterval)
		decision, found, err := fixture.reconciler.RunOnce(
			t.Context(), time.Now().UTC(), ports.LifecycleWakeTriggerDeadline,
		)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatalf(
				"claim scan selected a Sandbox at rest on poll %d: decision %#v",
				index, decision,
			)
		}
	}

	revision, etag := quiescenceSandboxETag(t, fixture, sandboxID)
	if revision != restRevision || etag != restETag {
		t.Fatalf(
			"Sandbox at rest moved from revision %d ETag %q to revision %d ETag %q across %d poll intervals",
			restRevision, restETag, revision, etag, quiescencePollIntervals,
		)
	}
}

// TestStoppedSandboxIfMatchSurvivesBackgroundReconciliation is the ETag hazard
// regression. A caller reads a stopped Sandbox, does something else for several
// poll intervals, and then sends If-Match on what it read. The precondition
// must hold: nothing a caller can observe changed in the meantime.
func TestStoppedSandboxIfMatchSurvivesBackgroundReconciliation(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := stopSandboxToRest(t, fixture)
	observedRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)

	fixture.deferOtherSandboxes(t, sandboxID)
	for index := 0; index < quiescencePollIntervals; index++ {
		time.Sleep(teardownPollInterval)
		if _, _, err := fixture.reconciler.RunOnce(
			t.Context(), time.Now().UTC(), ports.LifecycleWakeTriggerDeadline,
		); err != nil {
			t.Fatal(err)
		}
	}

	response := quiescenceHTTPRequest(
		t, fixture, http.MethodDelete, "/v1/sandboxes/"+sandboxID,
		"quiescence-stale-if-match-delete", observedRevision, nil,
	)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf(
			"delete with a revision read %d poll intervals earlier status=%d body=%s",
			quiescencePollIntervals, response.StatusCode, readResponse(t, response),
		)
	}
	response.Body.Close()

	// The intent is also the wake path: it schedules the Sandbox in the same
	// transaction that records the desired state, so the delete completes.
	if !fixture.sandboxIsDue(t, sandboxID) {
		t.Fatal("delete intent left the Sandbox at rest instead of scheduling it")
	}
	driveStoppedDeleteToCompletion(t, fixture, sandboxID)
}

// TestParkedSandboxWakesOnStartIntent covers the second lifecycle intent wake
// path: a Sandbox at rest that an application wants running is scheduled by the
// intent commit and reaches ready without waiting on a recovery poll.
func TestParkedSandboxWakesOnStartIntent(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := stopSandboxToRest(t, fixture)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID,
		"quiescence-start-intent", current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if !fixture.sandboxIsDue(t, sandboxID) {
		t.Fatal("start intent left the Sandbox at rest instead of scheduling it")
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	fixture.completeAssignmentReady(t, sandboxID)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateReady {
		t.Fatalf("started Sandbox state = %q, want ready", started.State)
	}
}

// TestParkedSandboxAdmitsSnapshotCreateWithoutSchedulingReconciliation records
// the other half of the wake enumeration. A Snapshot changes no input to the
// lifecycle decision for a Sandbox at rest, so it needs no reconciliation pass
// and must not schedule one — while still admitting under the revision the
// caller read from the parked Sandbox.
func TestParkedSandboxAdmitsSnapshotCreateWithoutSchedulingReconciliation(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := stopSandboxToRest(t, fixture)
	restRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)

	response := quiescenceHTTPRequest(
		t, fixture, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/snapshots",
		"quiescence-snapshot-create", restRevision,
		contracts.CreateSnapshotRequest{Name: "at-rest", Metadata: map[string]string{}},
	)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf(
			"Snapshot create on a Sandbox at rest status=%d body=%s",
			response.StatusCode, readResponse(t, response),
		)
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	if operation.Snapshot == nil || operation.Snapshot.State != "creating" {
		t.Fatalf("Snapshot create Operation = %#v", operation)
	}
	if dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatalf(
			"Snapshot create scheduled reconciliation at %s for a Sandbox at rest",
			dueAt,
		)
	}
	// The Snapshot admission is a Sandbox mutation, so it does move the public
	// revision. That is an observable change, and the distinction from a wait
	// commit is the whole point.
	snapshotRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)
	if snapshotRevision <= restRevision {
		t.Fatalf(
			"Snapshot create left the public revision at %d, want above %d",
			snapshotRevision, restRevision,
		)
	}
}

// TestTransitionalWaitHoldsThePublicRevision covers a Sandbox that genuinely
// belongs on the recovery poll: it is starting, and the reconciler must keep
// looking until the Runner acknowledges. Every one of those passes decides to
// wait, and a wait changes nothing a caller can observe, so the public revision
// and ETag must not move across any of them.
func TestTransitionalWaitHoldsThePublicRevision(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, _ := startSandboxToStarting(t, fixture)
	startingRevision, startingETag := quiescenceSandboxETag(t, fixture, sandboxID)
	var startingUpdatedAt time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT updated_at FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&startingUpdatedAt); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < quiescencePollIntervals; index++ {
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionWait)
		revision, etag := quiescenceSandboxETag(t, fixture, sandboxID)
		if revision != startingRevision || etag != startingETag {
			t.Fatalf(
				"wait %d moved the public revision from %d ETag %q to %d ETag %q",
				index, startingRevision, startingETag, revision, etag,
			)
		}
	}
	var updatedAt time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT updated_at FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if !updatedAt.Equal(startingUpdatedAt) {
		t.Fatalf(
			"waits moved updated_at from %s to %s without changing observable state",
			startingUpdatedAt, updatedAt,
		)
	}

	// The Sandbox is still genuinely scheduled: holding the revision must not
	// be confused with parking a Sandbox whose transition is outstanding.
	if _, scheduled := fixture.sandboxDueAt(t, sandboxID); !scheduled {
		t.Fatal("a starting Sandbox left the recovery poll while its Runner had not answered")
	}
}

// TestReadySandboxSleepsToItsIdleDeadlineAndFires covers the remaining
// schedule: a Sandbox carrying a real future deadline sleeps exactly to it,
// neither parking nor paying the recovery poll, and the deadline fires.
func TestReadySandboxSleepsToItsIdleDeadlineAndFires(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, _ := fixture.createReadySandbox(t)

	// The pinned Profile's idle timeout is measured from the last useful
	// activity, so moving that origin backwards puts the real deadline just
	// far enough ahead to be unambiguously later than the recovery poll.
	idleTimeout := time.Duration(testProfileSpec(1).Lifecycle.IdleSeconds) * time.Second
	deadlineDistance := 3 * teardownPollInterval
	idleOrigin := time.Now().UTC().Add(deadlineDistance - idleTimeout)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET last_activity_at=$2 WHERE id=$1`,
		sandboxID, idleOrigin,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.instances SET ready_at=$2
		WHERE id=(SELECT current_instance_id FROM secondbox.sandboxes WHERE id=$1)`,
		sandboxID, idleOrigin,
	); err != nil {
		t.Fatal(err)
	}

	fixture.runLifecycle(t, sandboxID, lifecycle.ActionWait)
	dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID)
	if !scheduled {
		t.Fatal("a ready Sandbox carrying an idle deadline parked instead of sleeping to it")
	}
	if remaining := time.Until(dueAt); remaining <= teardownPollInterval {
		t.Fatalf(
			"ready Sandbox is due in %s, want its idle deadline beyond the %s recovery poll",
			remaining, teardownPollInterval,
		)
	}

	fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
	var terminationReason, desiredState string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT COALESCE(lifecycle_termination_reason,''),desired_state
		FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&terminationReason, &desiredState); err != nil {
		t.Fatal(err)
	}
	if terminationReason != contracts.TerminationReasonIdleTimeout ||
		desiredState != contracts.SandboxDesiredStateStopped {
		t.Fatalf(
			"idle deadline fired with reason %q desired %q, want idle_timeout and stopped",
			terminationReason, desiredState,
		)
	}
}

// stopSandboxToRest drives one Sandbox from create through the full stop path
// and returns it at rest: stopped, wanted stopped, and carrying no schedule.
func stopSandboxToRest(t *testing.T, fixture *teardownFixture) string {
	t.Helper()
	sandboxID, _ := fixture.createReadySandbox(t)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.StopSandbox(
		t.Context(), fixture.principal, sandboxID,
		"quiescence-stop-"+sandboxID, current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStopInstance)
	fixture.completeFence(t, sandboxID)
	fixture.completeGenerationAdvance(t, sandboxID)
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionFinishStop)
	stopped, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != contracts.SandboxStateStopped ||
		stopped.DesiredState != contracts.SandboxDesiredStateStopped {
		t.Fatalf(
			"stopped Sandbox state=%q desired=%q, want stopped and stopped",
			stopped.State, stopped.DesiredState,
		)
	}
	return sandboxID
}

// startSandboxToStarting leaves one Sandbox in the state the reconciler must
// keep polling: started, with the Runner's acknowledgement outstanding.
func startSandboxToStarting(t *testing.T, fixture *teardownFixture) (string, string) {
	t.Helper()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		"quiescence-create-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
		contracts.CreateSandboxRequest{
			Profile:  fixture.profileName,
			Metadata: map[string]string{"fixture": "lifecycle-quiescence"},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox created=%t error=%v", created, err)
	}
	sandboxID := operation.SandboxID
	fixture.completeWorkspaceCreate(t, sandboxID, operation.ID)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	startOperation, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID,
		"quiescence-start-"+sandboxID, current.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	return sandboxID, startOperation.ID
}

// driveStoppedDeleteToCompletion finishes the delete of a Sandbox that was
// already stopped. Such a Sandbox holds no Instance, so the reconciler goes
// straight to the Workspace delete with no drain or stop hop in between.
func driveStoppedDeleteToCompletion(t *testing.T, fixture *teardownFixture, sandboxID string) {
	t.Helper()
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
	fixture.completeWorkspaceDelete(t, sandboxID)
	var state string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != contracts.SandboxStateDeleted {
		t.Fatalf("deleted Sandbox state = %q, want deleted", state)
	}
}

// quiescenceSandboxETag reads the Sandbox over HTTP and returns both the
// revision the body carries and the exact ETag header bytes served with it.
func quiescenceSandboxETag(
	t *testing.T,
	fixture *teardownFixture,
	sandboxID string,
) (int64, string) {
	t.Helper()
	response := quiescenceHTTPRequest(
		t, fixture, http.MethodGet, "/v1/sandboxes/"+sandboxID, "", 0, nil,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET Sandbox status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	etag := response.Header.Get("ETag")
	var sandbox contracts.Sandbox
	decodeResponseJSON(t, response, &sandbox)
	if etag == "" {
		t.Fatalf("Sandbox %s was served without an ETag", sandboxID)
	}
	return sandbox.Revision, etag
}

func quiescenceHTTPRequest(
	t *testing.T,
	fixture *teardownFixture,
	method string,
	path string,
	idempotencyKey string,
	ifMatch int64,
	body any,
) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, fixture.server+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testPlatformToken)
	request.Header.Set("X-SecondBox-Tenant-Ref", fixture.tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", fixture.subjectRef)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if ifMatch > 0 {
		request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(ifMatch, 10)+`"`)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
