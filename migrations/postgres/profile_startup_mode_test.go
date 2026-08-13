package postgresmigrations

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var profileStartupModeLineage = []string{
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
}

// coldBootSpec is what a Profile revision written after this migration looks
// like: the operator stated the startup mode. preFieldSpec is the same revision
// as it was recorded before the field existed.
const (
	coldBootSpec  = `{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"cold_boot"},"resources":{"vcpuCount":1}}`
	preFieldSpec  = `{"pool":"standard-amd64","architecture":"amd64","resources":{"vcpuCount":1}}`
	resumeSpec    = `{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"snapshot_resume"},"resources":{"vcpuCount":1}}`
	seededProfile = "profile-startup"
)

// TestProfileStartupModeConvergesOnFreshAndUpgradedSchemas proves the honest
// reading of the backfill: a revision that predates the field ends up with
// exactly the spec a fresh deployment records for the behavior it already had.
func TestProfileStartupModeConvergesOnFreshAndUpgradedSchemas(t *testing.T) {
	upgraded := newGuardDatabase(t)
	applyMigrations(t, upgraded, profileStartupModeLineage...)
	seedProfileRevision(t, upgraded, "revision-upgraded", 1, preFieldSpec)
	applyMigrations(t, upgraded, "0015_profile_startup_mode.sql")

	fresh := newGuardDatabase(t)
	applyMigrations(t, fresh, profileStartupModeLineage...)
	applyMigrations(t, fresh, "0015_profile_startup_mode.sql")
	seedProfileRevision(t, fresh, "revision-fresh", 1, coldBootSpec)

	upgradedSpec := profileRevisionSpec(t, upgraded, "revision-upgraded")
	freshSpec := profileRevisionSpec(t, fresh, "revision-fresh")
	if upgradedSpec != freshSpec {
		t.Fatalf(
			"upgraded and fresh Profile revision specs differ:\nupgraded: %s\nfresh:    %s",
			upgradedSpec, freshSpec,
		)
	}
}

// TestProfileStartupModeBackfillStampsOnlyRevisionsWithoutOne proves the guard:
// a revision that already states a mode is untouched, so re-running the lineage
// cannot rewrite an operator's snapshot_resume revision to cold_boot.
func TestProfileStartupModeBackfillStampsOnlyRevisionsWithoutOne(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrations(t, connection, profileStartupModeLineage...)
	seedProfileRevision(t, connection, "revision-pre-field", 1, preFieldSpec)
	seedProfileRevision(t, connection, "revision-cold", 2, coldBootSpec)
	seedProfileRevision(t, connection, "revision-resume", 3, resumeSpec)

	// Applying twice proves the backfill is idempotent, which is what a repeated
	// or partially observed migration run needs.
	applyMigrations(t, connection, "0015_profile_startup_mode.sql")
	applyMigrations(t, connection, "0015_profile_startup_mode.sql")

	for revisionID, wantMode := range map[string]string{
		"revision-pre-field": "cold_boot",
		"revision-cold":      "cold_boot",
		"revision-resume":    "snapshot_resume",
	} {
		var decoded struct {
			Startup struct {
				Mode string `json:"mode"`
			} `json:"startup"`
		}
		if err := json.Unmarshal(
			[]byte(profileRevisionSpec(t, connection, revisionID)), &decoded,
		); err != nil {
			t.Fatalf("decode %s spec: %v", revisionID, err)
		}
		if decoded.Startup.Mode != wantMode {
			t.Errorf("%s startup mode = %q, want %q", revisionID, decoded.Startup.Mode, wantMode)
		}
	}
}

func seedProfileRevision(
	t *testing.T,
	connection *pgx.Conn,
	revisionID string,
	number int64,
	specJSON string,
) {
	t.Helper()
	seeded := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (id,profile_name,revision_number,spec_json,created_at)
		VALUES ($1,$2,$3,$4::jsonb,$5)`,
		revisionID, seededProfile, number, specJSON, seeded,
	); err != nil {
		t.Fatal(err)
	}
}

func profileRevisionSpec(t *testing.T, connection *pgx.Conn, revisionID string) string {
	t.Helper()
	var spec string
	if err := connection.QueryRow(t.Context(), `
		SELECT spec_json::text FROM secondbox.profile_revisions WHERE id=$1`,
		revisionID,
	).Scan(&spec); err != nil {
		t.Fatal(err)
	}
	return spec
}
