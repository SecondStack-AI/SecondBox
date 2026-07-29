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
	if len(lineage) == 0 || lineage[0].version != "0001_secondbox" {
		return nil, errors.New("SecondBox PostgreSQL migration lineage has no canonical 0001 baseline")
	}
	for index := range lineage {
		if index != 0 && lineage[index-1].version >= lineage[index].version {
			return nil, errors.New("SecondBox PostgreSQL migration versions are not strictly ordered")
		}
	}
	return lineage, nil
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
	rows, err := connection.Query(ctx, `
		SELECT version,checksum_sha256
		FROM secondbox.schema_migrations
		ORDER BY version,applied_at,checksum_sha256`)
	if err != nil {
		return 0, fmt.Errorf("SecondBox PostgreSQL migration ledger read failed: %w", err)
	}
	defer rows.Close()
	type recordedMigration struct {
		version  string
		checksum string
	}
	recorded := make([]recordedMigration, 0, len(lineage))
	for rows.Next() {
		var item recordedMigration
		if err := rows.Scan(&item.version, &item.checksum); err != nil {
			return 0, fmt.Errorf("SecondBox PostgreSQL migration ledger scan failed: %w", err)
		}
		recorded = append(recorded, item)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("SecondBox PostgreSQL migration ledger iteration failed: %w", err)
	}
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
