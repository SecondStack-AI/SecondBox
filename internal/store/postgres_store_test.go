package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var storeTestDatabaseURL string

func TestMain(m *testing.M) {
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		fmt.Fprintln(os.Stderr, "SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL store tests")
		os.Exit(2)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	databaseName := fmt.Sprintf("secondbox_store_test_%d", os.Getpid())
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
	storeTestDatabaseURL = parsed.String()
	if err := postgresmigrations.Apply(ctx, storeTestDatabaseURL); err != nil {
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

func openStoreTest(t *testing.T) *PostgresControlPlaneStore {
	t.Helper()
	controlPlaneStore, err := NewPostgresControlPlaneStore(t.Context(), storeTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controlPlaneStore.Close)
	return controlPlaneStore
}

func TestReadMetricsSnapshotAggregatesOperationDurations(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `DELETE FROM secondbox.operations`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	for index, fixture := range []struct {
		kind     string
		state    string
		duration time.Duration
	}{
		{kind: "create", state: contracts.OperationStateSucceeded, duration: 250 * time.Millisecond},
		{kind: "create", state: contracts.OperationStateSucceeded, duration: time.Second},
		{kind: "start", state: contracts.OperationStateFailed, duration: 10 * time.Millisecond},
	} {
		completedAt := base.Add(fixture.duration)
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			INSERT INTO secondbox.operations (
				id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,
				request_id,request_metadata_json,error_code,error_message,retryable,
				created_at,started_at,completed_at,updated_at
			) VALUES ($1,'metrics-tenant','metrics-subject',$2,'',$3,$4,
				'metrics-request','{}','','',false,$5,$5,$6,$6)`,
			fmt.Sprintf("op_metrics_%d", index),
			fmt.Sprintf("sbx_metrics_%d", index),
			fixture.kind, fixture.state, base, completedAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := controlPlaneStore.ReadMetricsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.OperationDurations) != 2 {
		t.Fatalf("Operation duration series = %#v, want 2", snapshot.OperationDurations)
	}
	create := snapshot.OperationDurations[0]
	if create.Kind != "create" ||
		create.TerminalState != contracts.OperationStateSucceeded ||
		create.Histogram.Count != 2 ||
		create.Histogram.BucketCounts[5] != 1 ||
		create.Histogram.BucketCounts[7] != 2 {
		t.Fatalf("create duration histogram = %#v", create)
	}
	start := snapshot.OperationDurations[1]
	if start.Kind != "start" ||
		start.TerminalState != contracts.OperationStateFailed ||
		start.Histogram.Count != 1 ||
		start.Histogram.BucketCounts[1] != 1 {
		t.Fatalf("start duration histogram = %#v", start)
	}
}

func TestPostgresStoreRevisionConflictAndIdempotencyReplay(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	profile := contracts.Profile{
		Name: "store-idempotency", State: contracts.ProfileStateEnabled, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
		CurrentRevision: contracts.ProfileRevision{
			ID: "prv_store_idempotency_1", Number: 1, CreatedAt: now,
		},
	}
	idempotency := ports.AdminIdempotencyInput{
		TenantRef: "tenant", SubjectRef: "subject", Operation: "profile.create",
		TargetID: profile.Name, Key: "store-create-profile", RequestHash: "request-a",
		Now: now, Ends: now.Add(time.Hour),
	}
	created, result, err := controlPlaneStore.CreateProfile(t.Context(), profile, idempotency)
	if err != nil || result.Replayed || created.Name != profile.Name {
		t.Fatalf("create Profile = %#v replay=%v error=%v", created, result.Replayed, err)
	}
	replayed, result, err := controlPlaneStore.CreateProfile(t.Context(), profile, idempotency)
	if err != nil || !result.Replayed || replayed.CurrentRevision.ID != profile.CurrentRevision.ID {
		t.Fatalf("replay Profile = %#v replay=%v error=%v", replayed, result.Replayed, err)
	}
	conflicting := idempotency
	conflicting.RequestHash = "request-b"
	if _, _, err := controlPlaneStore.CreateProfile(t.Context(), profile, conflicting); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	if _, _, err := controlPlaneStore.ReviseProfile(
		t.Context(), profile.Name,
		contracts.ProfileRevision{ID: "prv_store_idempotency_2", CreatedAt: now},
		99, now, ports.AdminIdempotencyInput{},
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("revision conflict error = %v", err)
	}
}

func TestPostgresStoreGarbageListingAndCompletion(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	_, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.artifacts (
			id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,
			size_bytes,sha256,storage_key,state,metadata_json,retain_until,created_at,
			garbage_collection_marked_at
		) VALUES (
			'art_store_gc','tenant','subject','sandbox',1,'result','application/octet-stream',
			4,$1,'artifacts/art_store_gc','garbage_pending','{}',$2,$2,$3
		)`,
		strings.Repeat("a", 64), now.Add(-2*time.Hour), now.Add(-2*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := controlPlaneStore.ListGarbageObjectsDue(
		t.Context(), now, time.Hour, 10,
	)
	if err != nil || len(objects) != 1 || objects[0].ID != "art_store_gc" {
		t.Fatalf("garbage objects = %#v error=%v", objects, err)
	}
	if err := controlPlaneStore.CompleteGarbageObject(t.Context(), objects[0], now); err != nil {
		t.Fatal(err)
	}
	var state string
	var collectedAt time.Time
	if err := controlPlaneStore.pool.QueryRow(
		t.Context(),
		`SELECT state,garbage_collected_at FROM secondbox.artifacts WHERE id='art_store_gc'`,
	).Scan(&state, &collectedAt); err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || !collectedAt.Equal(now) {
		t.Fatalf("garbage completion state=%q collectedAt=%s", state, collectedAt)
	}
}
