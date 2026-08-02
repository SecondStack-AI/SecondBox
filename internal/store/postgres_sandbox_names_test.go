package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/pagination"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var sandboxNameTestBase = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// insertSandboxFixture writes one Sandbox and its Workspace directly so list
// filtering and the reserved-name index are exercised without the full
// creation path. It returns the insert error rather than failing, because
// several cases assert that the insert is rejected.
func insertSandboxFixture(
	t *testing.T,
	controlPlaneStore *PostgresControlPlaneStore,
	id string,
	tenantRef string,
	subjectRef string,
	metadata map[string]string,
	createdAt time.Time,
	deletedAt *time.Time,
) error {
	t.Helper()
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := "wsp_" + id
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'runner-1','ready',1024,1,'','','','','','{}',$5,$5)`,
		workspaceID, tenantRef, subjectRef, id, createdAt,
	); err != nil {
		t.Fatal(err)
	}
	_, err = controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at,deleted_at
		) VALUES ($1,$2,$3,'profile-1','prv_1','ready','running',1,$4,'',$5,'{}',1,$6,$6,$7)`,
		id, tenantRef, subjectRef, workspaceID, metadataJSON, createdAt, deletedAt,
	)
	return err
}

func sandboxNameScope(t *testing.T) (string, string) {
	t.Helper()
	suffix := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	return "tenant-" + suffix, "subject-" + suffix
}

func listedSandboxIDs(page contracts.SandboxPage) []string {
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestListSandboxesFiltersByMetadataContainment(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	fixtures := []struct {
		id       string
		metadata map[string]string
	}{
		{"sbx_filter_a", map[string]string{
			contracts.SandboxNameMetadataKey: "alpha", "tier": "gold",
		}},
		{"sbx_filter_b", map[string]string{
			contracts.SandboxNameMetadataKey: "beta", "tier": "gold",
		}},
		{"sbx_filter_c", map[string]string{"tier": "silver"}},
	}
	for index, fixture := range fixtures {
		if err := insertSandboxFixture(
			t, controlPlaneStore, fixture.id, tenantRef, subjectRef, fixture.metadata,
			sandboxNameTestBase.Add(time.Duration(index)*time.Minute), nil,
		); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		filter map[string]string
		want   []string
	}{
		{"no filter", nil, []string{"sbx_filter_a", "sbx_filter_b", "sbx_filter_c"}},
		{
			"reserved name resolves one Sandbox",
			map[string]string{contracts.SandboxNameMetadataKey: "alpha"},
			[]string{"sbx_filter_a"},
		},
		{"shared pair matches many", map[string]string{"tier": "gold"},
			[]string{"sbx_filter_a", "sbx_filter_b"}},
		{
			"pairs combine with AND",
			map[string]string{"tier": "gold", contracts.SandboxNameMetadataKey: "beta"},
			[]string{"sbx_filter_b"},
		},
		{"contradictory pairs match nothing",
			map[string]string{"tier": "gold", "missing": "value"}, []string{}},
		{"unknown name matches nothing",
			map[string]string{contracts.SandboxNameMetadataKey: "absent"}, []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := controlPlaneStore.ListSandboxes(
				t.Context(), tenantRef, subjectRef, 100, "", test.filter,
			)
			if err != nil {
				t.Fatal(err)
			}
			got := listedSandboxIDs(page)
			if len(got) != len(test.want) {
				t.Fatalf("ids = %v; want %v", got, test.want)
			}
			for index := range test.want {
				if got[index] != test.want[index] {
					t.Fatalf("ids = %v; want %v", got, test.want)
				}
			}
		})
	}
}

// TestListSandboxesBindsCursorToItsFilter proves a page cursor cannot leak
// across filter scopes, which would silently skip or repeat rows.
func TestListSandboxesBindsCursorToItsFilter(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	for index, id := range []string{"sbx_scope_a", "sbx_scope_b", "sbx_scope_c"} {
		if err := insertSandboxFixture(
			t, controlPlaneStore, id, tenantRef, subjectRef,
			map[string]string{"tier": "gold"},
			sandboxNameTestBase.Add(time.Duration(index)*time.Minute), nil,
		); err != nil {
			t.Fatal(err)
		}
	}
	unfiltered, err := controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 2, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unfiltered.NextCursor == nil {
		t.Fatal("an unfiltered first page must produce a cursor")
	}
	_, err = controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 2, *unfiltered.NextCursor,
		map[string]string{"tier": "gold"},
	)
	if !errors.Is(err, pagination.ErrInvalidListCursor) {
		t.Fatalf("error = %v; want an invalid-cursor rejection across filters", err)
	}

	filtered, err := controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 2, "", map[string]string{"tier": "gold"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.NextCursor == nil {
		t.Fatal("a filtered first page must produce a cursor")
	}
	_, err = controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 2, *filtered.NextCursor, nil,
	)
	if !errors.Is(err, pagination.ErrInvalidListCursor) {
		t.Fatalf("error = %v; want the reverse direction rejected too", err)
	}
}

func TestListSandboxesPagesWithinOneFilter(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	for index, id := range []string{"sbx_page_a", "sbx_page_b", "sbx_page_c"} {
		metadata := map[string]string{"tier": "gold"}
		if id == "sbx_page_b" {
			metadata = map[string]string{"tier": "silver"}
		}
		if err := insertSandboxFixture(
			t, controlPlaneStore, id, tenantRef, subjectRef, metadata,
			sandboxNameTestBase.Add(time.Duration(index)*time.Minute), nil,
		); err != nil {
			t.Fatal(err)
		}
	}
	filter := map[string]string{"tier": "gold"}
	first, err := controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 1, "", filter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := listedSandboxIDs(first); len(got) != 1 || got[0] != "sbx_page_a" {
		t.Fatalf("first page = %v; want the earliest matching Sandbox", got)
	}
	if first.NextCursor == nil {
		t.Fatal("a bounded filtered page must produce a cursor")
	}
	second, err := controlPlaneStore.ListSandboxes(
		t.Context(), tenantRef, subjectRef, 1, *first.NextCursor, filter,
	)
	if err != nil {
		t.Fatal(err)
	}
	// The non-matching Sandbox created between them is skipped, not paged over.
	if got := listedSandboxIDs(second); len(got) != 1 || got[0] != "sbx_page_c" {
		t.Fatalf("second page = %v; want the next matching Sandbox", got)
	}
}

func TestReservedSandboxNameIsUniquePerSubject(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	named := map[string]string{contracts.SandboxNameMetadataKey: "my-box"}
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_unique_a", tenantRef, subjectRef, named,
		sandboxNameTestBase, nil,
	); err != nil {
		t.Fatal(err)
	}
	err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_unique_b", tenantRef, subjectRef, named,
		sandboxNameTestBase.Add(time.Minute), nil,
	)
	if err == nil {
		t.Fatal("a repeated reserved name must be rejected")
	}
	if !isSandboxNameConflict(err) {
		t.Fatalf("error = %v; want a recognised reserved-name conflict", err)
	}
}

// TestUnnamedSandboxesDoNotCollide proves the partial index ignores Sandboxes
// that carry no reserved name.
func TestUnnamedSandboxesDoNotCollide(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	for index, id := range []string{"sbx_unnamed_a", "sbx_unnamed_b"} {
		if err := insertSandboxFixture(
			t, controlPlaneStore, id, tenantRef, subjectRef, map[string]string{"tier": "gold"},
			sandboxNameTestBase.Add(time.Duration(index)*time.Minute), nil,
		); err != nil {
			t.Fatalf("unnamed Sandbox %s was rejected: %v", id, err)
		}
	}
}

func TestDeletedSandboxReleasesItsName(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	named := map[string]string{contracts.SandboxNameMetadataKey: "recycled"}
	deletedAt := sandboxNameTestBase.Add(time.Hour)
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_recycled_old", tenantRef, subjectRef, named,
		sandboxNameTestBase, &deletedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_recycled_new", tenantRef, subjectRef, named,
		sandboxNameTestBase.Add(2*time.Hour), nil,
	); err != nil {
		t.Fatalf("a deleted Sandbox must release its name: %v", err)
	}
}

func TestSandboxNamesAreScopedToTheirSubject(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	tenantRef, subjectRef := sandboxNameScope(t)
	named := map[string]string{contracts.SandboxNameMetadataKey: "shared"}
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_scoped_a", tenantRef, subjectRef, named,
		sandboxNameTestBase, nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_scoped_b", tenantRef, subjectRef+"-other", named,
		sandboxNameTestBase, nil,
	); err != nil {
		t.Fatalf("another subject must be free to use the same name: %v", err)
	}
	if err := insertSandboxFixture(
		t, controlPlaneStore, "sbx_scoped_c", tenantRef+"-other", subjectRef, named,
		sandboxNameTestBase, nil,
	); err != nil {
		t.Fatalf("another tenant must be free to use the same name: %v", err)
	}
}

func TestEncodeSandboxMetadataFilterIsStable(t *testing.T) {
	first, err := encodeSandboxMetadataFilter(map[string]string{"b": "2", "a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeSandboxMetadataFilter(map[string]string{"a": "1", "b": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("encodings differ: %s vs %s", first, second)
	}
	if string(first) != `{"a":"1","b":"2"}` {
		t.Errorf("encoding = %s; want sorted keys", first)
	}
	empty, err := encodeSandboxMetadataFilter(nil)
	if err != nil || empty != nil {
		t.Errorf("empty filter = %v, %v; want nil with no error", empty, err)
	}
}
