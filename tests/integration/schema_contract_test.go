package integration_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		"operators", "operator_credentials", "projects", "service_accounts", "api_keys", "profiles", "profile_revisions",
		"runner_pools", "runners", "sandboxes", "workspaces", "instances",
		"assignments", "leases", "snapshots", "artifacts",
		"port_sessions", "data_plane_sessions", "data_plane_frames",
		"operations", "idempotency_records", "project_quotas", "profile_quotas", "audit_events",
	} {
		if !strings.Contains(sql, "create table secondbox."+table) {
			t.Errorf("SecondBox baseline is missing secondbox.%s", table)
		}
	}
	for _, forbidden := range []string{
		"foreign key", " references ", " check ", "tenant_ref", "subject_ref",
		"resource_classes", "lifecycle_policies", "agent_service",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("SecondBox baseline contains removed or forbidden schema fragment %q", forbidden)
		}
	}
}
