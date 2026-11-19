//go:build linux && scenario_live

package runnercontrol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestWorkspaceArchiveHTTPRealFilesystem(t *testing.T) {
	capacity := int64(64 << 20)
	large := os.Getenv("SECONDBOX_ARCHIVE_QUALIFICATION_LARGE") == "1"
	if large {
		capacity = 256 << 20
	}
	const workspaceID = "workspace-http-archive"
	newStore := func() (*workspacestore.Store, string) {
		root := t.TempDir()
		store, err := workspacestore.New(t.Context(), workspacestore.Config{
			Root: root, FormatterKind: workspacestore.FormatterMke2fs,
			TemplateCapacityBytes: capacity,
		})
		if err != nil {
			t.Fatalf("qualified reflink/ext4 store: %v", err)
		}
		return store, root
	}
	source, sourceRoot := newStore()
	target, _ := newStore()
	mutation := workspacestore.Mutation{
		OperationID: "create-http-archive", WorkspaceID: workspaceID, FencingToken: []byte("01234567890123456789012345678901"),
	}
	if _, err := source.Create(t.Context(), workspacestore.CreateWorkspaceRequest{
		Mutation: mutation, CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := source.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("uncommitted Code workspace data\n")
	input := filepath.Join(t.TempDir(), "journal")
	expectedHash := sha256.New()
	if large {
		file, err := os.OpenFile(input, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(io.MultiWriter(file, expectedHash), rand.Reader, 70<<20)
		if closeErr := file.Close(); copyErr != nil || closeErr != nil {
			t.Fatalf("large archive fixture: write=%v close=%v", copyErr, closeErr)
		}
	} else {
		if err := os.WriteFile(input, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = expectedHash.Write(payload)
	}
	command := exec.CommandContext(t.Context(), "debugfs", "-w", "-R", "write "+input+" /journal", "/proc/self/fd/3")
	command.ExtraFiles = []*os.File{attachment.Descriptor()}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write real ext4 workspace: %v %s", err, output)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var object []byte
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.Header.Get("X-Archive-Grant") != "trusted-test-grant" {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		if request.Method == http.MethodPut {
			if request.ContentLength <= 0 || request.ContentLength > 2<<20 {
				response.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			var err error
			object, err = io.ReadAll(io.LimitReader(request.Body, 2<<20))
			if err != nil || int64(len(object)) != request.ContentLength {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusOK)
			return
		}
		response.Header().Set("Content-Length", strconv.Itoa(len(object)))
		_, _ = response.Write(object)
	}))
	defer server.Close()
	grant := &runnerprotocol.WorkspaceArchiveTransfer{
		Url: server.URL, Headers: map[string]string{"X-Archive-Grant": "trusted-test-grant"},
		ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}
	qualificationEndpoint := os.Getenv("SECONDBOX_ARCHIVE_GRANT_ENDPOINT")
	if qualificationEndpoint != "" {
		grant = requestWorkspaceArchiveQualificationGrant(t, qualificationEndpoint, http.MethodPut, nil)
		t.Cleanup(func() { requestWorkspaceArchiveQualificationGrant(t, qualificationEndpoint, http.MethodDelete, nil) })
	}
	export := &runnerprotocol.LocalWorkspaceCommand{
		OperationId: "export-http-archive", WorkspaceId: workspaceID,
		ExpectedGeneration: 1, LogicalCapacityBytes: uint64(capacity),
		FencingToken: mutation.FencingToken, ArchiveTransfer: grant,
		Kind: runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT,
	}
	evidence, err := ExecuteWorkspaceArchive(t.Context(), export, source)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ArchiveDescriptor == nil || evidence.ReceiptRecordedAt.IsZero() {
		t.Fatalf("upload lacks descriptor or seal receipt: %+v", evidence)
	}
	if entries, err := os.ReadDir(filepath.Join(sourceRoot, "archive-staging")); err != nil || len(entries) != 0 {
		t.Fatal("archive upload left staging bytes")
	}
	if qualificationEndpoint == "" {
		grant.ArchiveDescriptor = evidence.ArchiveDescriptor
	} else {
		grant = requestWorkspaceArchiveQualificationGrant(t, qualificationEndpoint, http.MethodGet, evidence.ArchiveDescriptor)
		export.ArchiveTransfer = grant
	}
	export.OperationId = "import-http-archive"
	export.Kind = runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT
	if _, err := ExecuteWorkspaceArchive(t.Context(), export, target); err != nil {
		t.Fatal(err)
	}
	restored, err := target.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	read := exec.CommandContext(t.Context(), "debugfs", "-R", "cat /journal", "/proc/self/fd/3")
	read.ExtraFiles = []*os.File{restored.Descriptor()}
	restoredHash := sha256.New()
	read.Stdout = restoredHash
	if err := read.Run(); err != nil || !bytes.Equal(restoredHash.Sum(nil), expectedHash.Sum(nil)) {
		t.Fatalf("real filesystem restore data checksum mismatch or read failure: %v", err)
	}
	if large && evidence.ArchiveDescriptor.ArchiveBytes <= 64<<20 {
		t.Fatal("large qualification did not exercise multiple S3 upload parts")
	}
}

func requestWorkspaceArchiveQualificationGrant(t *testing.T, endpoint, method string, descriptor *runnerprotocol.WorkspaceArchiveDescriptor) *runnerprotocol.WorkspaceArchiveTransfer {
	t.Helper()
	input := struct {
		Method     string          `json:"method"`
		Descriptor json.RawMessage `json:"descriptor,omitempty"`
	}{Method: method}
	if descriptor != nil {
		encoded, err := protojson.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		input.Descriptor = encoded
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("AWS qualification grant request: %v", err)
	}
	defer response.Body.Close()
	if method == http.MethodDelete && response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("AWS qualification grant refused: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var grant runnerprotocol.WorkspaceArchiveTransfer
	if err := protojson.Unmarshal(body, &grant); err != nil {
		t.Fatal("AWS qualification provider returned an invalid grant")
	}
	return &grant
}
