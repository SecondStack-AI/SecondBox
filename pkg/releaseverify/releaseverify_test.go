package releaseverify

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPFetcherRejectsNonPublicLocation(t *testing.T) {
	_, err := HTTPFetcher(http.DefaultClient)(context.Background(), "http://example.com/release-index.json")
	if err == nil || !strings.Contains(err.Error(), "public HTTPS") {
		t.Fatalf("error = %v", err)
	}
}

func TestStrictTopLevelDocuments(t *testing.T) {
	fetch := func(context.Context, string) ([]byte, error) { return []byte(`{"schemaVersion":"wrong"}`), nil }
	if _, err := ArtifactManifest(context.Background(), "https://example.com/manifest", fetch); err == nil {
		t.Fatal("malformed artifact manifest was accepted")
	}
	if _, err := FinalRelease(context.Background(), "https://example.com/index", fetch); err == nil {
		t.Fatal("malformed release index was accepted")
	}
}
