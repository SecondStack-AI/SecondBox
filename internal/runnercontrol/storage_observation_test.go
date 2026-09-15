package runnercontrol

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestWorkspaceExclusiveStorageValidation(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	allocated, exclusive, overflow := uint64(4096), uint64(0), uint64(math.MaxInt64)+1
	for _, test := range []struct {
		name      string
		allocated *uint64
		exclusive *uint64
		reason    string
		valid     bool
	}{
		{name: "zero", allocated: &allocated, exclusive: &exclusive, valid: true},
		{name: "older observation", allocated: &allocated, valid: true},
		{name: "unsupported", allocated: &allocated, reason: "fiemap_unsupported", valid: true},
		{name: "cap", allocated: &allocated, reason: "exclusive_extent_limit", valid: true},
		{name: "unstable", allocated: &allocated, reason: "exclusive_extents_unstable", valid: true},
		{name: "encoded", allocated: &allocated, reason: "exclusive_extents_encoded", valid: true},
		{name: "probe failed", allocated: &allocated, reason: "exclusive_probe_failed", valid: true},
		{name: "unknown reason", allocated: &allocated, reason: "unknown"},
		{name: "value and reason", allocated: &allocated, exclusive: &exclusive, reason: "fiemap_unsupported"},
		{name: "overflow", allocated: &allocated, exclusive: &overflow},
		{name: "exclusive without allocated", exclusive: &exclusive},
		{name: "reason without allocated", reason: "fiemap_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := store.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			item := &runnerv1.WorkspaceStorageObservation{WorkspaceId: "validation-workspace", Generation: 1, ObservedAtUnixMs: uint64(now.UnixMilli()), AllocatedBytes: test.allocated, ExclusiveBytes: test.exclusive, ExclusiveReason: test.reason}
			if test.allocated == nil {
				item.UnavailableReason = "missing"
			}
			err = persistWorkspaceStorageObservations(t.Context(), tx, &runnerv1.RunnerHeartbeat{RunnerId: "validation-runner", WorkspaceStorage: []*runnerv1.WorkspaceStorageObservation{item}}, now)
			if rollbackErr := tx.Rollback(t.Context()); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

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
	exclusive := uint64(0)
	heartbeat := &runnerv1.RunnerHeartbeat{RunnerId: "runner-home", ConnectionId: "storage-connection", MessageId: "storage-heartbeat-1", Sequence: 1, Allocatable: &runnerv1.Capacity{}, Reserved: &runnerv1.Capacity{}, DrainPhase: runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE, StartupTiming: &runnerv1.StartupTiming{},
		WorkspaceStorage: []*runnerv1.WorkspaceStorageObservation{
			{WorkspaceId: "observed-workspace", Generation: 1, ObservedAtUnixMs: uint64(now.UnixMilli()), AllocatedBytes: &allocated, ExclusiveBytes: &exclusive},
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
	if observation.Status != "available" || observation.AllocatedBytes == nil || *observation.AllocatedBytes != 4096 || observation.ExclusiveBytes == nil || *observation.ExclusiveBytes != 0 || !observation.ObservedAt.Equal(now) {
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
	heartbeat.MessageId, heartbeat.Sequence = "storage-heartbeat-3", 3
	heartbeat.WorkspaceStorage = []*runnerv1.WorkspaceStorageObservation{{WorkspaceId: "observed-workspace", Generation: 1, ObservedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()), AllocatedBytes: &allocated, ExclusiveReason: "fiemap_unsupported"}}
	if _, err := store.RecordHeartbeat(t.Context(), heartbeat, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	observation = readObservation()
	if observation.Status != "available" || observation.AllocatedBytes == nil || observation.ExclusiveBytes != nil || observation.ExclusiveReason != "fiemap_unsupported" {
		t.Fatalf("allocated-only measurement = %+v", observation)
	}
}
