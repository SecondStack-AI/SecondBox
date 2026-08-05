package postgresmigrations

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLifecycleHotPathIndexesConvergeOnFreshAndUpgradedSchemas(t *testing.T) {
	prior := []string{
		"0002_sandbox_name_index.sql",
		"0003_control_plane_wakeups.sql",
		"0004_direct_port_data_plane.sql",
		"0005_relay_data_plane_wakeups.sql",
		"0006_eager_assignment_dispatch.sql",
		"0007_relay_frame_retention.sql",
		"0008_workspace_relocations.sql",
		"0009_remove_frame_relay.sql",
	}
	upgraded := newGuardDatabase(t)
	applyMigrations(t, upgraded, prior...)
	applyMigrations(t, upgraded, "0010_lifecycle_hot_path_indexes.sql")

	fresh := newGuardDatabase(t)
	applyMigrations(t, fresh, append(prior, "0010_lifecycle_hot_path_indexes.sql")...)

	upgradedShape := lifecycleHotPathIndexShape(t, upgraded)
	freshShape := lifecycleHotPathIndexShape(t, fresh)
	if upgradedShape != freshShape {
		t.Fatalf("upgraded and fresh lifecycle index shapes differ:\nupgraded:\n%s\nfresh:\n%s", upgradedShape, freshShape)
	}
	for _, fragment := range []string{
		"sandboxes_lifecycle_reconcile_idx",
		"(next_reconcile_at, id)",
		"state <> 'deleted'::text",
		"leases_active_expiry_idx",
		"(expires_at, id)",
		"state = 'active'::text",
	} {
		if !strings.Contains(upgradedShape, fragment) {
			t.Errorf("lifecycle hot-path indexes are missing %q:\n%s", fragment, upgradedShape)
		}
	}
}

func lifecycleHotPathIndexShape(t *testing.T, connection *pgx.Conn) string {
	t.Helper()
	var shape string
	if err := connection.QueryRow(t.Context(), `
		SELECT string_agg(indexname || ':' || indexdef, E'\n' ORDER BY indexname)
		FROM pg_indexes
		WHERE schemaname='secondbox'
		  AND indexname IN ('sandboxes_lifecycle_reconcile_idx','leases_active_expiry_idx')`,
	).Scan(&shape); err != nil {
		t.Fatal(err)
	}
	return shape
}
