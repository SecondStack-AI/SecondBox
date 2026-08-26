package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestQuotaWouldExceedEverySubjectLimit(t *testing.T) {
	base := contracts.QuotaLimits{
		MaxSandboxes: 10, MaxActiveInstances: 10, MaxCPUMillis: 10,
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
		"CPU":                   {usage: quotaUsage{cpuMillis: 10}, requestedCPU: 1},
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

func TestTenantQuotaWouldExceedEveryDataPlaneLimit(t *testing.T) {
	base := contracts.TenantQuota{
		MaxSandboxes: 10, MaxActiveInstances: 10, MaxCPUMillis: 10,
		MaxMemoryBytes: 10, MaxSnapshots: 10, MaxPortSessions: 10,
		MaxConcurrentOperations: 10,
	}
	tests := map[string]struct {
		usage contracts.TenantQuotaUsage
		delta quotaUsage
	}{
		"sandboxes":             {usage: contracts.TenantQuotaUsage{Sandboxes: 10}, delta: quotaUsage{sandboxes: 1}},
		"active instances":      {usage: contracts.TenantQuotaUsage{ActiveInstances: 10}, delta: quotaUsage{activeInstances: 1}},
		"CPU":                   {usage: contracts.TenantQuotaUsage{CPUMillis: 10}, delta: quotaUsage{cpuMillis: 1}},
		"memory":                {usage: contracts.TenantQuotaUsage{MemoryBytes: 10}, delta: quotaUsage{memoryBytes: 1}},
		"snapshots":             {usage: contracts.TenantQuotaUsage{Snapshots: 10}, delta: quotaUsage{snapshots: 1}},
		"port sessions":         {usage: contracts.TenantQuotaUsage{PortSessions: 10}, delta: quotaUsage{portSessions: 1}},
		"concurrent operations": {usage: contracts.TenantQuotaUsage{ConcurrentOperations: 10}, delta: quotaUsage{concurrentOperations: 1}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if !tenantDataPlaneQuotaWouldExceed(base, test.usage, test.delta) {
				t.Fatalf("%s limit was not exceeded", name)
			}
		})
	}
	if tenantDataPlaneQuotaWouldExceed(base, contracts.TenantQuotaUsage{}, quotaUsage{
		sandboxes: 1, activeInstances: 1, cpuMillis: 1, memoryBytes: 1,
		snapshots: 1, portSessions: 1, concurrentOperations: 1,
	}) {
		t.Fatal("in-limit Tenant reservation was rejected")
	}
}

func TestSubjectQuotaCoversCommittedUsageEveryDimension(t *testing.T) {
	quota := contracts.QuotaLimits{
		MaxSandboxes: 10, MaxActiveInstances: 10, MaxCPUMillis: 10,
		MaxMemoryBytes: 10, MaxSnapshots: 10, MaxPortSessions: 10,
		MaxConcurrentOperations: 10,
	}
	tests := map[string]quotaUsage{
		"sandboxes":             {sandboxes: 11},
		"active instances":      {activeInstances: 11},
		"CPU":                   {cpuMillis: 11},
		"memory":                {memoryBytes: 11},
		"snapshots":             {snapshots: 11},
		"port sessions":         {portSessions: 11},
		"concurrent operations": {concurrentOperations: 11},
	}
	for name, usage := range tests {
		t.Run(name, func(t *testing.T) {
			if subjectQuotaCoversUsage(quota, usage) {
				t.Fatalf("%s narrowing was accepted", name)
			}
		})
	}
	if !subjectQuotaCoversUsage(quota, quotaUsage{
		sandboxes: 10, activeInstances: 10, cpuMillis: 10, memoryBytes: 10,
		snapshots: 10, portSessions: 10, concurrentOperations: 10,
	}) {
		t.Fatal("quota equal to committed usage was rejected")
	}
}

func TestManagedTenantRecordQuotaIsAtomicIsolatedAndReleases(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	newTenant := func(ref string) contracts.Tenant {
		tenant := managementTestTenant(ref, now)
		tenant.AggregateQuota.MaxActiveSubjects = 1
		tenant.AggregateQuota.MaxApplicationAuthorities = 1
		return tenant
	}
	for _, ref := range []string{"record-quota-a", "record-quota-b"} {
		if _, err := controlPlaneStore.CreateTenant(t.Context(), newTenant(ref)); err != nil {
			t.Fatal(err)
		}
	}

	createSubjects := func(tenantRef string, count int) ([]contracts.Subject, []error) {
		start := make(chan struct{})
		results := make(chan contracts.Subject, count)
		failures := make(chan error, count)
		var wait sync.WaitGroup
		for index := range count {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				subject := managementTestSubject(tenantRef, fmt.Sprintf("subject-%d", index), now)
				created, _, err := controlPlaneStore.CreateManagedSubject(t.Context(), subject, ports.AdminIdempotencyInput{
					TenantRef: tenantRef, Operation: "subject.create", TargetID: subject.Ref,
					Key: "subject-key-" + subject.Ref, RequestHash: "subject-hash-" + subject.Ref,
					Now: now, Ends: now.Add(time.Hour), AuditEvent: adminTestAudit("subject-key-"+subject.Ref, now),
				})
				if err != nil {
					failures <- err
					return
				}
				results <- created
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(failures)
		var created []contracts.Subject
		for subject := range results {
			created = append(created, subject)
		}
		var errs []error
		for err := range failures {
			errs = append(errs, err)
		}
		return created, errs
	}

	createdA, failuresA := createSubjects("record-quota-a", 8)
	if len(createdA) != 1 || len(failuresA) != 7 {
		t.Fatalf("Tenant A Subject admission successes=%d failures=%d", len(createdA), len(failuresA))
	}
	for _, err := range failuresA {
		if !errors.Is(err, ports.ErrQuotaExceeded) {
			t.Fatalf("Tenant A Subject quota error = %v", err)
		}
	}
	createdB, failuresB := createSubjects("record-quota-b", 1)
	if len(createdB) != 1 || len(failuresB) != 0 {
		t.Fatalf("Tenant B Subject admission successes=%d failures=%v", len(createdB), failuresB)
	}

	applicationExpiry := now.Add(30 * time.Minute)
	start := make(chan struct{})
	applications := make(chan contracts.ApplicationCredentialResponse, 8)
	applicationFailures := make(chan error, 8)
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			authority := contracts.ApplicationAuthority{
				ID: fmt.Sprintf("record-authority-%d", index), TenantRef: "record-quota-a",
				SubjectRef: createdA[0].Ref, State: contracts.AuthorityStateActive,
				Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
				Metadata: map[string]string{}, ExpiresAt: &applicationExpiry,
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			created, _, err := controlPlaneStore.CreateManagedApplicationAuthority(t.Context(), authority, ports.AdminIdempotencyInput{
				TenantRef: authority.TenantRef, SubjectRef: authority.SubjectRef,
				Operation: "application_authority.create", TargetID: authority.ID,
				Key: "authority-key-" + authority.ID, RequestHash: "authority-hash-" + authority.ID,
				Now: now, Ends: now.Add(time.Hour), AuditEvent: adminTestAudit("authority-key-"+authority.ID, now),
			})
			if err != nil {
				applicationFailures <- err
				return
			}
			applications <- created
		}()
	}
	close(start)
	wait.Wait()
	close(applications)
	close(applicationFailures)
	var admitted []contracts.ApplicationCredentialResponse
	for application := range applications {
		admitted = append(admitted, application)
	}
	var authorityQuotaFailures int
	for err := range applicationFailures {
		if !errors.Is(err, ports.ErrQuotaExceeded) {
			t.Fatalf("ApplicationAuthority quota error = %v", err)
		}
		authorityQuotaFailures++
	}
	if len(admitted) != 1 || authorityQuotaFailures != 7 {
		t.Fatalf("ApplicationAuthority admissions=%d quota failures=%d", len(admitted), authorityQuotaFailures)
	}
	if _, _, err := controlPlaneStore.RevokeManagedApplicationAuthority(
		t.Context(), "record-quota-a", admitted[0].Authority.ID, admitted[0].Authority.Revision,
		now.Add(time.Minute), ports.AdminIdempotencyInput{
			TenantRef: "record-quota-a", SubjectRef: createdA[0].Ref,
			Operation: "application_authority.revoke", TargetID: admitted[0].Authority.ID,
			Key: "authority-release", RequestHash: "authority-release-hash",
			Now: now.Add(time.Minute), Ends: now.Add(time.Hour), AuditEvent: adminTestAudit("authority-release", now.Add(time.Minute)),
		},
	); err != nil {
		t.Fatal(err)
	}
	replacement := contracts.ApplicationAuthority{
		ID: "record-authority-replacement", TenantRef: "record-quota-a",
		SubjectRef: createdA[0].Ref, State: contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, ExpiresAt: &applicationExpiry,
		Revision: 1, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute),
	}
	if _, _, err := controlPlaneStore.CreateManagedApplicationAuthority(t.Context(), replacement, ports.AdminIdempotencyInput{
		TenantRef: replacement.TenantRef, SubjectRef: replacement.SubjectRef,
		Operation: "application_authority.create", TargetID: replacement.ID,
		Key: "authority-replacement", RequestHash: "authority-replacement-hash",
		Now: replacement.CreatedAt, Ends: now.Add(time.Hour), AuditEvent: adminTestAudit("authority-replacement", replacement.CreatedAt),
	}); err != nil {
		t.Fatalf("ApplicationAuthority admission after release: %v", err)
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
			'{"resources":{"cpuMillis":1000,"memoryBytes":1073741824}}',$1
		);
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_cpu_millis,
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
		usage.Usage.CPUMillis != 2000 ||
		usage.Usage.MemoryBytes != 2*1073741824 {
		t.Fatalf(
			"subject usage sandboxes=%d active=%d cpu=%d memory=%d, want 3/2/2000/2147483648",
			usage.Usage.Sandboxes, usage.Usage.ActiveInstances,
			usage.Usage.CPUMillis, usage.Usage.MemoryBytes,
		)
	}
}
