package postgresmigrations

import (
	"testing"
)

// TestSubjectSandboxPolicyProfilesRewritesSingleProfileSelections proves a
// stored single-Profile selection becomes a one-element Profile set with its
// other values intact, an absent policy stays absent, and re-running is harmless.
func TestSubjectSandboxPolicyProfilesRewritesSingleProfileSelections(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrationsBefore(t, connection, "0037_subject_sandbox_policy_profiles.sql")
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.subjects(tenant_ref,ref,state,cleanup_state,cleanup_operation_id,quota_json,metadata_json,sandbox_policy_json,revision,created_at,updated_at)
		VALUES
			('tenant','selected','active','none','','{}','{}','{"profile":"agent","lifecycle":{"idleSeconds":60,"maximumDurationSeconds":null},"attributedExecution":{"maximumConnections":64}}',3,now(),now()),
			('tenant','unselected','active','none','','{}','{}',NULL,1,now(),now())`,
	); err != nil {
		t.Fatal(err)
	}
	applyMigrations(t, connection, "0037_subject_sandbox_policy_profiles.sql")
	applyMigrations(t, connection, "0037_subject_sandbox_policy_profiles.sql")
	selected := `{"profiles": ["agent"], "lifecycle": {"idleSeconds": 60, "maximumDurationSeconds": null}, "attributedExecution": {"maximumConnections": 64}}`
	for ref, want := range map[string]*string{"selected": &selected, "unselected": nil} {
		var policy *string
		var revision int64
		if err := connection.QueryRow(t.Context(), `SELECT sandbox_policy_json::text,revision FROM secondbox.subjects WHERE tenant_ref='tenant' AND ref=$1`, ref).Scan(&policy, &revision); err != nil {
			t.Fatal(err)
		}
		if (policy == nil) != (want == nil) || policy != nil && *policy != *want {
			t.Errorf("%s policy = %v, want %v", ref, deref(policy), deref(want))
		}
		if ref == "selected" && revision != 3 {
			t.Errorf("migration changed the Subject revision to %d", revision)
		}
	}
}

func deref(value *string) string {
	if value == nil {
		return "<null>"
	}
	return *value
}
