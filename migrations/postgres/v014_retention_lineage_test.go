package postgresmigrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestApplyUpgradesV014RetentionBaselineWithoutRewritingLedger(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	ctx := context.Background()
	sql := strings.Replace(migrationSQL(t, "0001_secondbox.sql"), "retain_until timestamptz NOT NULL,", "retain_until timestamptz,", 1)
	sum := sha256.Sum256([]byte(sql))
	checksum := hex.EncodeToString(sum[:])
	if checksum != "1f7903ecc3f6d3e1a2c2a27af7cc4c87a3863187cde20efb8d6e795a4dd1bc42" {
		t.Fatalf("fixture no longer reproduces published v0.14.0: %s", checksum)
	}
	if _, err := connection.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO secondbox.schema_migrations (version, checksum_sha256, applied_at) VALUES ('0001_secondbox', $1, clock_timestamp())`, checksum); err != nil {
		t.Fatal(err)
	}
	for _, filename := range v020MigrationFiles[1:] {
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}
	seedRecordedMigration(t, connection, fenceCommandKindVersion+".sql", fenceCommandKindVersion)
	for _, filename := range postFenceMigrationFiles {
		if filename >= "0028" {
			break
		}
		seedRecordedMigration(t, connection, filename, strings.TrimSuffix(filename, ".sql"))
	}
	before := readLedgerRows(t, connection)
	if err := Apply(ctx, databaseURL); err != nil {
		t.Fatalf("v0.14.0 upgrade failed: %v", err)
	}
	if err := Apply(ctx, databaseURL); err != nil {
		t.Fatalf("upgrade replay failed: %v", err)
	}
	after := readLedgerRows(t, connection)
	for index, row := range before {
		if after[index] != row {
			t.Fatalf("published migration history changed: before=%+v after=%+v", row, after[index])
		}
	}
	assertLedgerVersions(t, connection, embeddedLineageVersions(t))
}
