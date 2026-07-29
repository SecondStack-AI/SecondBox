package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
)

func TestSecondBoxBaselineOwnsCleanLogicalSchemaWithoutPhysicalCrossTableConstraints(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate SecondBox schema contract test")
	}
	migrationPath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "migrations", "postgres", "0001_secondbox.sql")
	contents, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(contents))
	for _, table := range []string{
		"profiles", "profile_revisions",
		"runner_pools", "runners", "sandboxes", "workspaces", "instances",
		"assignments", "leases", "activity_sessions", "activity_touches",
		"snapshots", "workspace_restores", "artifacts",
		"port_sessions", "data_plane_sessions", "data_plane_frames",
		"operations", "idempotency_records", "audit_events",
	} {
		if !strings.Contains(sql, "create table secondbox."+table) {
			t.Errorf("SecondBox baseline is missing secondbox.%s", table)
		}
	}
	for _, forbidden := range []string{
		"foreign key", " references ", " check ",
		"resource_classes", "lifecycle_policies", "agent_service",
		"host_path", "image_path", "workspace_path",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("SecondBox baseline contains removed or forbidden schema fragment %q", forbidden)
		}
	}
	for _, fragment := range []string{
		"home_runner_id text not null",
		"logical_capacity_bytes bigint not null",
		"mutation_effect_id text not null",
		"create index workspaces_home_state_idx",
		"create table secondbox.workspace_restores",
		"create index workspace_restores_home_state_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("SecondBox baseline is missing local Workspace authority %q", fragment)
		}
	}
}

func TestPostgresMigrationsApplyCleanlyAndRejectReorderedLineage(t *testing.T) {
	if err := postgresmigrations.Apply(t.Context(), integrationDatabaseURL); err != nil {
		t.Fatalf("idempotent migration apply failed: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	const (
		baselineVersion  = "0001_secondbox"
		reorderedVersion = "0000_reordered"
	)
	t.Cleanup(func() {
		if _, err := pool.Exec(
			context.Background(),
			`UPDATE secondbox.schema_migrations SET version=$1 WHERE version=$2`,
			baselineVersion, reorderedVersion,
		); err != nil {
			t.Errorf("restore reordered migration fixture: %v", err)
		}
	})
	if _, err := pool.Exec(
		t.Context(),
		`UPDATE secondbox.schema_migrations SET version=$1 WHERE version=$2`,
		reorderedVersion, baselineVersion,
	); err != nil {
		t.Fatal(err)
	}

	err = postgresmigrations.Apply(t.Context(), integrationDatabaseURL)
	if err == nil || !strings.Contains(err.Error(), "not an embedded prefix") {
		t.Fatalf("reordered migration lineage error = %v", err)
	}
}

func TestOwnershipColumnsAndSubjectQuotaArePresentAfterFreshMigration(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ownedTables := []string{
		"sandboxes",
		"workspaces",
		"leases",
		"activity_sessions",
		"activity_touches",
		"snapshots",
		"workspace_restores",
		"artifacts",
		"port_sessions",
		"data_plane_sessions",
		"operations",
		"idempotency_records",
		"audit_events",
	}
	for _, table := range ownedTables {
		for _, column := range []string{"tenant_ref", "subject_ref"} {
			var generated, expression, nullable string
			if err := pool.QueryRow(t.Context(), `
				SELECT is_generated,COALESCE(generation_expression,''),is_nullable
				FROM information_schema.columns
				WHERE table_schema='secondbox' AND table_name=$1 AND column_name=$2`,
				table,
				column,
			).Scan(&generated, &expression, &nullable); err != nil {
				t.Errorf("secondbox.%s.%s: %v", table, column, err)
				continue
			}
			migrated := table == "sandboxes" || table == "workspaces" ||
				table == "leases" || table == "activity_sessions" ||
				table == "activity_touches" || table == "snapshots" ||
				table == "workspace_restores" || table == "artifacts" ||
				table == "port_sessions" || table == "data_plane_sessions" ||
				table == "operations" || table == "idempotency_records" ||
				table == "audit_events"
			if migrated && (generated != "NEVER" || expression != "" || nullable != "NO") {
				t.Errorf(
					"secondbox.%s.%s migration = %q %q nullable=%q",
					table,
					column,
					generated,
					expression,
					nullable,
				)
			}
		}
	}

	for _, table := range append(ownedTables,
		"operators", "operator_credentials", "projects", "project_quotas",
		"service_accounts", "api_keys", "profile_quotas",
	) {
		var count int
		if err := pool.QueryRow(t.Context(), `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema='secondbox' AND table_name=$1 AND column_name='project_id'`,
			table,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("secondbox.%s still has project_id", table)
		}
	}
	for _, table := range []string{
		"operators", "operator_credentials", "projects", "project_quotas",
		"service_accounts", "api_keys", "profile_quotas",
	} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `SELECT to_regclass('secondbox.' || $1) IS NOT NULL`, table).
			Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("secondbox.%s still exists", table)
		}
	}

	var primaryKey string
	if err := pool.QueryRow(t.Context(), `
		SELECT indexdef
		FROM pg_indexes
		WHERE schemaname='secondbox'
		  AND tablename='subject_quotas'
		  AND indexname='subject_quotas_pkey'`,
	).Scan(&primaryKey); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(primaryKey, "(tenant_ref, subject_ref)") {
		t.Fatalf("subject quota primary key = %q", primaryKey)
	}
}
