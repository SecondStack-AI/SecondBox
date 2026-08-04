package secondboxclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
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

func TestHighLevelArtifactsVerifyBoundsDigestAndMultipart(t *testing.T) {
	content := []byte("artifact content")
	sum := sha256.Sum256(content)
	var uploadFields map[string]string
	var uploadContent []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/artifacts/artifact-1/content":
			writer.Header().Set("Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
			_, _ = writer.Write(content)
		case "/v1/sandboxes/sandbox-1/artifacts":
			reader, err := request.MultipartReader()
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			uploadFields = map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				value, _ := io.ReadAll(part)
				if part.FormName() == "content" {
					uploadContent = value
				} else {
					uploadFields[part.FormName()] = string(value)
				}
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"artifact-1","sandboxId":"sandbox-1","generation":3,"name":"result","mediaType":"text/plain","sha256":"`+strings.Repeat("0", 64)+`","sizeBytes":16,"metadata":{},"createdAt":"2026-08-03T00:00:00Z","expiresAt":"2026-08-04T00:00:00Z"}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewSecondBoxSubjectClient(server.URL, "token", "tenant", "subject", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := client.DownloadArtifact(t.Context(), "artifact-1", int64(len(content)))
	if err != nil || string(downloaded) != string(content) {
		t.Fatalf("DownloadArtifact() = %q, %v", downloaded, err)
	}
	if _, err := client.DownloadArtifact(t.Context(), "artifact-1", int64(len(content)-1)); err == nil {
		t.Fatal("bounded download unexpectedly succeeded")
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 3, Revision: 2})
	if _, err := handle.UploadArtifact(t.Context(), "result", "text/plain", Metadata{"kind": "test"}, content, "", ""); err != nil {
		t.Fatal(err)
	}
	if string(uploadContent) != string(content) || uploadFields["name"] != "result" || uploadFields["mediaType"] != "text/plain" || uploadFields["sha256"] != hexDigest(sum[:]) {
		t.Fatalf("upload fields/content = %#v/%q", uploadFields, uploadContent)
	}
	var metadata Metadata
	if err := json.Unmarshal([]byte(uploadFields["metadata"]), &metadata); err != nil || metadata["kind"] != "test" {
		t.Fatalf("upload metadata = %#v, %v", metadata, err)
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

func hexDigest(value []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(value)*2)
	for index, item := range value {
		encoded[index*2] = digits[item>>4]
		encoded[index*2+1] = digits[item&15]
	}
	return string(encoded)
}
