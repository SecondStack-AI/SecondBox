package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBaselineDefinesCompleteLogicalSchemaWithoutPhysicalRelationships(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve schema test path")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations", "0001_sandbox_baseline.sql")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(content))
	for _, table := range []string{
		"resource_classes", "lifecycle_policies", "workspaces", "environments",
		"instances", "leases", "snapshots", "artifacts",
	} {
		if !strings.Contains(sql, "create table sandbox."+table) {
			t.Errorf("baseline is missing sandbox.%s", table)
		}
	}
	for _, forbidden := range []string{"foreign key", " references ", " check "} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("baseline contains forbidden database constraint %q", forbidden)
		}
	}
	for _, seed := range []string{"agent-compartment", "coding-environment"} {
		if !strings.Contains(sql, seed) {
			t.Errorf("baseline is missing lifecycle policy %s", seed)
		}
	}
}
