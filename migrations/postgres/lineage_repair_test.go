package postgresmigrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// v020MigrationFiles is the lineage every release before the fence command-kind
// migration shipped, in the order those releases applied and recorded it.
var v020MigrationFiles = []string{
	"0001_secondbox.sql",
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
}

// postFenceMigrationFiles is the lineage shipped after the fence command-kind
// migration, in the order the embedded lineage sorts it.
var postFenceMigrationFiles = []string{
	"0014_lifecycle_quiescent_schedule.sql",
}

func embeddedLineageVersions(t *testing.T) []string {
	t.Helper()
	versions := make([]string, 0, len(v020MigrationFiles)+1+len(postFenceMigrationFiles))
	for _, filename := range v020MigrationFiles {
		versions = append(versions, strings.TrimSuffix(filename, ".sql"))
	}
	versions = append(versions, fenceCommandKindVersion)
	for _, filename := range postFenceMigrationFiles {
		versions = append(versions, strings.TrimSuffix(filename, ".sql"))
	}
	return versions
}

// seedRecordedMigration executes one embedded migration file and records it in
// the ledger under recordedVersion with the checksum of its content, exactly as
// a release binary that embedded the file under that version would have.
func seedRecordedMigration(
	t *testing.T,
	connection *pgx.Conn,
	filename string,
	recordedVersion string,
) {
	t.Helper()
	sql := migrationSQL(t, filename)
	ctx := context.Background()
	if _, err := connection.Exec(ctx, sql); err != nil {
		t.Fatalf("seed %s: %v", filename, err)
	}
	sum := sha256.Sum256([]byte(sql))
	if _, err := connection.Exec(ctx, `
		INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
		VALUES ($1,$2,clock_timestamp())`,
		recordedVersion, hex.EncodeToString(sum[:]),
	); err != nil {
		t.Fatalf("record %s: %v", recordedVersion, err)
	}
}

type ledgerRow struct {
	version   string
	checksum  string
	appliedAt time.Time
}

func readLedgerRows(t *testing.T, connection *pgx.Conn) []ledgerRow {
	t.Helper()
	rows, err := connection.Query(context.Background(), `
		SELECT version,checksum_sha256,applied_at
		FROM secondbox.schema_migrations
		ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ledger := make([]ledgerRow, 0, len(v020MigrationFiles)+1)
	for rows.Next() {
		var item ledgerRow
		if err := rows.Scan(&item.version, &item.checksum, &item.appliedAt); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ledger
}

func assertLedgerVersions(t *testing.T, connection *pgx.Conn, want []string) {
	t.Helper()
	ledger := readLedgerRows(t, connection)
	got := make([]string, 0, len(ledger))
	for _, item := range ledger {
		got = append(got, item.version)
	}
	if len(got) != len(want) {
		t.Fatalf("ledger versions = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ledger versions = %v; want %v", got, want)
		}
	}
}

// TestApplyUpgradesLedgerRecordedThroughV021 proves a database that recorded
// every migration up to and including 0012 under the ordering all pre-fence
// releases shipped upgrades cleanly, applying the fence migration and every
// migration that followed it in embedded order.
func TestApplyUpgradesLedgerRecordedThroughV021(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	for _, filename := range v020MigrationFiles {
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}

	if err := Apply(context.Background(), databaseURL); err != nil {
		t.Fatalf("upgrade from the pre-fence lineage failed: %v", err)
	}
	embedded := embeddedLineageVersions(t)
	assertLedgerVersions(t, connection, embedded)

	var lastApplied string
	if err := connection.QueryRow(context.Background(), `
		SELECT version FROM secondbox.schema_migrations
		ORDER BY applied_at DESC LIMIT 1`,
	).Scan(&lastApplied); err != nil {
		t.Fatal(err)
	}
	if want := embedded[len(embedded)-1]; lastApplied != want {
		t.Fatalf("last applied migration = %s; want %s", lastApplied, want)
	}
}

// TestApplyRenamesFenceVersionRecordedByFreshV022Install proves a database that
// v0.2.2 installed from scratch — the fence migration content applied ninth and
// recorded as 0010_lifecycle_fence_command_kind — is repaired by renaming that
// one ledger row without re-executing anything, and that the repair holds on a
// second Apply.
func TestApplyRenamesFenceVersionRecordedByFreshV022Install(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	for _, filename := range v020MigrationFiles[:9] {
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}
	seedRecordedMigration(
		t, connection,
		fenceCommandKindVersion+".sql", fenceCommandKindCollisionVersion,
	)
	for _, filename := range v020MigrationFiles[9:] {
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}
	before := readLedgerRows(t, connection)

	if err := Apply(context.Background(), databaseURL); err != nil {
		t.Fatalf("repair of the v0.2.2 fence ordering failed: %v", err)
	}

	expected := make([]ledgerRow, len(before))
	copy(expected, before)
	for index := range expected {
		if expected[index].version == fenceCommandKindCollisionVersion {
			expected[index].version = fenceCommandKindVersion
		}
	}
	sort.Slice(expected, func(left, right int) bool {
		return expected[left].version < expected[right].version
	})
	// Every version this ledger already carried is renamed in place and never
	// re-executed; the migrations shipped after the fence row are applied on
	// top and sort after every one of them.
	after := readLedgerRows(t, connection)
	if len(after) != len(expected)+len(postFenceMigrationFiles) {
		t.Fatalf(
			"ledger rows = %d; want %d",
			len(after), len(expected)+len(postFenceMigrationFiles),
		)
	}
	for index := range expected {
		if after[index].version != expected[index].version ||
			after[index].checksum != expected[index].checksum ||
			!after[index].appliedAt.Equal(expected[index].appliedAt) {
			t.Fatalf(
				"ledger row %d = %+v; want the renamed original %+v",
				index, after[index], expected[index],
			)
		}
	}
	for index, filename := range postFenceMigrationFiles {
		want := strings.TrimSuffix(filename, ".sql")
		if got := after[len(expected)+index].version; got != want {
			t.Fatalf("ledger row after the repair = %s; want %s", got, want)
		}
	}

	if err := Apply(context.Background(), databaseURL); err != nil {
		t.Fatalf("second Apply after the repair failed: %v", err)
	}
	repeated := readLedgerRows(t, connection)
	if len(repeated) != len(after) {
		t.Fatalf("ledger rows after repeated Apply = %d; want %d", len(repeated), len(after))
	}
	for index := range after {
		if repeated[index].version != after[index].version ||
			repeated[index].checksum != after[index].checksum ||
			!repeated[index].appliedAt.Equal(after[index].appliedAt) {
			t.Fatalf(
				"ledger row %d changed on repeated Apply: %+v -> %+v",
				index, after[index], repeated[index],
			)
		}
	}
}

// TestApplyRejectsFenceVersionRecordedBeforePriorMigrations proves a ledger
// holding the fence row under its collision version without the migrations
// that precede it in the embedded lineage fails with the operator-facing
// instruction and is left untouched.
func TestApplyRejectsFenceVersionRecordedBeforePriorMigrations(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	for _, filename := range v020MigrationFiles[:9] {
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}
	seedRecordedMigration(
		t, connection,
		fenceCommandKindVersion+".sql", fenceCommandKindCollisionVersion,
	)
	before := readLedgerRows(t, connection)

	err := Apply(context.Background(), databaseURL)
	if err == nil {
		t.Fatal("a ledger missing migrations that precede the fence row must stop Apply")
	}
	for _, want := range []string{
		fenceCommandKindCollisionVersion,
		"run the release binary that recorded it to completion",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q; want it to contain %q", err.Error(), want)
		}
	}

	after := readLedgerRows(t, connection)
	if len(after) != len(before) {
		t.Fatalf("ledger rows = %d; want the untouched %d", len(after), len(before))
	}
	for index := range before {
		if after[index].version != before[index].version ||
			after[index].checksum != before[index].checksum ||
			!after[index].appliedAt.Equal(before[index].appliedAt) {
			t.Fatalf("ledger row %d mutated: %+v -> %+v", index, before[index], after[index])
		}
	}
}

// TestApplyRefusesToRenameFenceVersionWithForeignChecksum proves a ledger row
// that merely carries the collision version but not the fence migration's
// content is never renamed.
func TestApplyRefusesToRenameFenceVersionWithForeignChecksum(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	seedRecordedMigration(t, connection, "0001_secondbox.sql", "0001_secondbox")
	foreign := sha256.Sum256([]byte("not the fence migration content"))
	foreignChecksum := hex.EncodeToString(foreign[:])
	if _, err := connection.Exec(context.Background(), `
		INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
		VALUES ($1,$2,clock_timestamp())`,
		fenceCommandKindCollisionVersion, foreignChecksum,
	); err != nil {
		t.Fatal(err)
	}

	err := Apply(context.Background(), databaseURL)
	if err == nil {
		t.Fatal("a foreign checksum under the collision version must stop Apply")
	}
	if !strings.Contains(err.Error(), "refusing to rename an unrecognized row") {
		t.Errorf("error = %q; want the impostor refusal", err.Error())
	}

	var recordedChecksum string
	if err := connection.QueryRow(context.Background(), `
		SELECT checksum_sha256 FROM secondbox.schema_migrations WHERE version=$1`,
		fenceCommandKindCollisionVersion,
	).Scan(&recordedChecksum); err != nil {
		t.Fatalf("the impostor row must survive unrenamed: %v", err)
	}
	if recordedChecksum != foreignChecksum {
		t.Fatalf("impostor checksum = %s; want %s", recordedChecksum, foreignChecksum)
	}
}

func TestApplyCreatesFullLineageOnEmptyDatabase(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	if err := Apply(context.Background(), databaseURL); err != nil {
		t.Fatalf("fresh install failed: %v", err)
	}
	assertLedgerVersions(t, connection, embeddedLineageVersions(t))
}

func TestValidateMigrationLineageRequiresUniqueNumericPrefixes(t *testing.T) {
	duplicated := []migration{
		{version: "0001_secondbox"},
		{version: "0002_first"},
		{version: "0002_second"},
	}
	err := validateMigrationLineage(duplicated)
	if err == nil {
		t.Fatal("a duplicated numeric prefix must fail lineage validation")
	}
	for _, want := range []string{
		"SecondBox PostgreSQL migration numeric prefix 0002 is not unique",
		"0002_first",
		"0002_second",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q; want it to contain %q", err.Error(), want)
		}
	}

	distinct := []migration{
		{version: "0001_secondbox"},
		{version: "0002_first"},
		{version: "0003_second"},
	}
	if err := validateMigrationLineage(distinct); err != nil {
		t.Fatalf("distinct numeric prefixes must validate: %v", err)
	}
}

// TestEmbeddedLineageOrdersEveryShippedMigration pins the shipped lineage: one
// migration per numeric prefix, with the fence command-kind migration sorted
// after every migration that predates it.
func TestEmbeddedLineageOrdersEveryShippedMigration(t *testing.T) {
	lineage, err := readEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	want := embeddedLineageVersions(t)
	if len(lineage) != len(want) {
		t.Fatalf("embedded lineage length = %d; want %d", len(lineage), len(want))
	}
	for index := range want {
		if lineage[index].version != want[index] {
			t.Fatalf(
				"embedded lineage position %d = %s; want %s",
				index, lineage[index].version, want[index],
			)
		}
	}
}
