package postgresmigrations

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var lifecycleQuiescentScheduleLineage = []string{
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
}

func TestLifecycleQuiescentScheduleConvergesOnFreshAndUpgradedSchemas(t *testing.T) {
	upgraded := newGuardDatabase(t)
	applyMigrations(t, upgraded, lifecycleQuiescentScheduleLineage...)
	applyMigrations(t, upgraded, "0014_lifecycle_quiescent_schedule.sql")

	fresh := newGuardDatabase(t)
	applyMigrations(
		t, fresh,
		append(
			append([]string{}, lifecycleQuiescentScheduleLineage...),
			"0014_lifecycle_quiescent_schedule.sql",
		)...,
	)

	upgradedShape := lifecycleReconcileIndexShape(t, upgraded)
	freshShape := lifecycleReconcileIndexShape(t, fresh)
	if upgradedShape != freshShape {
		t.Fatalf(
			"upgraded and fresh lifecycle reconcile index shapes differ:\nupgraded:\n%s\nfresh:\n%s",
			upgradedShape, freshShape,
		)
	}
	for _, fragment := range []string{
		"sandboxes_lifecycle_reconcile_idx",
		"(next_reconcile_at, id)",
		"next_reconcile_at IS NOT NULL",
		"state <> 'deleted'::text",
	} {
		if !strings.Contains(upgradedShape, fragment) {
			t.Errorf("lifecycle reconcile index is missing %q:\n%s", fragment, upgradedShape)
		}
	}
	// The rest matrix belongs to the reconciler, not to the index predicate.
	if strings.Contains(upgradedShape, "desired_state") {
		t.Errorf(
			"lifecycle reconcile index still restates the reconciler rest matrix:\n%s",
			upgradedShape,
		)
	}
}

// TestLifecycleQuiescentScheduleParksSandboxesAlreadyAtRest covers the upgrade
// of a populated database: a Sandbox that reached rest under the old predicate
// kept a stale past deadline, and the migration parks it so the upgraded
// deployment does not claim its whole resting population on the first poll.
func TestLifecycleQuiescentScheduleParksSandboxesAlreadyAtRest(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrations(t, connection, lifecycleQuiescentScheduleLineage...)
	seeded := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for _, sandbox := range []struct {
		id       string
		state    string
		desired  string
		wantPark bool
	}{
		{id: "sbx-rest-stopped", state: "stopped", desired: "stopped", wantPark: true},
		{id: "sbx-rest-failed", state: "failed", desired: "stopped", wantPark: true},
		{id: "sbx-rest-deleted", state: "deleted", desired: "deleted", wantPark: true},
		{id: "sbx-pending-start", state: "stopped", desired: "running", wantPark: false},
		{id: "sbx-pending-delete", state: "stopped", desired: "deleted", wantPark: false},
		{id: "sbx-pending-ready", state: "ready", desired: "running", wantPark: false},
	} {
		seedQuiescenceSandbox(t, connection, sandbox.id, sandbox.state, sandbox.desired, seeded)
	}

	applyMigrations(t, connection, "0014_lifecycle_quiescent_schedule.sql")

	for id, wantPark := range map[string]bool{
		"sbx-rest-stopped":   true,
		"sbx-rest-failed":    true,
		"sbx-rest-deleted":   true,
		"sbx-pending-start":  false,
		"sbx-pending-delete": false,
		"sbx-pending-ready":  false,
	} {
		var deadline *time.Time
		var revision int64
		if err := connection.QueryRow(t.Context(), `
			SELECT next_reconcile_at,revision FROM secondbox.sandboxes WHERE id=$1`,
			id,
		).Scan(&deadline, &revision); err != nil {
			t.Fatal(err)
		}
		if parked := deadline == nil; parked != wantPark {
			t.Errorf("Sandbox %s parked=%t, want %t", id, parked, wantPark)
		}
		// Parking changes no field a caller observes, so the public revision
		// must survive the upgrade untouched.
		if revision != 7 {
			t.Errorf("Sandbox %s revision = %d, want the seeded 7", id, revision)
		}
	}
}

func seedQuiescenceSandbox(
	t *testing.T,
	connection *pgx.Conn,
	sandboxID string,
	state string,
	desiredState string,
	seeded time.Time,
) {
	t.Helper()
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			last_activity_at,revision,lifecycle_termination_reason,lifecycle_failure_class,
			lifecycle_failure_message,lifecycle_intent_kind,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,reconcile_retry_count,reconcile_retry_limit,
			created_at,updated_at,deleted_at
		) VALUES (
			$1,'tenant-quiescence','subject-quiescence','profile-quiescence','revision-quiescence',
			$2,$3,1,$1 || '-workspace','','{}','{}',$4,7,'','','','','' ,NULL,$4,0,8,$4,$4,NULL
		)`,
		sandboxID, state, desiredState, seeded,
	); err != nil {
		t.Fatal(err)
	}
}

func lifecycleReconcileIndexShape(t *testing.T, connection *pgx.Conn) string {
	t.Helper()
	var shape string
	if err := connection.QueryRow(t.Context(), `
		SELECT string_agg(indexname || ':' || indexdef, E'\n' ORDER BY indexname)
		FROM pg_indexes
		WHERE schemaname='secondbox' AND indexname='sandboxes_lifecycle_reconcile_idx'`,
	).Scan(&shape); err != nil {
		t.Fatal(err)
	}
	return shape
}
