// Package postgresmigrations applies the embedded, immutable SecondBox schema lineage.
package postgresmigrations

import (
	"bufio"
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

	// initialV1CatalogSHA256 freezes all SecondBox tables, columns, indexes,
	// constraints, triggers, sequences, and functions created by migration 0001.
	initialV1CatalogSHA256 = "d0bebde73afb726cc9003722f25ef3ad525338d5523fdf0fffa7cb047fe6a9ba"
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
		var recorded int64
		if err := connection.QueryRow(
			ctx,
			`SELECT count(*) FROM secondbox.schema_migrations`,
		).Scan(&recorded); err != nil {
			return fmt.Errorf("SecondBox PostgreSQL migration ledger count failed: %w", err)
		}
		if recorded == 0 {
			if err := adoptExactInitialV1Baseline(ctx, connection, lineage[0]); err != nil {
				return err
			}
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

func adoptExactInitialV1Baseline(
	ctx context.Context,
	connection *pgx.Conn,
	baseline migration,
) error {
	actual, err := postgresCatalogSHA256(ctx, connection)
	if err != nil {
		return err
	}
	if actual != initialV1CatalogSHA256 {
		return fmt.Errorf(
			"SecondBox PostgreSQL untracked baseline catalog mismatch: actual=%s expected=%s",
			actual,
			initialV1CatalogSHA256,
		)
	}
	command, err := connection.Exec(ctx, `
		INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
		SELECT $1,$2,clock_timestamp()
		WHERE NOT EXISTS (SELECT 1 FROM secondbox.schema_migrations)`,
		baseline.version,
		baseline.sha256,
	)
	if err != nil {
		return fmt.Errorf("SecondBox PostgreSQL exact baseline adoption failed: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("SecondBox PostgreSQL exact baseline adoption lost ledger authority")
	}
	return nil
}

func postgresCatalogSHA256(ctx context.Context, connection *pgx.Conn) (string, error) {
	rows, err := connection.Query(ctx, `
		WITH catalog_objects AS (
			SELECT 'table'::text AS kind,
			       json_build_array(
			         class.relname,class.relkind,class.relpersistence,
			         class.relrowsecurity,class.relforcerowsecurity
			       )::text AS detail
			FROM pg_class AS class
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox'
			  AND class.relkind IN ('r','p','v','m','f')
			UNION ALL
			SELECT 'column',
			       json_build_array(
			         class.relname,attribute.attnum,attribute.attname,
			         format_type(attribute.atttypid,attribute.atttypmod),
			         attribute.attnotnull,
			         COALESCE(pg_get_expr(default_value.adbin,default_value.adrelid),''),
			         attribute.attidentity,attribute.attgenerated
			       )::text
			FROM pg_attribute AS attribute
			JOIN pg_class AS class ON class.oid=attribute.attrelid
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			LEFT JOIN pg_attrdef AS default_value
			  ON default_value.adrelid=attribute.attrelid
			 AND default_value.adnum=attribute.attnum
			WHERE namespace.nspname='secondbox'
			  AND class.relkind IN ('r','p','v','m','f')
			  AND attribute.attnum>0
			  AND NOT attribute.attisdropped
			UNION ALL
			SELECT 'index',
			       json_build_array(
			         table_class.relname,index_class.relname,
			         pg_get_indexdef(index_class.oid)
			       )::text
			FROM pg_index AS index
			JOIN pg_class AS table_class ON table_class.oid=index.indrelid
			JOIN pg_class AS index_class ON index_class.oid=index.indexrelid
			JOIN pg_namespace AS namespace ON namespace.oid=table_class.relnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'constraint',
			       json_build_array(
			         class.relname,constraint_record.conname,
			         constraint_record.contype,
			         pg_get_constraintdef(constraint_record.oid,true)
			       )::text
			FROM pg_constraint AS constraint_record
			JOIN pg_class AS class ON class.oid=constraint_record.conrelid
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'trigger',
			       json_build_array(class.relname,trigger.tgname,pg_get_triggerdef(trigger.oid,true))::text
			FROM pg_trigger AS trigger
			JOIN pg_class AS class ON class.oid=trigger.tgrelid
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox' AND NOT trigger.tgisinternal
			UNION ALL
			SELECT 'sequence',
			       json_build_array(class.relname)::text
			FROM pg_class AS class
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox' AND class.relkind='S'
			UNION ALL
			SELECT 'function',
			       json_build_array(
			         procedure.proname,pg_get_function_identity_arguments(procedure.oid),
			         pg_get_function_result(procedure.oid),
			         pg_get_functiondef(procedure.oid)
			       )::text
			FROM pg_proc AS procedure
			JOIN pg_namespace AS namespace ON namespace.oid=procedure.pronamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'type',
			       json_build_array(
			         type_record.typname,type_record.typtype,type_record.typcategory,
			         type_record.typnotnull,
			         COALESCE(pg_get_expr(type_record.typdefaultbin,0),''),
			         COALESCE(type_record.typdefault,'')
			       )::text
			FROM pg_type AS type_record
			JOIN pg_namespace AS namespace ON namespace.oid=type_record.typnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'enum_label',
			       json_build_array(type_record.typname,enum_record.enumsortorder,enum_record.enumlabel)::text
			FROM pg_enum AS enum_record
			JOIN pg_type AS type_record ON type_record.oid=enum_record.enumtypid
			JOIN pg_namespace AS namespace ON namespace.oid=type_record.typnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'policy',
			       json_build_array(
			         class.relname,policy_record.polname,policy_record.polpermissive,
			         policy_record.polcmd,
			         pg_get_expr(policy_record.polqual,policy_record.polrelid),
			         pg_get_expr(policy_record.polwithcheck,policy_record.polrelid)
			       )::text
			FROM pg_policy AS policy_record
			JOIN pg_class AS class ON class.oid=policy_record.polrelid
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'rule',
			       json_build_array(
			         class.relname,rewrite_record.rulename,
			         pg_get_ruledef(rewrite_record.oid,true)
			       )::text
			FROM pg_rewrite AS rewrite_record
			JOIN pg_class AS class ON class.oid=rewrite_record.ev_class
			JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'collation',
			       json_build_array(
			         collation_record.collname,collation_record.collprovider,
			         collation_record.collisdeterministic,
			         collation_record.collcollate,collation_record.collctype
			       )::text
			FROM pg_collation AS collation_record
			JOIN pg_namespace AS namespace ON namespace.oid=collation_record.collnamespace
			WHERE namespace.nspname='secondbox'
			UNION ALL
			SELECT 'extension',
			       json_build_array(extension_record.extname,extension_record.extversion)::text
			FROM pg_extension AS extension_record
			JOIN pg_namespace AS namespace ON namespace.oid=extension_record.extnamespace
			WHERE namespace.nspname='secondbox'
		)
		SELECT kind,detail
		FROM catalog_objects
		ORDER BY kind,detail`)
	if err != nil {
		return "", fmt.Errorf("SecondBox PostgreSQL baseline catalog query failed: %w", err)
	}
	defer rows.Close()
	hasher := sha256.New()
	writer := bufio.NewWriter(hasher)
	for rows.Next() {
		var kind, detail string
		if err := rows.Scan(&kind, &detail); err != nil {
			return "", fmt.Errorf("SecondBox PostgreSQL baseline catalog scan failed: %w", err)
		}
		if _, err := writer.WriteString(kind + "\x1f" + detail + "\n"); err != nil {
			return "", fmt.Errorf("SecondBox PostgreSQL baseline catalog hash failed: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("SecondBox PostgreSQL baseline catalog iteration failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return "", fmt.Errorf("SecondBox PostgreSQL baseline catalog hash flush failed: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
