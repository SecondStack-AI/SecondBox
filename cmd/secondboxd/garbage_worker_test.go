package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var garbageWorkerTestSequence atomic.Uint64

func TestGarbageCollectorWorkerReclaimsDueObject(t *testing.T) {
	databaseURL := openGarbageWorkerTestDatabase(t)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	content := []byte("expired Artifact")
	sum := sha256.Sum256(content)
	deleted := make(chan string, 1)
	objectServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPut:
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodHead:
			writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			writer.Header().Set("X-Amz-Meta-Sha256", hex.EncodeToString(sum[:]))
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if _, err := writer.Write(content); err != nil {
				t.Error(err)
			}
		case http.MethodDelete:
			deleted <- request.URL.Path
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(objectServer.Close)
	objects, err := objectstore.NewS3Store(t.Context(), objectstore.S3Config{
		Endpoint: objectServer.URL, Region: "us-east-1", Bucket: "secondbox",
		AccessKeyID: "garbage-test-access", SecretAccessKey: "garbage-test-secret",
		UsePathStyle: true, RetryMaxAttempts: 1, HTTPTimeout: time.Second,
		TempDirectory: t.TempDir(), MaxObjectBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	storageKey := "artifacts/art-garbage-worker"
	if _, err := objects.PutImmutable(
		t.Context(), storageKey, bytes.NewReader(content), int64(len(content)),
		hex.EncodeToString(sum[:]),
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.artifacts (
			id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,
			size_bytes,sha256,storage_key,state,metadata_json,retain_until,created_at,
			garbage_collection_marked_at
		) VALUES (
			'art-garbage-worker','tenant','subject','sandbox',1,'expired','application/octet-stream',
			$1,$2,$3,'garbage_pending','{}',$4,$4,$4
		)`,
		len(content), hex.EncodeToString(sum[:]), storageKey, now.Add(-2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	completed := make(chan error, 1)
	go func() {
		completed <- runGarbageCollector(
			ctx,
			lifecycle.GarbageCollector{
				Catalog: databaseStore, Objects: objects, Grace: time.Minute, BatchSize: 10,
			},
			time.Hour,
		)
	}()
	select {
	case path := <-deleted:
		if path != "/secondbox/"+storageKey {
			t.Fatalf("deleted object path = %q", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("garbage collector did not delete the due object")
	}
	var state string
	var garbageCollectedAt *time.Time
	for deadline := time.Now().Add(5 * time.Second); ; {
		err := pool.QueryRow(t.Context(), `
			SELECT state,garbage_collected_at FROM secondbox.artifacts
			WHERE id='art-garbage-worker'`,
		).Scan(&state, &garbageCollectedAt)
		if err == nil && state == "deleted" && garbageCollectedAt != nil {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("garbage collection state = %q at %v", state, garbageCollectedAt)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("garbage collector shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("garbage collector did not stop with process context")
	}
}

func openGarbageWorkerTestDatabase(t *testing.T) string {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required for garbage worker tests")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_garbage_worker_test_%d_%d",
		os.Getpid(), garbageWorkerTestSequence.Add(1),
	)
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		admin.Close(t.Context())
		t.Fatal(err)
	}
	parsed.Path = "/" + databaseName
	databaseURL := parsed.String()
	if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
		admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(t.Context())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop garbage worker test database: %v", err)
		}
		if err := admin.Close(context.Background()); err != nil {
			t.Errorf("close garbage worker test admin connection: %v", err)
		}
	})
	return databaseURL
}
