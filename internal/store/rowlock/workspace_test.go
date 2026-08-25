package rowlock

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var rowlockTestDatabaseURL string

func TestMain(m *testing.M) {
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		fmt.Fprintln(os.Stderr, "SECONDBOX_TEST_DATABASE_URL is required for rowlock tests")
		os.Exit(2)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	databaseName := fmt.Sprintf("secondbox_rowlock_test_%d", os.Getpid())
	adminURL := *parsed
	adminURL.Path = "/postgres"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		panic(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		panic(err)
	}
	parsed.Path = "/" + databaseName
	rowlockTestDatabaseURL = parsed.String()
	if err := postgresmigrations.Apply(ctx, rowlockTestDatabaseURL); err != nil {
		panic(err)
	}
	code := m.Run()
	if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
		panic(err)
	}
	if err := admin.Close(ctx); err != nil {
		panic(err)
	}
	os.Exit(code)
}

func TestSandboxWorkspaceSnapshotHelpersHoldDocumentedLockOrder(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, "order")

	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if err := blocker.QueryRow(t.Context(), `SELECT id FROM secondbox.workspaces WHERE id=$1 FOR UPDATE`, fixture.workspaceID).Scan(new(string)); err != nil {
		t.Fatal(err)
	}

	helper, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Rollback(t.Context())
	var helperPID int32
	if err := helper.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&helperPID); err != nil {
		t.Fatal(err)
	}
	type helperResult struct {
		locked   SandboxWorkspace
		snapshot Snapshot
		err      error
	}
	result := make(chan helperResult, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		locked, lockErr := SandboxWorkspaceForSubject(t.Context(), helper, fixture.tenantRef, fixture.subjectRef, fixture.sandboxID)
		var snapshot Snapshot
		if lockErr == nil {
			snapshot, lockErr = SnapshotForSubject(t.Context(), helper, fixture.tenantRef, fixture.subjectRef, locked, fixture.snapshotID)
		}
		result <- helperResult{locked: locked, snapshot: snapshot, err: lockErr}
		<-release
	}()
	waitForBackendLock(t, pool, helperPID)
	assertRowLockUnavailable(t, pool, "secondbox.sandboxes", fixture.sandboxID)

	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil {
		close(release)
		<-done
		t.Fatal(got.err)
	}
	if got.locked.WorkspaceID != fixture.workspaceID || got.snapshot.ID != fixture.snapshotID {
		close(release)
		<-done
		t.Fatalf("locked authority = %#v snapshot = %#v", got.locked, got.snapshot)
	}
	assertRowLockUnavailable(t, pool, "secondbox.sandboxes", fixture.sandboxID)
	assertRowLockUnavailable(t, pool, "secondbox.workspaces", fixture.workspaceID)
	assertRowLockUnavailable(t, pool, "secondbox.snapshots", fixture.snapshotID)
	close(release)
	<-done
}

func TestQuotaLedgerPrecedesSandboxDuringLifecycleMutation(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, fmt.Sprintf("quota-before-sandbox-%d", time.Now().UnixNano()))

	quotaBlocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer quotaBlocker.Rollback(t.Context())
	if err := quotaBlocker.QueryRow(t.Context(), `
		SELECT tenant_ref FROM secondbox.tenant_quotas
		WHERE tenant_ref=$1 FOR UPDATE`, fixture.tenantRef,
	).Scan(new(string)); err != nil {
		t.Fatal(err)
	}

	mutation, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Rollback(t.Context())
	var mutationPID int32
	if err := mutation.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&mutationPID); err != nil {
		t.Fatal(err)
	}
	mutationResult := make(chan error, 1)
	go func() {
		_, lockErr := SandboxWorkspaceForSubject(
			t.Context(), mutation, fixture.tenantRef, fixture.subjectRef, fixture.sandboxID,
		)
		if lockErr == nil {
			_, lockErr = mutation.Exec(t.Context(), `
				UPDATE secondbox.sandboxes SET desired_state='running'
				WHERE id=$1`, fixture.sandboxID)
		}
		mutationResult <- lockErr
	}()
	waitForBackendLock(t, pool, mutationPID)

	probe, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var sandboxID string
	if err := probe.QueryRow(t.Context(), `
		SELECT id FROM secondbox.sandboxes WHERE id=$1 FOR UPDATE NOWAIT`, fixture.sandboxID,
	).Scan(&sandboxID); err != nil {
		if rollbackErr := probe.Rollback(t.Context()); rollbackErr != nil {
			t.Fatalf("rollback Sandbox lock probe after query failure: %v (query: %v)", rollbackErr, err)
		}
		t.Fatalf("lifecycle mutation locked Sandbox before quota ledger: %v", err)
	}
	if err := probe.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := quotaBlocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-mutationResult; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotByIDReverifiesLogicalLinkageAfterLock(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, "linkage")
	mismatchedSnapshotID := "snapshot-rowlock-mismatch"
	now := time.Now().UTC()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at
		) VALUES ($1,$2,$3,'different-sandbox','different-workspace','different-runner',
			'different-operation','different-effect','{}',1,'mismatch',1,'{}','ready',$4,$4,$4)`,
		mismatchedSnapshotID, fixture.tenantRef, fixture.subjectRef, now,
	); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	locked, err := SandboxWorkspaceByID(t.Context(), tx, fixture.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotByID(t.Context(), tx, locked, mismatchedSnapshotID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched Snapshot linkage error = %v", err)
	}
	assertRowLockUnavailable(t, pool, "secondbox.snapshots", mismatchedSnapshotID)
}

type rowlockFixture struct {
	tenantRef   string
	subjectRef  string
	sandboxID   string
	workspaceID string
	snapshotID  string
}

func seedRowlockFixture(t *testing.T, pool *pgxpool.Pool, suffix string) rowlockFixture {
	t.Helper()
	fixture := rowlockFixture{
		tenantRef:   "tenant-rowlock-" + suffix,
		subjectRef:  "subject-rowlock-" + suffix,
		sandboxID:   "sandbox-rowlock-" + suffix,
		workspaceID: "workspace-rowlock-" + suffix,
		snapshotID:  "snapshot-rowlock-" + suffix,
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ($2,10,10,10000,10737418240,10,10,10,10,10,$5);
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_cpu_millis,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($2,$3,10,10,10000,10737418240,10,10,10,$5);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES ($1,$2,$3,$4,'runner-rowlock','ready',1048576,1,'','','','','','{}',$5,$5);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,
			compatibility_summary_json,revision,created_at,updated_at
		) VALUES ($4,$2,$3,'profile-rowlock','revision-rowlock','stopped','stopped',
			1,$1,'','{}','{}',1,$5,$5);
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at
		) VALUES ($6,$2,$3,$4,$1,'runner-rowlock',$7,$8,'{}',1,'snapshot',1,
			'{}','ready',$9,$5,$5)`,
		pgx.QueryExecModeSimpleProtocol,
		fixture.workspaceID, fixture.tenantRef, fixture.subjectRef, fixture.sandboxID, now,
		fixture.snapshotID, "operation-rowlock-"+suffix, "effect-rowlock-"+suffix, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func openRowlockTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), rowlockTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func waitForBackendLock(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(t.Context(), `
			SELECT COALESCE(wait_event_type='Lock',false)
			FROM pg_stat_activity WHERE pid=$1`, pid,
		).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not block on the Workspace lock")
}

func assertRowLockUnavailable(t *testing.T, pool *pgxpool.Pool, table, id string) {
	t.Helper()
	query := fmt.Sprintf("SELECT id FROM %s WHERE id=$1 FOR UPDATE NOWAIT", table)
	var lockedID string
	err := pool.QueryRow(t.Context(), query, id).Scan(&lockedID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("NOWAIT lock for %s %q error = %v", table, id, err)
	}
}
