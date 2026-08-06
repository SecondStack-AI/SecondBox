// Package postgresmigrations applies the embedded, immutable SecondBox schema lineage.
package postgresmigrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	migrationLockName = "secondbox-postgres-migrations-v1"
)

//go:embed *.sql
var migrationFiles embed.FS

var migrationVersionPattern = regexp.MustCompile(`^[0-9]{4}_[a-z0-9_]+$`)

type migration struct {
	version  string
	filename string
	sql      string
	sha256   string
}

// Apply upgrades one PostgreSQL database under a process-independent advisory lock.
func Apply(ctx context.Context, databaseURL string) (resultErr error) {
	if strings.TrimSpace(databaseURL) == "" {
		return errors.New("SecondBox PostgreSQL migration database URL is required")
	}
	lineage, err := readEmbeddedMigrations()
	if err != nil {
		return err
	}
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("SecondBox PostgreSQL migration connection failed: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, connection.Close(ctx))
	}()
	if _, err := connection.Exec(
		ctx,
		`SELECT pg_advisory_lock(hashtextextended($1,0))`,
		migrationLockName,
	); err != nil {
		return fmt.Errorf("SecondBox PostgreSQL migration lock failed: %w", err)
	}
	defer func() {
		_, unlockErr := connection.Exec(
			context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtextextended($1,0))`,
			migrationLockName,
		)
		if unlockErr != nil {
			resultErr = errors.Join(
				resultErr,
				fmt.Errorf("SecondBox PostgreSQL migration unlock failed: %w", unlockErr),
			)
		}
	}()

	schemaExists, ledgerExists, err := migrationAuthorityState(ctx, connection)
	if err != nil {
		return err
	}
	if schemaExists && !ledgerExists {
		return errors.New("SecondBox PostgreSQL migration ledger is missing from an existing schema")
	}
	if ledgerExists {
		if err := repairLedgerFenceVersionCollision(ctx, connection, lineage); err != nil {
			return err
		}
	}
	appliedCount, err := validateRecordedMigrationPrefix(ctx, connection, lineage)
	if err != nil {
		return err
	}
	for _, item := range lineage[appliedCount:] {
		if err := applyMigration(ctx, connection, item); err != nil {
			return err
		}
	}
	return nil
}

func readEmbeddedMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("SecondBox embedded PostgreSQL migrations cannot be listed: %w", err)
	}
	lineage := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		content, err := migrationFiles.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf(
				"SecondBox embedded PostgreSQL migration %s cannot be read: %w",
				entry.Name(),
				err,
			)
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		if !migrationVersionPattern.MatchString(version) {
			return nil, fmt.Errorf(
				"SecondBox embedded PostgreSQL migration version %q is invalid",
				version,
			)
		}
		sum := sha256.Sum256(content)
		lineage = append(lineage, migration{
			version: version, filename: entry.Name(), sql: string(content),
			sha256: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(lineage, func(left, right int) bool {
		return lineage[left].version < lineage[right].version
	})
	if err := validateMigrationLineage(lineage); err != nil {
		return nil, err
	}
	return lineage, nil
}

// validateMigrationLineage checks a version-sorted lineage: it must start at
// the canonical baseline, be strictly ordered, and use each 4-digit numeric
// prefix exactly once so positional ledger validation has a single ordering.
func validateMigrationLineage(lineage []migration) error {
	if len(lineage) == 0 || lineage[0].version != "0001_secondbox" {
		return errors.New("SecondBox PostgreSQL migration lineage has no canonical 0001 baseline")
	}
	for index := range lineage {
		if index == 0 {
			continue
		}
		if lineage[index-1].version >= lineage[index].version {
			return errors.New("SecondBox PostgreSQL migration versions are not strictly ordered")
		}
		if lineage[index-1].version[:4] == lineage[index].version[:4] {
			return fmt.Errorf(
				"SecondBox PostgreSQL migration numeric prefix %s is not unique: %s and %s",
				lineage[index].version[:4],
				lineage[index-1].version,
				lineage[index].version,
			)
		}
	}
	return nil
}

type recordedMigration struct {
	version  string
	checksum string
}

func readRecordedMigrations(
	ctx context.Context,
	connection *pgx.Conn,
) ([]recordedMigration, error) {
	rows, err := connection.Query(ctx, `
		SELECT version,checksum_sha256
		FROM secondbox.schema_migrations
		ORDER BY version,applied_at,checksum_sha256`)
	if err != nil {
		return nil, fmt.Errorf("SecondBox PostgreSQL migration ledger read failed: %w", err)
	}
	defer rows.Close()
	var recorded []recordedMigration
	for rows.Next() {
		var item recordedMigration
		if err := rows.Scan(&item.version, &item.checksum); err != nil {
			return nil, fmt.Errorf("SecondBox PostgreSQL migration ledger scan failed: %w", err)
		}
		recorded = append(recorded, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox PostgreSQL migration ledger iteration failed: %w", err)
	}
	return recorded, nil
}

func validateRecordedMigrationPrefix(
	ctx context.Context,
	connection *pgx.Conn,
	lineage []migration,
) (int, error) {
	_, ledgerExists, err := migrationAuthorityState(ctx, connection)
	if err != nil {
		return 0, err
	}
	if !ledgerExists {
		return 0, nil
	}
	recorded, err := readRecordedMigrations(ctx, connection)
	if err != nil {
		return 0, err
	}
	return recordedMigrationPrefixLength(recorded, lineage)
}

// recordedMigrationPrefixLength requires the recorded ledger, sorted by
// version, to be an exact positional prefix of the embedded lineage.
func recordedMigrationPrefixLength(
	recorded []recordedMigration,
	lineage []migration,
) (int, error) {
	if len(recorded) > len(lineage) {
		return 0, fmt.Errorf(
			"SecondBox PostgreSQL migration ledger is ahead of embedded lineage: recorded=%d embedded=%d",
			len(recorded),
			len(lineage),
		)
	}
	for index, recordedItem := range recorded {
		embeddedItem := lineage[index]
		if recordedItem.version != embeddedItem.version {
			return 0, fmt.Errorf(
				"SecondBox PostgreSQL migration ledger is not an embedded prefix: position=%d recorded=%s embedded=%s",
				index,
				recordedItem.version,
				embeddedItem.version,
			)
		}
		if recordedItem.checksum != embeddedItem.sha256 {
			return 0, fmt.Errorf(
				"SecondBox PostgreSQL migration checksum drift: version=%s recorded=%s embedded=%s",
				recordedItem.version,
				recordedItem.checksum,
				embeddedItem.sha256,
			)
		}
	}
	return len(recorded), nil
}

const (
	fenceCommandKindVersion          = "0013_lifecycle_fence_command_kind"
	fenceCommandKindCollisionVersion = "0010_lifecycle_fence_command_kind"
)

// repairLedgerFenceVersionCollision reconciles the two published lineage
// orderings of the lifecycle fence command-kind migration. The v0.2.2 release
// embedded its content as 0010_lifecycle_fence_command_kind, which sorts
// before three migrations already shipped in v0.2.0
// (0010_lifecycle_hot_path_indexes, 0011_session_accounting_retention,
// 0012_proxied_port_transport); every other release orders the same content
// last as 0013_lifecycle_fence_command_kind. Databases created by v0.2.2
// therefore recorded the fence version at ledger position 9 and would fail
// positional prefix validation forever. When the recorded row carries the
// exact embedded content checksum and renaming it yields an exact prefix of
// the embedded lineage, the row is renamed in place; nothing is re-executed.
func repairLedgerFenceVersionCollision(
	ctx context.Context,
	connection *pgx.Conn,
	lineage []migration,
) error {
	recorded, err := readRecordedMigrations(ctx, connection)
	if err != nil {
		return err
	}
	collisionIndex := -1
	for index, item := range recorded {
		if item.version == fenceCommandKindCollisionVersion {
			collisionIndex = index
			break
		}
	}
	if collisionIndex == -1 {
		return nil
	}
	embeddedChecksum := ""
	for _, item := range lineage {
		if item.version == fenceCommandKindVersion {
			embeddedChecksum = item.sha256
			break
		}
	}
	if embeddedChecksum == "" {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration lineage is missing %s while its ledger repair is active",
			fenceCommandKindVersion,
		)
	}
	if recorded[collisionIndex].checksum != embeddedChecksum {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration ledger row %s carries checksum %s, not the embedded %s content %s; refusing to rename an unrecognized row",
			fenceCommandKindCollisionVersion,
			recorded[collisionIndex].checksum,
			fenceCommandKindVersion,
			embeddedChecksum,
		)
	}
	renamed := make([]recordedMigration, len(recorded))
	copy(renamed, recorded)
	renamed[collisionIndex].version = fenceCommandKindVersion
	sort.Slice(renamed, func(left, right int) bool {
		return renamed[left].version < renamed[right].version
	})
	if _, err := recordedMigrationPrefixLength(renamed, lineage); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration ledger recorded %s ahead of migrations that precede it in the embedded lineage; run the release binary that recorded it to completion before upgrading: %w",
			fenceCommandKindCollisionVersion,
			err,
		)
	}
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration ledger repair transaction failed: %w",
			err,
		)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `
		UPDATE secondbox.schema_migrations
		SET version=$1
		WHERE version=$2 AND checksum_sha256=$3`,
		fenceCommandKindVersion,
		fenceCommandKindCollisionVersion,
		embeddedChecksum,
	); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration ledger repair update failed: %w",
			err,
		)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration ledger repair commit failed: %w",
			err,
		)
	}
	return nil
}

func migrationAuthorityState(
	ctx context.Context,
	connection *pgx.Conn,
) (bool, bool, error) {
	var schemaName, ledgerName *string
	if err := connection.QueryRow(ctx, `
		SELECT to_regnamespace('secondbox')::text,
		       to_regclass('secondbox.schema_migrations')::text`,
	).Scan(&schemaName, &ledgerName); err != nil {
		return false, false, fmt.Errorf(
			"SecondBox PostgreSQL migration authority lookup failed: %w",
			err,
		)
	}
	return schemaName != nil, ledgerName != nil, nil
}

func applyMigration(ctx context.Context, connection *pgx.Conn, item migration) error {
	var recordedChecksum string
	err := connection.QueryRow(ctx, `
		SELECT checksum_sha256
		FROM secondbox.schema_migrations
		WHERE version=$1`,
		item.version,
	).Scan(&recordedChecksum)
	switch {
	case err == nil:
		if recordedChecksum != item.sha256 {
			return fmt.Errorf(
				"SecondBox PostgreSQL migration checksum drift: version=%s recorded=%s embedded=%s",
				item.version,
				recordedChecksum,
				item.sha256,
			)
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		// A fresh database has no ledger until the baseline transaction creates it.
		schemaExists, ledgerExists, authorityErr := migrationAuthorityState(ctx, connection)
		if authorityErr != nil {
			return authorityErr
		}
		if schemaExists || ledgerExists {
			return fmt.Errorf(
				"SecondBox PostgreSQL migration ledger lookup failed for %s: %w",
				item.version,
				err,
			)
		}
	}

	transaction, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration %s transaction failed: %w",
			item.version,
			err,
		)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, item.sql); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration %s execution failed: %w",
			item.version,
			err,
		)
	}
	if _, err := transaction.Exec(ctx, `
		INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
		VALUES ($1,$2,clock_timestamp())`,
		item.version,
		item.sha256,
	); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration %s ledger insert failed: %w",
			item.version,
			err,
		)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf(
			"SecondBox PostgreSQL migration %s commit failed: %w",
			item.version,
			err,
		)
	}
	return nil
}
