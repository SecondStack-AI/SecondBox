package runnercontrol

import (
	"encoding/json"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestHeartbeatWorkspaceStoragePreservesScopeActivityAndObservationTime(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	seedRunnerConnectionForDataPlaneDisconnect(t, store, "storage-connection", "storage-connection", now)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,mutation_state,local_receipt_json,created_at,updated_at)
		VALUES ('observed-workspace','tenant','subject','observed-sandbox','runner-home','ready',1073741824,1,'','','','','','{}',$1,$1), ('other-workspace','other-tenant','other-subject','other-sandbox','other-runner','ready',1073741824,1,'','','','','','{}',$1,$1);
		INSERT INTO secondbox.sandboxes (vcpu_count,memory_bytes,workspace_bytes,id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,revision,created_at,updated_at,last_activity_at)
		VALUES (1,1073741824,1073741824,'observed-sandbox','tenant','subject','profile','revision','stopped','stopped',1,'observed-workspace','','{}','{}',1,$1,$1,$1)`, pgx.QueryExecModeSimpleProtocol, now); err != nil {
		t.Fatal(err)
	}
	allocated := uint64(4096)
	heartbeat := &runnerv1.RunnerHeartbeat{RunnerId: "runner-home", ConnectionId: "storage-connection", MessageId: "storage-heartbeat-1", Sequence: 1, Allocatable: &runnerv1.Capacity{}, Reserved: &runnerv1.Capacity{}, DrainPhase: runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE, StartupTiming: &runnerv1.StartupTiming{},
		WorkspaceStorage: []*runnerv1.WorkspaceStorageObservation{
			{WorkspaceId: "observed-workspace", Generation: 1, ObservedAtUnixMs: uint64(now.UnixMilli()), AllocatedBytes: &allocated},
			{WorkspaceId: "other-workspace", Generation: 1, ObservedAtUnixMs: uint64(now.UnixMilli()), AllocatedBytes: &allocated},
		}, StoragePressure: &runnerv1.StoragePressureObservation{Status: "warning", ObservedAtUnixMs: uint64(now.UnixMilli())}}
	if _, err := store.RecordHeartbeat(t.Context(), heartbeat, now); err != nil {
		t.Fatal(err)
	}
	var other []byte
	if err := store.pool.QueryRow(t.Context(), `SELECT storage_observation_json FROM secondbox.workspaces WHERE id='other-workspace'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Fatal("Runner reported storage for another home")
	}
	readObservation := func() contracts.WorkspaceStorageObservation {
		t.Helper()
		var encoded []byte
		var activity time.Time
		var revision int64
		var instanceID string
		if err := store.pool.QueryRow(t.Context(), `SELECT workspace.storage_observation_json,sandbox.last_activity_at,sandbox.revision,sandbox.current_instance_id FROM secondbox.workspaces AS workspace JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id WHERE workspace.id='observed-workspace'`).Scan(&encoded, &activity, &revision, &instanceID); err != nil {
			t.Fatal(err)
		}
		if !activity.Equal(now) || revision != 1 || instanceID != "" {
			t.Fatal("storage observation changed Sandbox lifecycle")
		}
		var observation contracts.WorkspaceStorageObservation
		if err := json.Unmarshal(encoded, &observation); err != nil {
			t.Fatal(err)
		}
		return observation
	}
	observation := readObservation()
	if observation.Status != "available" || observation.AllocatedBytes == nil || *observation.AllocatedBytes != 4096 || !observation.ObservedAt.Equal(now) {
		t.Fatalf("measurement = %+v", observation)
	}
	heartbeat.MessageId, heartbeat.Sequence = "storage-heartbeat-2", 2
	heartbeat.WorkspaceStorage, heartbeat.StoragePressure = nil, nil
	if _, err := store.RecordHeartbeat(t.Context(), heartbeat, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !readObservation().ObservedAt.Equal(now) {
		t.Fatal("missing report refreshed stored measurement")
	}
}
