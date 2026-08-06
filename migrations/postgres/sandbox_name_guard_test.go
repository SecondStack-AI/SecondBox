package postgresmigrations

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var guardTestDatabaseURL string

func TestMain(m *testing.M) {
	raw := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if raw == "" {
		fmt.Fprintln(
			os.Stderr,
			"SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL migration tests",
		)
		os.Exit(2)
	}
	guardTestDatabaseURL = raw
	os.Exit(m.Run())
}

// newGuardDatabase creates one disposable database carrying only the baseline.
func newGuardDatabase(t *testing.T) *pgx.Conn {
	t.Helper()
	connection, _ := newDisposableDatabase(t)
	if _, err := connection.Exec(
		context.Background(), migrationSQL(t, "0001_secondbox.sql"),
	); err != nil {
		t.Fatal(err)
	}
	return connection
}

// newDisposableDatabase creates one empty disposable database and returns a
// connection to it together with its URL for callers exercising Apply.
func newDisposableDatabase(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	parsed, err := url.Parse(guardTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("secondbox_guard_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminURL := *parsed
	adminURL.Path = "/postgres"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	connection, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_ = connection.Close(cleanup)
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop guard database: %v", err)
		}
		_ = admin.Close(cleanup)
	})
	return connection, parsed.String()
}

func migrationSQL(t *testing.T, filename string) string {
	t.Helper()
	content, err := migrationFiles.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func insertGuardSandbox(
	t *testing.T,
	connection *pgx.Conn,
	id string,
	tenantRef string,
	subjectRef string,
	name string,
	deleted bool,
) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	var deletedAt *time.Time
	if deleted {
		stamp := created.Add(time.Hour)
		deletedAt = &stamp
	}
	if _, err := connection.Exec(ctx, `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'runner-1','ready',1024,1,'','','','','','{}',$5,$5)`,
		"wsp_"+id, tenantRef, subjectRef, id, created,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at,deleted_at
		) VALUES ($1,$2,$3,'profile-1','prv_1','ready','running',1,$4,'',
			jsonb_build_object('secondbox.dev/name',$5::text),'{}',1,$6,$6,$7)`,
		id, tenantRef, subjectRef, "wsp_"+id, name, created, deletedAt,
	); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxNameIndexReportsPreexistingCollisions proves an upgrade of a
// database that already holds a duplicate reserved name fails with a message
// naming the duplicate, not with a raw unique violation.
func TestSandboxNameIndexReportsPreexistingCollisions(t *testing.T) {
	connection := newGuardDatabase(t)
	insertGuardSandbox(t, connection, "sbx_guard_a", "tenant-1", "subject-1", "my-box", false)
	insertGuardSandbox(t, connection, "sbx_guard_b", "tenant-1", "subject-1", "my-box", false)

	_, err := connection.Exec(context.Background(), migrationSQL(t, "0002_sandbox_name_index.sql"))
	if err == nil {
		t.Fatal("a pre-existing duplicate reserved name must stop the migration")
	}
	message := err.Error()
	for _, want := range []string{
		"held by more than one live Sandbox",
		"tenant-1/subject-1=my-box",
		"Rename or delete the duplicates",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q; want it to contain %q", message, want)
		}
	}
}

func TestSandboxNameIndexAcceptsDistinctAndDeletedNames(t *testing.T) {
	connection := newGuardDatabase(t)
	// Distinct names, a shared name across subjects, a shared name across
	// tenants, and a deleted predecessor are all permitted.
	insertGuardSandbox(t, connection, "sbx_ok_a", "tenant-1", "subject-1", "alpha", false)
	insertGuardSandbox(t, connection, "sbx_ok_b", "tenant-1", "subject-1", "beta", false)
	insertGuardSandbox(t, connection, "sbx_ok_c", "tenant-1", "subject-2", "alpha", false)
	insertGuardSandbox(t, connection, "sbx_ok_d", "tenant-2", "subject-1", "alpha", false)
	insertGuardSandbox(t, connection, "sbx_ok_e", "tenant-1", "subject-1", "alpha", true)
	insertGuardSandbox(t, connection, "sbx_ok_f", "tenant-1", "subject-1", "alpha", true)

	if _, err := connection.Exec(
		context.Background(), migrationSQL(t, "0002_sandbox_name_index.sql"),
	); err != nil {
		t.Fatalf("a database without live duplicates must migrate: %v", err)
	}
}

// TestSandboxNameIndexEnforcesUniquenessAfterMigrating proves the guard and the
// index agree: what the guard rejects at upgrade, the index rejects afterwards.
func TestSandboxNameIndexEnforcesUniquenessAfterMigrating(t *testing.T) {
	connection := newGuardDatabase(t)
	if _, err := connection.Exec(
		context.Background(), migrationSQL(t, "0002_sandbox_name_index.sql"),
	); err != nil {
		t.Fatal(err)
	}
	insertGuardSandbox(t, connection, "sbx_after_a", "tenant-1", "subject-1", "my-box", false)
	ctx := context.Background()
	created := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(ctx, `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES ('wsp_sbx_after_b','tenant-1','subject-1','sbx_after_b','runner-1','ready',
			1024,1,'','','','','','{}',$1,$1)`, created,
	); err != nil {
		t.Fatal(err)
	}
	_, err := connection.Exec(ctx, `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at,deleted_at
		) VALUES ('sbx_after_b','tenant-1','subject-1','profile-1','prv_1','ready','running',1,
			'wsp_sbx_after_b','',jsonb_build_object('secondbox.dev/name','my-box'),'{}',1,$1,$1,NULL)`,
		created,
	)
	if err == nil || !strings.Contains(err.Error(), "sandboxes_subject_name_idx") {
		t.Fatalf("error = %v; want the reserved-name index to reject the duplicate", err)
	}
}
