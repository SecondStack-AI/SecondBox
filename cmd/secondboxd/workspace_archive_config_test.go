package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
)

func TestWorkspaceArchiveStartupRefusesDisabledTierWithPendingGC(t *testing.T) {
	rawURL := os.Getenv("SECONDBOX_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Fatal("SECONDBOX_TEST_DATABASE_URL is required for archive startup regression")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(t.Context())
	name := fmt.Sprintf("secondbox_archive_startup_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	parsed.Path = "/" + name
	if err := postgresmigrations.Apply(t.Context(), parsed.String()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgx.Connect(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close(t.Context())
	if _, err := pool.Exec(t.Context(), `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES('pending-object',now())`); err != nil {
		t.Fatal(err)
	}
	storage, err := store.NewPostgresControlPlaneStore(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	_, err = configureWorkspaceArchives(t.Context(), "", storage)
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("startup with disabled archive tier and pending object GC: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM secondbox.archive_cleanup`); err != nil {
		t.Fatal(err)
	}
	if _, err := configureWorkspaceArchives(t.Context(), "", storage); err != nil {
		t.Fatalf("settled archive tier cannot be disabled: %v", err)
	}
}
