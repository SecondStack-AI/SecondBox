package store

import (
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestPostgresFinishStopAdvancesGenerationAndFencesActivity(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,generation,retained_bytes,
			current_checkpoint_id,created_at,updated_at
		) VALUES ('wrk_store_stop','tenant','subject','sbx_store_stop',3,0,'',$1,$1)`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sbx_store_stop','tenant','subject','profile','revision','stopping','stopped',
			3,'wrk_store_stop','ins_store_stop','{}','{}','store-worker',$2,7,$1,$1
		)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.leases (
			id,tenant_ref,subject_ref,sandbox_id,generation,state,
			expires_at,revision,created_at,updated_at
		) VALUES ('lease_store_stop','tenant','subject','sbx_store_stop',3,'active',$2,1,$1,$1)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,
			last_activity_at,created_at,updated_at
		) VALUES ('activity_store_stop','tenant','subject','sbx_store_stop',3,'terminal','active',
		          'lease_store_stop',$1,$1,$1)`, now,
	); err != nil {
		t.Fatal(err)
	}
	claim := ports.LifecycleReconcileClaim{
		SandboxID: "sbx_store_stop", ObservedState: contracts.SandboxStateStopping,
		DesiredState: contracts.SandboxDesiredStateStopped,
		WorkerID:     "store-worker", Revision: 7,
	}
	if err := controlPlaneStore.ApplyLifecycleAction(
		t.Context(), claim, "finish_stop", contracts.TerminationReasonRequestedStop,
		now, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var sandboxGeneration, workspaceGeneration int64
	var sandboxState, leaseState, activityState string
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT sandbox.generation,workspace.generation,sandbox.state,lease.state,activity.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.leases AS lease ON lease.sandbox_id=sandbox.id
		JOIN secondbox.activity_sessions AS activity ON activity.sandbox_id=sandbox.id
		WHERE sandbox.id='sbx_store_stop'`,
	).Scan(
		&sandboxGeneration, &workspaceGeneration, &sandboxState, &leaseState, &activityState,
	); err != nil {
		t.Fatal(err)
	}
	if sandboxGeneration != 4 || workspaceGeneration != 4 ||
		sandboxState != contracts.SandboxStateStopped ||
		leaseState != contracts.LeaseStateFenced || activityState != "closed" {
		t.Fatalf(
			"finish_stop generations=%d/%d states=%s/%s/%s",
			sandboxGeneration, workspaceGeneration, sandboxState, leaseState, activityState,
		)
	}
}
