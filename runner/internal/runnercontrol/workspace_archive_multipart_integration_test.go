//go:build linux && scenario_live

package runnercontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func TestWorkspaceArchiveMultipartCompletionAndAbort(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprintf("completion-error-%t", rejected), func(t *testing.T) {
			store, err := workspacestore.New(t.Context(), workspacestore.Config{
				Root: t.TempDir(), FormatterKind: workspacestore.FormatterMke2fs, TemplateCapacityBytes: 64 << 20,
			})
			if err != nil {
				t.Fatal(err)
			}
			mutation := workspacestore.Mutation{OperationID: "create-multipart", WorkspaceID: "multipart-workspace", FencingToken: bytes.Repeat([]byte{1}, 32)}
			if _, err := store.Create(t.Context(), workspacestore.CreateWorkspaceRequest{Mutation: mutation, CapacityBytes: 64 << 20}); err != nil {
				t.Fatal(err)
			}
			mutation.OperationID = "export-multipart"
			upload, err := store.OpenWorkspaceArchiveUpload(t.Context(), workspacestore.RelocationExportRequest{Mutation: mutation, ExpectedGeneration: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer upload.Close()
			var content bytes.Buffer
			parts, completed, aborted := 0, false, false
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch {
				case strings.HasPrefix(request.URL.Path, "/part/"):
					parts++
					if request.Method != http.MethodPut || request.ContentLength <= 0 || request.ContentLength > 1024 {
						response.WriteHeader(http.StatusBadRequest)
						return
					}
					if _, err := io.Copy(&content, io.LimitReader(request.Body, 1024)); err != nil {
						t.Error(err)
					}
					response.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, parts))
				case request.URL.Path == "/complete":
					completed = true
					var completion workspaceArchiveMultipartCompletion
					if err := xml.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&completion); err != nil || len(completion.Parts) != parts {
						t.Errorf("multipart completion: parts=%d payload=%+v error=%v", parts, completion, err)
					}
					for index, part := range completion.Parts {
						if part.Number != index+1 || part.ETag != fmt.Sprintf(`"part-%d"`, index+1) {
							t.Errorf("unordered multipart completion: %+v", part)
						}
					}
					if rejected {
						_, _ = io.WriteString(response, "<Error><Code>InternalError</Code></Error>")
					} else {
						_, _ = io.WriteString(response, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
					}
				case request.URL.Path == "/abort":
					aborted = true
					response.WriteHeader(http.StatusNoContent)
				default:
					response.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			grant := &runnerprotocol.WorkspaceArchiveTransfer{PartSizeBytes: 1024, CompleteUrl: server.URL + "/complete", AbortUrl: server.URL + "/abort"}
			for index := 0; index < 64; index++ {
				grant.PartUrls = append(grant.PartUrls, server.URL+"/part/"+strconv.Itoa(index+1))
			}
			err = uploadWorkspaceArchiveHTTP(t.Context(), server.Client(), grant, upload)
			if (err != nil) != rejected || !completed || aborted != rejected || parts < 2 {
				t.Fatalf("multipart outcome error=%v completed=%t aborted=%t parts=%d", err, completed, aborted, parts)
			}
			digest := sha256.Sum256(content.Bytes())
			if actual := "sha256:" + hex.EncodeToString(digest[:]); actual != upload.Descriptor().ArchiveChecksum {
				t.Fatalf("multipart upload checksum=%s expected=%s", actual, upload.Descriptor().ArchiveChecksum)
			}
		})
	}
}
