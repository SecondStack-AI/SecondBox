package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestQuotaWouldExceedEverySubjectLimit(t *testing.T) {
	base := contracts.QuotaLimits{
		MaxSandboxes: 10, MaxActiveInstances: 10, MaxVCPUCount: 10,
		MaxMemoryBytes: 10, MaxSnapshots: 10, MaxPortSessions: 10,
		MaxConcurrentOperations: 10,
	}
	tests := map[string]struct {
		usage           quotaUsage
		requestedCPU    int64
		requestedMemory int64
		requestedActive int64
	}{
		"sandboxes":             {usage: quotaUsage{sandboxes: 10}},
		"active instances":      {usage: quotaUsage{activeInstances: 10}, requestedActive: 1},
		"CPU":                   {usage: quotaUsage{vcpuCount: 10}, requestedCPU: 1},
		"memory":                {usage: quotaUsage{memoryBytes: 10}, requestedMemory: 1},
		"snapshots":             {usage: quotaUsage{snapshots: 11}},
		"port sessions":         {usage: quotaUsage{portSessions: 11}},
		"concurrent operations": {usage: quotaUsage{concurrentOperations: 11}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if !quotaWouldExceed(
				base, test.usage, test.requestedCPU, test.requestedMemory, test.requestedActive,
			) {
				t.Fatalf("%s limit was not exceeded", name)
			}
		})
	}
	if quotaWouldExceed(base, quotaUsage{}, 1, 1, 1) {
		t.Fatal("in-limit reservation was rejected")
	}
}

func TestSubjectQuotaUsageCountsComputeForActiveStatesOnly(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	tenantRef, subjectRef := "usage-tenant", "usage-subject"
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES (
			'revision-usage','profile-usage',1,
			'{"resources":{"vcpuCount":1,"memoryBytes":1073741824}}',$1
		);
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_vcpu_count,
			max_memory_bytes,max_snapshots,
			max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($2,$3,100,20,80000,171798691840,1,1,1,$1)`,
		pgx.QueryExecModeSimpleProtocol,
		now, tenantRef, subjectRef,
	); err != nil {
		t.Fatal(err)
	}
	for index, state := range []string{"ready", "stopping", "stopped", "deleted"} {
		sandboxID := fmt.Sprintf("sbx-usage-%s", state)
		workspaceID := fmt.Sprintf("workspace-usage-%s", state)
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			INSERT INTO secondbox.workspaces (
				id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
				logical_capacity_bytes,generation,mutation_kind,mutation_id,
				mutation_effect_id,mutation_operation_id,mutation_state,
				local_receipt_json,created_at,updated_at
			) VALUES ($1,$4,$5,$2,'runner-usage','ready',1073741824,1,'','','','','','{}',$3,$3)`,
			workspaceID, sandboxID, now, tenantRef, subjectRef,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			INSERT INTO secondbox.sandboxes (
				id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,
				desired_state,generation,workspace_id,current_instance_id,
				metadata_json,compatibility_summary_json,revision,created_at,updated_at
			) VALUES ($1,$4,$5,'profile-usage','revision-usage',$6,'stopped',1,$2,'','{}','{}',$7,$3,$3)`,
			sandboxID, workspaceID, now, tenantRef, subjectRef, state, int64(index+1),
		); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := controlPlaneStore.GetSubjectUsage(t.Context(), tenantRef, subjectRef)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage.Sandboxes != 3 ||
		usage.Usage.ActiveInstances != 2 ||
		usage.Usage.VCPUCount != 2 ||
		usage.Usage.MemoryBytes != 2*1073741824 {
		t.Fatalf(
			"subject usage sandboxes=%d active=%d cpu=%d memory=%d, want 3/2/2/2147483648",
			usage.Usage.Sandboxes, usage.Usage.ActiveInstances,
			usage.Usage.VCPUCount, usage.Usage.MemoryBytes,
		)
	}
}
