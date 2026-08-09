package secondboxclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

func TestHighLevelSandboxAdoptionListingAndMetadataFencing(t *testing.T) {
	var observedMetadata []string
	var observedIfMatch string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes":
			observedMetadata = request.URL.Query()["metadata"]
			_, _ = io.WriteString(writer, `{"items":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes/sandbox-1":
			_, _ = io.WriteString(writer, sandboxResourceJSON(7))
		case request.Method == http.MethodPut && request.URL.Path == "/v1/sandboxes/sandbox-1/metadata":
			observedIfMatch = request.Header.Get("If-Match")
			_, _ = io.WriteString(writer, sandboxResourceJSON(8))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewSecondBoxSubjectClient(server.URL, "token", "tenant", "subject", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListSandboxes(t.Context(), SandboxListOptions{
		PageOptions: PageOptions{Limit: 10}, Metadata: Metadata{"z": "last", "a": "first"},
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observedMetadata, []string{"a=first", "z=last"}) {
		t.Fatalf("metadata query = %#v", observedMetadata)
	}
	handle, err := client.AdoptSandbox(t.Context(), "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := handle.UpdateMetadata(t.Context(), Metadata{"owner": "application"})
	if err != nil {
		t.Fatal(err)
	}
	if observedIfMatch != `"revision-7"` || updated.Revision != 8 || handle.Snapshot().Revision != 8 {
		t.Fatalf("metadata fence/result = %q/%d/%d", observedIfMatch, updated.Revision, handle.Snapshot().Revision)
	}
}

func TestPageOptionsRejectUnboundedValues(t *testing.T) {
	client, err := NewSecondBoxSubjectClient("https://secondbox.example", "token", "tenant", "subject", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListProfiles(context.Background(), PageOptions{Limit: 201}); err == nil {
		t.Fatal("invalid page limit unexpectedly reached transport")
	}
}

func sandboxResourceJSON(revision int) string {
	return `{"id":"sandbox-1","profile":"durable-coding","profileRevisionId":"profile-revision-1","metadata":{},"desiredState":"running","state":"ready","generation":3,"revision":` +
		strconv.Itoa(revision) + `,"workspace":{"id":"workspace-1","generation":3,"state":"ready","sizeBytes":0,"createdAt":"2026-08-03T00:00:00Z","updatedAt":"2026-08-03T00:00:00Z"},"createdAt":"2026-08-03T00:00:00Z","updatedAt":"2026-08-03T00:00:00Z"}`
}
