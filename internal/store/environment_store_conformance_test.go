package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/internal/store"
	"secondstack/sandbox-service/internal/storeconformance"
)

func TestMemoryEnvironmentStoreConformance(t *testing.T) {
	storeconformance.RunEnvironmentStoreConformance(t, func(*testing.T) ports.EnvironmentStore {
		return store.NewMemoryEnvironmentStore(time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC))
	})
}

func TestPostgresEnvironmentStoreConformance(t *testing.T) {
	databaseURL := requireDisposableSandboxTestDatabaseURL(t)
	storeconformance.RunEnvironmentStoreConformance(t, func(t *testing.T) ports.EnvironmentStore {
		resetDisposableSandboxTestSchema(t, databaseURL)
		environmentStore, err := store.NewPostgresEnvironmentStore(context.Background(), databaseURL)
		if err != nil {
			t.Fatalf("NewPostgresEnvironmentStore() error = %v", err)
		}
		t.Cleanup(environmentStore.Close)
		return environmentStore
	})
}

func requireDisposableSandboxTestDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("SANDBOX_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("SANDBOX_SERVICE_TEST_DATABASE_URL is required and must target a disposable PostgreSQL database")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse SANDBOX_SERVICE_TEST_DATABASE_URL: %v", err)
	}
	databaseName := strings.ToLower(config.ConnConfig.Database)
	if !strings.Contains(databaseName, "test") && !strings.Contains(databaseName, "conformance") {
		t.Fatalf(
			"SANDBOX_SERVICE_TEST_DATABASE_URL database %q is not explicitly marked test or conformance",
			config.ConnConfig.Database,
		)
	}
	return databaseURL
}

func resetDisposableSandboxTestSchema(t *testing.T, databaseURL string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect disposable Sandbox test database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS sandbox CASCADE; CREATE SCHEMA sandbox"); err != nil {
		t.Fatalf("reset disposable Sandbox test schema: %v", err)
	}
	migrationPath := sandboxBaselineMigrationPath(t)
	migrationSQL, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read Sandbox baseline migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migrationSQL)); err != nil {
		t.Fatalf("apply Sandbox baseline migration: %v", err)
	}
}

func sandboxBaselineMigrationPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve EnvironmentStore conformance test path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations", "0001_sandbox_baseline.sql")
}
