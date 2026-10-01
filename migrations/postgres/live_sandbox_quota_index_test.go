package postgresmigrations

import (
	"strings"
	"testing"
)

func TestLiveSandboxQuotaIndexCoversQuotaUsageAndExcludesDeletedSandboxes(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrationsBefore(t, connection, "0035_live_sandbox_quota_index.sql")
	applyMigrations(t, connection, "0035_live_sandbox_quota_index.sql")
	var definition string
	if err := connection.QueryRow(t.Context(), `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname='secondbox' AND indexname='sandboxes_live_quota_idx'`,
	).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"(tenant_ref, subject_ref)",
		"INCLUDE (state, desired_state, vcpu_count, memory_bytes, workspace_id)",
		"WHERE (state <> 'deleted'::text)",
	} {
		if !strings.Contains(definition, fragment) {
			t.Errorf("live Sandbox quota index is missing %q: %s", fragment, definition)
		}
	}
}
