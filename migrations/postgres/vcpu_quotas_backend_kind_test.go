package postgresmigrations

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var vcpuConversionLineage = []string{
	"0002_sandbox_name_index.sql",
	"0003_control_plane_wakeups.sql",
	"0004_direct_port_data_plane.sql",
	"0005_relay_data_plane_wakeups.sql",
	"0006_eager_assignment_dispatch.sql",
	"0007_relay_frame_retention.sql",
	"0008_workspace_relocations.sql",
	"0009_remove_frame_relay.sql",
	"0010_lifecycle_hot_path_indexes.sql",
	"0011_session_accounting_retention.sql",
	"0012_proxied_port_transport.sql",
	"0013_lifecycle_fence_command_kind.sql",
	"0014_lifecycle_quiescent_schedule.sql",
	"0015_profile_startup_mode.sql",
	"0016_persisted_authorities.sql",
	"0017_tenant_aggregate_quotas.sql",
	"0018_subject_cleanup.sql",
}

// milliUnitSpec is a Profile revision as recorded before the vCPU conversion;
// vcpuSpec is what a fresh deployment records for the same allowance.
const (
	milliUnitSpec = `{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"cold_boot"},"resources":{"cpuMillis":1500,"processLimit":64,"memoryBytes":1073741824}}`
	vcpuSpec      = `{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"cold_boot"},"resources":{"vcpuCount":2,"memoryBytes":1073741824}}`
)

// TestVCPUConversionConvergesOnFreshAndUpgradedSchemas proves the forward
// migration: a database recorded in CPU milli-units converts to exactly the
// representation a fresh deployment writes, without editing the baseline. The
// migration ledger applies DDL once, so only the guarded JSON rewrites need
// to converge rather than the column rename.
func TestVCPUConversionConvergesOnFreshAndUpgradedSchemas(t *testing.T) {
	upgraded := newGuardDatabase(t)
	applyMigrations(t, upgraded, vcpuConversionLineage[:len(vcpuConversionLineage)-2]...)
	seedTenantLedger(t, upgraded)
	applyMigrations(t, upgraded, "0017_tenant_aggregate_quotas.sql", "0018_subject_cleanup.sql")
	seedProfileRevision(t, upgraded, "revision-milli", 1, milliUnitSpec)
	seedSubjectQuota(t, upgraded, "max_cpu_millis", 1500)
	seedRunnerCapacity(t, upgraded,
		`{"CPUMillis": 8000, "MemoryBytes": 1073741824}`,
		`{"CPUMillis": 2500, "MemoryBytes": 268435456}`,
	)
	applyMigrations(t, upgraded, "0019_vcpu_quotas_and_backend_kind.sql")

	fresh := newGuardDatabase(t)
	applyMigrations(t, fresh, vcpuConversionLineage...)
	applyMigrations(t, fresh, "0019_vcpu_quotas_and_backend_kind.sql")
	seedProfileRevision(t, fresh, "revision-vcpu", 1, vcpuSpec)

	upgradedSpec := profileRevisionSpec(t, upgraded, "revision-milli")
	freshSpec := profileRevisionSpec(t, fresh, "revision-vcpu")
	if upgradedSpec != freshSpec {
		t.Fatalf(
			"upgraded and fresh Profile revision specs differ:\nupgraded: %s\nfresh:    %s",
			upgradedSpec, freshSpec,
		)
	}

	if got := subjectQuotaVCPUs(t, upgraded); got != 2 {
		t.Errorf("converted quota max_vcpu_count = %d, want 2 (1500 milli-units rounded up)", got)
	}

	var capacity, reserved string
	if err := upgraded.QueryRow(t.Context(), `
		SELECT capacity_json::text, reserved_capacity_json::text
		FROM secondbox.runners WHERE id='runner-vcpu'`,
	).Scan(&capacity, &reserved); err != nil {
		t.Fatal(err)
	}
	if capacity != `{"VCPUCount": 8, "MemoryBytes": 1073741824}` {
		t.Errorf("converted capacity document = %s", capacity)
	}
	if reserved != `{"VCPUCount": 3, "MemoryBytes": 268435456}` {
		t.Errorf("converted reserved-capacity document = %s", reserved)
	}

	var capacityPolicy string
	if err := upgraded.QueryRow(t.Context(), `
		SELECT capacity_policy_json::text FROM secondbox.runner_pools WHERE name='pool-vcpu'`,
	).Scan(&capacityPolicy); err != nil {
		t.Fatal(err)
	}
	if capacityPolicy != `{"maxSandboxes": 8, "maxVcpuCount": 16}` {
		t.Errorf("converted pool capacity policy = %s", capacityPolicy)
	}

	var processLimitRetained bool
	if err := upgraded.QueryRow(t.Context(), `
		SELECT spec_json->'resources' ? 'processLimit'
		FROM secondbox.profile_revisions WHERE id='revision-milli'`,
	).Scan(&processLimitRetained); err != nil {
		t.Fatal(err)
	}
	if processLimitRetained {
		t.Error("removed processLimit field survived the conversion")
	}

	var tenantLedgerVCPUs int64
	if err := upgraded.QueryRow(t.Context(), `
		SELECT max_vcpu_count FROM secondbox.tenant_quotas WHERE tenant_ref='tenant-ledger'`,
	).Scan(&tenantLedgerVCPUs); err != nil {
		t.Fatal(err)
	}
	if tenantLedgerVCPUs != 2 {
		t.Errorf("converted tenant ledger max_vcpu_count = %d, want 2 (1500 milli-units rounded up)", tenantLedgerVCPUs)
	}
	var aggregateVCPUs, subjectDocumentVCPUs int64
	var aggregateMilliRetained, subjectMilliRetained bool
	if err := upgraded.QueryRow(t.Context(), `
		SELECT (tenant.aggregate_quota_json->>'maxVcpuCount')::bigint,
		       tenant.aggregate_quota_json ? 'maxCpuMillis',
		       (subject.quota_json->>'maxVcpuCount')::bigint,
		       subject.quota_json ? 'maxCpuMillis'
		FROM secondbox.tenants AS tenant
		JOIN secondbox.subjects AS subject ON subject.tenant_ref=tenant.ref
		WHERE tenant.ref='tenant-ledger'`,
	).Scan(&aggregateVCPUs, &aggregateMilliRetained, &subjectDocumentVCPUs, &subjectMilliRetained); err != nil {
		t.Fatal(err)
	}
	if aggregateVCPUs != 2 || aggregateMilliRetained {
		t.Errorf("converted tenant aggregate quota document maxVcpuCount = %d (maxCpuMillis retained: %v), want 2 without milli-units",
			aggregateVCPUs, aggregateMilliRetained)
	}
	if subjectDocumentVCPUs != 2 || subjectMilliRetained {
		t.Errorf("converted subject quota document maxVcpuCount = %d (maxCpuMillis retained: %v), want 2 without milli-units",
			subjectDocumentVCPUs, subjectMilliRetained)
	}

	for _, table := range []string{"runner_pools", "runners"} {
		var sealed string
		if err := upgraded.QueryRow(t.Context(),
			`SELECT backend_kind FROM secondbox.`+table+` LIMIT 1`,
		).Scan(&sealed); err != nil {
			t.Fatalf("%s backend_kind: %v", table, err)
		}
		if sealed != "" {
			t.Errorf("%s backend_kind = %q, want unsealed empty default", table, sealed)
		}
	}
}

// TestVCPUConversionFloorsQuotaAndProfileAtOneVCPU proves fractional-core
// allowances widen to one whole vCPU instead of converting to zero.
func TestVCPUConversionFloorsQuotaAndProfileAtOneVCPU(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrations(t, connection, vcpuConversionLineage...)
	seedProfileRevision(t, connection, "revision-fractional", 1,
		`{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"cold_boot"},"resources":{"cpuMillis":500}}`)
	seedSubjectQuota(t, connection, "max_cpu_millis", 500)
	applyMigrations(t, connection, "0019_vcpu_quotas_and_backend_kind.sql")

	var revisionVCPUs int64
	var hasMilliUnits bool
	if err := connection.QueryRow(t.Context(), `
		SELECT (spec_json->'resources'->>'vcpuCount')::bigint,
		       spec_json->'resources' ? 'cpuMillis'
		FROM secondbox.profile_revisions WHERE id='revision-fractional'`,
	).Scan(&revisionVCPUs, &hasMilliUnits); err != nil {
		t.Fatal(err)
	}
	if revisionVCPUs != 1 || hasMilliUnits {
		t.Errorf(
			"fractional revision vcpuCount = %d (cpuMillis retained: %v), want 1 with no milli-units",
			revisionVCPUs, hasMilliUnits,
		)
	}
	if got := subjectQuotaVCPUs(t, connection); got != 1 {
		t.Errorf("fractional quota max_vcpu_count = %d, want 1", got)
	}
}

// TestVCPUConversionSurvivesMaximumMilliUnitValues proves the ceiling
// division cannot overflow for any previously valid bigint milli-unit value.
func TestVCPUConversionSurvivesMaximumMilliUnitValues(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrations(t, connection, vcpuConversionLineage...)
	const nearMaximum = int64(9223372036854775807) - 7
	seedSubjectQuota(t, connection, "max_cpu_millis", nearMaximum)
	applyMigrations(t, connection, "0019_vcpu_quotas_and_backend_kind.sql")
	if got := subjectQuotaVCPUs(t, connection); got != nearMaximum/1000+1 {
		t.Errorf("near-maximum quota max_vcpu_count = %d, want %d", got, nearMaximum/1000+1)
	}
}

func subjectQuotaVCPUs(t *testing.T, connection *pgx.Conn) int64 {
	t.Helper()
	var maxVCPUs int64
	if err := connection.QueryRow(t.Context(), `
		SELECT max_vcpu_count FROM secondbox.subject_quotas
		WHERE tenant_ref='tenant-vcpu' AND subject_ref='subject-vcpu'`,
	).Scan(&maxVCPUs); err != nil {
		t.Fatal(err)
	}
	return maxVCPUs
}

func seedSubjectQuota(t *testing.T, connection *pgx.Conn, cpuColumn string, cpuValue int64) {
	t.Helper()
	seeded := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,`+cpuColumn+`,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at)
		VALUES ('tenant-vcpu','subject-vcpu',8,4,$1,8589934592,4,4,8,$2)`,
		cpuValue, seeded,
	); err != nil {
		t.Fatal(err)
	}
}

func seedTenantLedger(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	seeded := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.tenants (
			ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
			aggregate_quota_json,expiry_policy_json,metadata_json,revision,created_at,updated_at)
		VALUES ('tenant-ledger','active','["durable-coding"]','["sandbox:read"]',
			'{"maxSandboxes":8,"maxActiveInstances":4,"maxCpuMillis":1500,"maxMemoryBytes":8589934592,"maxSnapshots":4,"maxPortSessions":4,"maxConcurrentOperations":8,"maxActiveSubjects":4,"maxApplicationAuthorities":4}',
			'{"maximumSubjectLifetimeSeconds":3600,"maximumAuthorityLifetimeSeconds":3600}','{}',1,$1,$1)`,
		seeded,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,revision,created_at,updated_at)
		VALUES ('tenant-ledger','subject-ledger','active','none',
			'{"maxSandboxes":4,"maxActiveInstances":2,"maxCpuMillis":1500,"maxMemoryBytes":4294967296,"maxSnapshots":2,"maxPortSessions":2,"maxConcurrentOperations":4}',
			'{}',1,$1,$1)`,
		seeded,
	); err != nil {
		t.Fatal(err)
	}
}

func seedRunnerCapacity(t *testing.T, connection *pgx.Conn, capacityJSON, reservedJSON string) {
	t.Helper()
	seeded := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at)
		VALUES ('pool-vcpu','ready','["amd64"]','[]','{"maxSandboxes":8,"maxCpuMillis":16000}',1,1,$1,$1)`,
		seeded,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,revision,created_at,updated_at)
		VALUES ('runner-vcpu','pool-vcpu','runner-vcpu','ready','["amd64"]','[]',$1::jsonb,
			'[1]',1,1,'test','',0,'none',$2::jsonb,'[]',0,0,1,$3,$3)`,
		capacityJSON, reservedJSON, seeded,
	); err != nil {
		t.Fatal(err)
	}
}
