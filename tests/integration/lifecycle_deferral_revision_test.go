package integration_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// deferralPasses is the number of deferral commits the stability assertions
// hold a Sandbox through. A start the reconciler cannot place pays one of these
// per poll interval for as long as the condition lasts, which is unbounded: an
// invalid Profile waits for an operator and an absent home Runner waits for a
// Runner.
const deferralPasses = 12

// TestInvalidProfileStartFailsBeforeAssignmentAndRequiresExplicitRetry covers a
// Sandbox whose pinned Profile revision cannot be resolved into an assignment.
// Durable incompatibility is terminal: it releases the Workspace mutation,
// fails the Operation before compute exists, and parks reconciliation until an
// operator repairs the Profile and explicitly retries.
func TestInvalidProfileStartFailsBeforeAssignmentAndRequiresExplicitRetry(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := createStoppedSandboxWantedRunning(t, fixture, "invalid-profile")
	repairProfile := breakPinnedProfileNetworkMode(t, fixture, sandboxID)
	t.Cleanup(func() { repairProfile(t) })

	beforeRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	failed, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != contracts.SandboxStateFailed || failed.Revision != beforeRevision+1 {
		t.Fatalf("invalid Profile terminal Sandbox = %#v, prior revision %d", failed, beforeRevision)
	}
	if _, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		t.Fatal("invalid Profile terminal failure remained scheduled")
	}
	if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
		t.Fatalf("invalid Profile failure placed %d Assignments, want none", count)
	}
	var mutationState, operationState, operationError string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT workspace.mutation_state,operation.state,operation.error_code
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.operations AS operation ON operation.sandbox_id=sandbox.id
		WHERE sandbox.id=$1 AND operation.kind IN ('create','start')
		ORDER BY operation.created_at DESC,operation.id DESC LIMIT 1`, sandboxID,
	).Scan(&mutationState, &operationState, &operationError); err != nil {
		t.Fatal(err)
	}
	if mutationState != "" || operationState != contracts.OperationStateFailed ||
		operationError != "profile_unavailable" {
		t.Fatalf(
			"invalid Profile terminal mutation=%q operation=%q/%q",
			mutationState, operationState, operationError,
		)
	}

	repairProfile(t)
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID,
		"invalid-profile-explicit-retry-"+sandboxID, failed.Revision,
	); err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateStarting {
		t.Fatalf("repaired Profile left the Sandbox %q, want starting", started.State)
	}
	// Placement is an observable change, and the distinction from a deferral is
	// the whole point.
	if started.Revision <= failed.Revision {
		t.Fatalf(
			"explicit retry placement left revision at %d, want above failed %d",
			started.Revision, failed.Revision,
		)
	}
}

// TestUnavailableHomeRunnerStartDeferralHoldsThePublicRevision covers the other
// deferral of the same class: a Sandbox wanted running whose home Runner is not
// placeable. A running Sandbox never relocates, so this one waits for its own
// Runner to come back rather than for an operator, and it must wait without
// moving anything a caller reads.
func TestUnavailableHomeRunnerStartDeferralHoldsThePublicRevision(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID := createStoppedSandboxWantedRunning(t, fixture, "unavailable-home-runner")
	setFixtureRunnerState(t, fixture, "offline")

	deferredRevision, deferredETag := quiescenceSandboxETag(t, fixture, sandboxID)
	deferredUpdatedAt := sandboxUpdatedAt(t, fixture, sandboxID)
	for index := 0; index < deferralPasses; index++ {
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
		revision, etag := quiescenceSandboxETag(t, fixture, sandboxID)
		if revision != deferredRevision || etag != deferredETag {
			t.Fatalf(
				"unavailable home Runner deferral %d moved the public revision from %d ETag %q to %d ETag %q",
				index, deferredRevision, deferredETag, revision, etag,
			)
		}
		if _, scheduled := fixture.sandboxDueAt(t, sandboxID); !scheduled {
			t.Fatalf("unavailable home Runner deferral %d left the Sandbox unscheduled", index)
		}
	}
	if updatedAt := sandboxUpdatedAt(t, fixture, sandboxID); !updatedAt.Equal(deferredUpdatedAt) {
		t.Fatalf(
			"unavailable home Runner deferrals moved updated_at from %s to %s",
			deferredUpdatedAt, updatedAt,
		)
	}
	if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
		t.Fatalf("unavailable home Runner deferrals placed %d Assignments, want none", count)
	}

	setFixtureRunnerState(t, fixture, "ready")
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	started, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != contracts.SandboxStateStarting {
		t.Fatalf("returned home Runner left the Sandbox %q, want starting", started.State)
	}
	if started.Revision <= deferredRevision {
		t.Fatalf(
			"placement left the public revision at %d, want above the held %d",
			started.Revision, deferredRevision,
		)
	}
}

// createStoppedSandboxWantedRunning leaves one Sandbox in the state both
// deferrals act on: stopped, holding no Instance, and wanted running.
func createStoppedSandboxWantedRunning(
	t *testing.T,
	fixture *teardownFixture,
	label string,
) string {
	t.Helper()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		"deferral-create-"+label+"-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
		contracts.CreateSandboxRequest{
			Profile:  fixture.profileName,
			Metadata: map[string]string{"fixture": "lifecycle-deferral"},
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
	if current.State != contracts.SandboxStateStopped {
		t.Fatalf("created Sandbox state = %q, want stopped", current.State)
	}
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID,
		"deferral-start-"+label+"-"+sandboxID, current.Revision,
	); err != nil {
		t.Fatal(err)
	}
	return sandboxID
}

// breakPinnedProfileNetworkMode corrupts the durable Profile revision the
// Sandbox is pinned to, the way an operator-authored policy the control plane
// cannot resolve reaches the reconciler, and returns the repair.
func breakPinnedProfileNetworkMode(
	t *testing.T,
	fixture *teardownFixture,
	sandboxID string,
) func(*testing.T) {
	t.Helper()
	var profileRevisionID string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT profile_revision_id FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&profileRevisionID); err != nil {
		t.Fatal(err)
	}
	var networkMode string
	if err := fixture.pool.QueryRow(t.Context(), `
		UPDATE secondbox.profile_revisions
		SET spec_json=jsonb_set(spec_json,'{network,mode}','"unresolvable"')
		WHERE id=$1
		RETURNING spec_json->'network'->>'mode'`, profileRevisionID,
	).Scan(&networkMode); err != nil {
		t.Fatal(err)
	}
	if networkMode != "unresolvable" {
		t.Fatalf("pinned Profile network mode = %q, want the corrupted value", networkMode)
	}
	return func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := fixture.pool.Exec(ctx, `
			UPDATE secondbox.profile_revisions
			SET spec_json=jsonb_set(spec_json,'{network,mode}','"deny_all"')
			WHERE id=$1`, profileRevisionID,
		); err != nil {
			t.Fatal(err)
		}
	}
}

// setFixtureRunnerState moves the fixture's home Runner in and out of the
// placement candidate set the scheduler locks.
func setFixtureRunnerState(t *testing.T, fixture *teardownFixture, state string) {
	t.Helper()
	tag, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET state=$2,updated_at=$3 WHERE id=$1`,
		fixture.runnerID, state, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("Runner %s state update changed %d rows", fixture.runnerID, tag.RowsAffected())
	}
}

func sandboxUpdatedAt(t *testing.T, fixture *teardownFixture, sandboxID string) time.Time {
	t.Helper()
	var updatedAt time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT updated_at FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	return updatedAt
}

func sandboxAssignmentCount(t *testing.T, fixture *teardownFixture, sandboxID string) int64 {
	t.Helper()
	var count int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.assignments WHERE sandbox_id=$1`, sandboxID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
