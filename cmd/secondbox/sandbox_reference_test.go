package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func referenceSandboxJSON(id, state string, deletedAt string) string {
	value := map[string]any{
		"id": id, "profile": "durable-coding", "profileRevisionId": "prv_1",
		"state": state, "desiredState": "running", "generation": 2,
		"workspace": map[string]any{
			"id": "wsp_" + id, "generation": 2, "state": "ready", "sizeBytes": 1024,
			"createdAt": "2026-07-28T00:00:00Z", "updatedAt": "2026-07-28T00:00:00Z",
		},
		"metadata":  map[string]string{contracts.SandboxNameMetadataKey: "my-box"},
		"revision":  1,
		"createdAt": "2026-07-28T00:00:00Z", "updatedAt": "2026-07-28T00:00:00Z",
	}
	if deletedAt != "" {
		value["deletedAt"] = deletedAt
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// referenceServer answers listSandboxes with scripted pages and getSandbox by path.
type referenceServer struct {
	server  *httptest.Server
	queries []string
	pages   []string
	served  int
}

func newReferenceServer(t *testing.T, pages ...string) *referenceServer {
	t.Helper()
	recorder := &referenceServer{pages: pages}
	recorder.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if request.URL.Path != "/v1/sandboxes" {
				_, _ = io.WriteString(writer, referenceSandboxJSON("sbx_direct", "ready", ""))
				return
			}
			recorder.queries = append(recorder.queries, request.URL.RawQuery)
			if recorder.served >= len(recorder.pages) {
				_, _ = io.WriteString(writer, `{"items":[]}`)
				return
			}
			page := recorder.pages[recorder.served]
			recorder.served++
			_, _ = io.WriteString(writer, page)
		},
	))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (recorder *referenceServer) client(t *testing.T) *secondboxclient.Client {
	t.Helper()
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		recorder.server.URL, "token-1", "tenant-1", "subject-1", recorder.server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestResolveSandboxReferenceUsesThePrefixToAvoidALookup proves an identifier
// is fetched directly rather than searched for by name.
func TestResolveSandboxReferenceUsesThePrefixToAvoidALookup(t *testing.T) {
	recorder := newReferenceServer(t)
	handle, err := resolveSandboxReference(
		context.Background(), recorder.client(t), "sbx_direct",
	)
	if err != nil {
		t.Fatal(err)
	}
	if handle.Snapshot().ID != "sbx_direct" {
		t.Errorf("id = %q", handle.Snapshot().ID)
	}
	if len(recorder.queries) != 0 {
		t.Errorf("queries = %v; an identifier must not trigger a name search", recorder.queries)
	}
}

func TestResolveSandboxReferenceFindsALiveNamedSandbox(t *testing.T) {
	page := fmt.Sprintf(`{"items":[%s]}`, referenceSandboxJSON("sbx_named", "ready", ""))
	recorder := newReferenceServer(t, page)
	handle, err := resolveSandboxReference(context.Background(), recorder.client(t), "my-box")
	if err != nil {
		t.Fatal(err)
	}
	if handle.Snapshot().ID != "sbx_named" {
		t.Errorf("id = %q", handle.Snapshot().ID)
	}
	if len(recorder.queries) != 1 {
		t.Fatalf("queries = %v; want one name search", recorder.queries)
	}
	query := recorder.queries[0]
	if !strings.Contains(query, "metadata=secondbox.dev%2Fname%3Dmy-box") {
		t.Errorf("query = %q; want the reserved name filter", query)
	}
}

// TestResolveSandboxReferenceSkipsDeletedPredecessors proves a recycled name
// resolves to the live Sandbox, not to a deleted one that still lists.
func TestResolveSandboxReferenceSkipsDeletedPredecessors(t *testing.T) {
	page := fmt.Sprintf(`{"items":[%s,%s,%s]}`,
		referenceSandboxJSON("sbx_gone1", "deleted", "2026-07-28T01:00:00Z"),
		referenceSandboxJSON("sbx_gone2", "deleted", "2026-07-28T02:00:00Z"),
		referenceSandboxJSON("sbx_live", "ready", ""),
	)
	recorder := newReferenceServer(t, page)
	handle, err := resolveSandboxReference(context.Background(), recorder.client(t), "my-box")
	if err != nil {
		t.Fatal(err)
	}
	if handle.Snapshot().ID != "sbx_live" {
		t.Errorf("id = %q; want the live Sandbox", handle.Snapshot().ID)
	}
}

func TestResolveSandboxReferencePagesPastDeletedPredecessors(t *testing.T) {
	cursor := "cursor-1"
	first := fmt.Sprintf(`{"items":[%s],"nextCursor":%q}`,
		referenceSandboxJSON("sbx_gone", "deleted", "2026-07-28T01:00:00Z"), cursor)
	second := fmt.Sprintf(`{"items":[%s]}`, referenceSandboxJSON("sbx_live", "ready", ""))
	recorder := newReferenceServer(t, first, second)
	handle, err := resolveSandboxReference(context.Background(), recorder.client(t), "my-box")
	if err != nil {
		t.Fatal(err)
	}
	if handle.Snapshot().ID != "sbx_live" {
		t.Errorf("id = %q", handle.Snapshot().ID)
	}
	if len(recorder.queries) != 2 || !strings.Contains(recorder.queries[1], "cursor=cursor-1") {
		t.Errorf("queries = %v; want the second page requested with the cursor", recorder.queries)
	}
}

func TestResolveSandboxReferenceReportsAnUnknownName(t *testing.T) {
	recorder := newReferenceServer(t, `{"items":[]}`)
	_, err := resolveSandboxReference(context.Background(), recorder.client(t), "absent")
	if err == nil || !strings.Contains(err.Error(), `found no Sandbox named "absent"`) {
		t.Fatalf("error = %v; want an unknown-name report", err)
	}
}

// TestResolveSandboxReferenceReportsAnOnlyDeletedName proves a name whose
// Sandboxes are all deleted is reported as absent rather than resolved.
func TestResolveSandboxReferenceReportsAnOnlyDeletedName(t *testing.T) {
	page := fmt.Sprintf(`{"items":[%s]}`,
		referenceSandboxJSON("sbx_gone", "deleted", "2026-07-28T01:00:00Z"))
	recorder := newReferenceServer(t, page)
	_, err := resolveSandboxReference(context.Background(), recorder.client(t), "my-box")
	if err == nil || !strings.Contains(err.Error(), "found no Sandbox named") {
		t.Fatalf("error = %v; want an absent report", err)
	}
}

func TestResolveSandboxReferenceStopsAtItsPageBound(t *testing.T) {
	pages := make([]string, 0, nameResolutionPageBound)
	for index := range nameResolutionPageBound {
		pages = append(pages, fmt.Sprintf(`{"items":[%s],"nextCursor":%q}`,
			referenceSandboxJSON("sbx_gone", "deleted", "2026-07-28T01:00:00Z"),
			fmt.Sprintf("cursor-%d", index)))
	}
	recorder := newReferenceServer(t, pages...)
	_, err := resolveSandboxReference(context.Background(), recorder.client(t), "my-box")
	if err == nil || !strings.Contains(err.Error(), "supply the Sandbox identifier") {
		t.Fatalf("error = %v; want an explicit give-up naming the workaround", err)
	}
	if recorder.served != nameResolutionPageBound {
		t.Errorf("pages served = %d; want the bound honoured", recorder.served)
	}
}

func TestResolveSandboxReferenceRequiresAReference(t *testing.T) {
	recorder := newReferenceServer(t)
	if _, err := resolveSandboxReference(context.Background(), recorder.client(t), ""); err == nil {
		t.Fatal("an empty reference must be rejected")
	}
}

func TestLiveSandboxIgnoresDeletedRepresentations(t *testing.T) {
	var live, deletedState, deletedStamp secondboxclient.Sandbox
	if err := json.Unmarshal([]byte(referenceSandboxJSON("sbx_a", "ready", "")), &live); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(
		[]byte(referenceSandboxJSON("sbx_b", "deleted", "")), &deletedState,
	); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(
		[]byte(referenceSandboxJSON("sbx_c", "ready", "2026-07-28T01:00:00Z")), &deletedStamp,
	); err != nil {
		t.Fatal(err)
	}
	if !liveSandbox(live) {
		t.Error("a ready Sandbox is live")
	}
	if liveSandbox(deletedState) {
		t.Error("a Sandbox in the deleted state is not live")
	}
	if liveSandbox(deletedStamp) {
		t.Error("a Sandbox carrying a deletion timestamp is not live")
	}
}

// TestResolveSandboxReferenceRejectsAResponseWithoutASandbox keeps a malformed
// response legible instead of surfacing as a missing path parameter later.
func TestResolveSandboxReferenceRejectsAResponseWithoutASandbox(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{}`)
		}))
	t.Cleanup(server.Close)
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		server.URL, "token-1", "tenant-1", "subject-1", server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveSandboxReference(context.Background(), client, "sbx_absent")
	if err == nil || !strings.Contains(err.Error(), "received no Sandbox") {
		t.Fatalf("error = %v; want a legible empty-response rejection", err)
	}
}
