package postgresmigrations

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	pinnedAssetsSpec   = `{"pool":"standard-amd64","architecture":"amd64","runtimeBundleDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","toolchainBundleDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","startup":{"mode":"cold_boot"}}`
	unpinnedAssetsSpec = `{"pool":"standard-amd64","architecture":"amd64","startup":{"mode":"cold_boot"}}`
)

// TestProfileExecutionAssetsUnpinnedConvergesOnFreshAndUpgradedSchemas proves
// an upgraded revision ends with exactly the spec a fresh deployment records,
// and that re-running the migration is harmless.
func TestProfileExecutionAssetsUnpinnedConvergesOnFreshAndUpgradedSchemas(t *testing.T) {
	upgraded := newGuardDatabase(t)
	applyMigrationsBefore(t, upgraded, "0031_profile_execution_assets_unpinned.sql")
	seedProfileRevision(t, upgraded, "revision-upgraded", 1, pinnedAssetsSpec)
	applyMigrations(t, upgraded, "0031_profile_execution_assets_unpinned.sql")
	applyMigrations(t, upgraded, "0031_profile_execution_assets_unpinned.sql")

	fresh := newGuardDatabase(t)
	applyMigrationsBefore(t, fresh, "0031_profile_execution_assets_unpinned.sql")
	applyMigrations(t, fresh, "0031_profile_execution_assets_unpinned.sql")
	seedProfileRevision(t, fresh, "revision-fresh", 1, unpinnedAssetsSpec)

	upgradedSpec := profileRevisionSpec(t, upgraded, "revision-upgraded")
	freshSpec := profileRevisionSpec(t, fresh, "revision-fresh")
	if upgradedSpec != freshSpec {
		t.Fatalf(
			"upgraded and fresh Profile revision specs differ:\nupgraded: %s\nfresh:    %s",
			upgradedSpec, freshSpec,
		)
	}
}

func applyMigrationsBefore(t *testing.T, connection *pgx.Conn, filename string) {
	t.Helper()
	lineage, err := readEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range lineage[1:] {
		if item.filename == filename {
			return
		}
		applyMigrations(t, connection, item.filename)
	}
	t.Fatalf("migration %s is absent from the embedded lineage", filename)
}
