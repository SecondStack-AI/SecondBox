package postgresmigrations

import (
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"testing"
)

func TestSandboxResourcesBackfillUsesPinnedRevision(t *testing.T) {
	connection := newGuardDatabase(t)
	lineage, err := readEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range lineage[1:] {
		if item.filename == "0023_sandbox_resources.sql" {
			break
		}
		applyMigrations(t, connection, item.filename)
	}
	insertGuardSandbox(t, connection, "sbx_resources_old", "tenant-resources", "subject-resources", "old", false)
	insertGuardSandbox(t, connection, "sbx_resources_deleted", "tenant-resources", "subject-resources", "deleted", true)
	if _, err := connection.Exec(t.Context(), `
  INSERT INTO secondbox.profile_revisions(id,profile_name,revision_number,spec_json,created_at)
  VALUES ('prv_1','profile-1',1,'{"resources":{"vcpuCount":2,"memoryBytes":2147483648,"workspaceBytes":4294967296}}',now()),
         ('prv_2','profile-1',2,'{"resources":{"vcpuCount":8,"memoryBytes":8589934592,"workspaceBytes":17179869184}}',now())
 `); err != nil {
		t.Fatal(err)
	}
	applyMigrations(t, connection, "0023_sandbox_resources.sql")
	for _, id := range []string{"sbx_resources_old", "sbx_resources_deleted"} {
		var got contracts.SandboxResources
		if err := connection.QueryRow(t.Context(), `SELECT vcpu_count,memory_bytes,workspace_bytes FROM secondbox.sandboxes WHERE id=$1`, id).Scan(&got.VCPUCount, &got.MemoryBytes, &got.WorkspaceBytes); err != nil {
			t.Fatal(err)
		}
		want := contracts.SandboxResources{VCPUCount: 2, MemoryBytes: 2 << 30, WorkspaceBytes: 4 << 30}
		if got != want {
			t.Fatalf("%s resources = %+v, want %+v", id, got, want)
		}
	}
	var nullable, defaults int
	if err := connection.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE is_nullable='YES'),count(*) FILTER (WHERE column_default IS NOT NULL) FROM information_schema.columns WHERE table_schema='secondbox' AND table_name='sandboxes' AND column_name IN ('vcpu_count','memory_bytes','workspace_bytes')`).Scan(&nullable, &defaults); err != nil {
		t.Fatal(err)
	}
	if nullable != 0 || defaults != 0 {
		t.Fatalf("resources nullable=%d defaults=%d", nullable, defaults)
	}
}
