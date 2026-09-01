package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestReadEgressContextPreflightComparesRequirementsAndGroupsAssignments(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES (
			'prv-egress-diagnostics','profile-egress-diagnostics',4,
			'{"pool":"pool-egress-diagnostics","network":{"requiresTenantEgressContext":true}}',$1
		);
		INSERT INTO secondbox.profiles (
			name,state,current_revision_id,revision,created_at,updated_at
		) VALUES (
			'profile-egress-diagnostics','enabled','prv-egress-diagnostics',4,$1,$1
		);
		INSERT INTO secondbox.tenants (
			ref,state,egress_context,allowed_profile_grants_json,
			allowed_application_scopes_json,aggregate_quota_json,expiry_policy_json,
			metadata_json,revision,created_at,updated_at
		) VALUES
			('tenant-egress-ready','active','customer-a','["profile-egress-diagnostics"]',
			 '[]','{}','{}','{}',1,$1,$1),
			('tenant-egress-missing','active',NULL,'["profile-egress-diagnostics"]',
			 '[]','{}','{}','{}',1,$1,$1),
			('tenant-egress-unavailable','active','customer-c','["profile-egress-diagnostics"]',
			 '[]','{}','{}','{}',1,$1,$1);
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,backend_kind,
			supported_egress_contexts_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES
			('runner-egress-ready','pool-egress-diagnostics','runner-egress-ready','ready',
			 '["amd64"]','[]','{}','[4]',1,1,'test','connection-egress-ready',0,
			 'active','{}','{}','firecracker','["customer-a","customer-b"]',0,0,$1,1,$1,$1),
			('runner-egress-disconnected','pool-egress-diagnostics','runner-egress-disconnected','ready',
			 '["amd64"]','[]','{}','[4]',1,1,'test','',0,
			 'active','{}','{}','firecracker','["customer-c"]',0,0,$1,1,$1,$1);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,egress_context,revision,created_at,updated_at
		) VALUES
			('assignment-egress-a','sandbox-egress-a','instance-egress-a','runner-egress-ready',
			 'prv-egress-diagnostics','firecracker','',1,$2,'ready','{}','[]','{}','',0,3,
			 $3,$3,'',$3,$3,'customer-a',1,$1,$1),
			('assignment-egress-b','sandbox-egress-b','instance-egress-b','runner-egress-ready',
			 'prv-egress-diagnostics','firecracker','',1,$2,'ready','{}','[]','{}','',0,3,
			 $3,$3,'',$3,$3,'customer-a',1,$1,$1)
	`, pgx.QueryExecModeSimpleProtocol, now, []byte("01234567890123456789012345678901"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	preflight, err := store.ReadEgressContextPreflight(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Ready || preflight.Truncated {
		t.Fatalf("preflight readiness = ready %v truncated %v", preflight.Ready, preflight.Truncated)
	}
	if len(preflight.Requirements) != 3 {
		t.Fatalf("requirements = %#v", preflight.Requirements)
	}
	ready := preflight.Requirements[1]
	if ready.Status != "ready" || len(ready.CompatibleRunnerIDs) != 1 ||
		ready.CompatibleRunnerIDs[0] != "runner-egress-ready" {
		t.Fatalf("ready requirement = %#v", ready)
	}
	if preflight.Requirements[0].Status != "tenant_context_missing" {
		t.Fatalf("missing requirement = %#v", preflight.Requirements[0])
	}
	if preflight.Requirements[2].Status != "runner_context_unavailable" {
		t.Fatalf("unavailable requirement = %#v", preflight.Requirements[2])
	}
	if len(preflight.Runners) != 2 || preflight.Runners[0].Connected ||
		!preflight.Runners[1].Connected {
		t.Fatalf("Runner advertisements = %#v", preflight.Runners)
	}
	if len(preflight.ActiveAssignments) != 1 ||
		preflight.ActiveAssignments[0].Count != 2 ||
		preflight.ActiveAssignments[0].EgressContext == nil ||
		*preflight.ActiveAssignments[0].EgressContext != "customer-a" {
		t.Fatalf("active assignments = %#v", preflight.ActiveAssignments)
	}
}
