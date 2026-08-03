package secondboxclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestGeneratedOperationsMatchOpenAPI(t *testing.T) {
	expected := loadOpenAPIOperations(t)
	for operationID, want := range expected {
		got, exists := LookupOperation(operationID)
		if !exists {
			t.Errorf("OpenAPI operation %q is absent from the Go SDK", operationID)
			continue
		}
		if got.Method != want.Method || got.PathTemplate != want.Path ||
			got.RequestBodyRequired != want.RequestBodyRequired ||
			!reflect.DeepEqual(operationContentTypes(got), want.ContentTypes) {
			t.Errorf(
				"Go SDK operation %q differs from OpenAPI\n  SDK:     method=%s path=%s required=%t content=%v\n  OpenAPI: method=%s path=%s required=%t content=%v",
				operationID,
				got.Method, got.PathTemplate, got.RequestBodyRequired, operationContentTypes(got),
				want.Method, want.Path, want.RequestBodyRequired, want.ContentTypes,
			)
		}
	}
	for operationID := range operations {
		if _, exists := expected[operationID]; !exists {
			t.Errorf("Go SDK operation %q is absent from OpenAPI", operationID)
		}
	}
}

func TestSecondBoxClientSendsGeneratedOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/v1/sandboxes/sandbox-1" {
			t.Errorf("request path = %s, want /v1/sandboxes/sandbox-1", request.URL.Path)
		}
		if request.URL.Query().Get("include") != "instance" {
			t.Errorf("include query = %q, want instance", request.URL.Query().Get("include"))
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"sandbox-1"}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(t.Context(), GetSandboxOperation, RequestOptions{
		PathParameters:  map[string]string{"sandboxId": "sandbox-1"},
		QueryParameters: url.Values{"include": []string{"instance"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200", response.StatusCode)
	}
}

func TestSecondBoxClientPreservesMultipartBoundary(t *testing.T) {
	const contentType = "multipart/form-data; boundary=secondbox-boundary"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != contentType {
			t.Errorf("content type = %q, want %q", request.Header.Get("Content-Type"), contentType)
		}
		response.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata, found := LookupOperation("uploadSandboxArtifact")
	if !found {
		t.Fatal("uploadSandboxArtifact operation is missing")
	}
	response, err := client.Do(t.Context(), metadata, RequestOptions{
		PathParameters: map[string]string{"sandboxId": "sandbox-1"},
		Body:           strings.NewReader("--secondbox-boundary--\r\n"),
		ContentType:    contentType,
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

type openAPIOperationExpectation struct {
	Method              string
	Path                string
	RequestBodyRequired bool
	ContentTypes        []string
}

func loadOpenAPIOperations(t *testing.T) map[string]openAPIOperationExpectation {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Go SDK transport test")
	}
	contractPath := filepath.Join(
		filepath.Dir(sourceFile), "..", "..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json",
	)
	contents, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	var document struct {
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			PathItems map[string]json.RawMessage `json:"pathItems"`
		} `json:"components"`
	}
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode OpenAPI contract: %v", err)
	}
	expected := make(map[string]openAPIOperationExpectation)
	for path, rawPathItem := range document.Paths {
		pathItem := decodeOpenAPIPathItem(t, rawPathItem)
		if rawReference, exists := pathItem["$ref"]; exists {
			var reference string
			if err := json.Unmarshal(rawReference, &reference); err != nil {
				t.Fatalf("decode OpenAPI path-item reference for %s: %v", path, err)
			}
			const prefix = "#/components/pathItems/"
			if !strings.HasPrefix(reference, prefix) {
				t.Fatalf("unsupported OpenAPI path-item reference %q", reference)
			}
			referenced, exists := document.Components.PathItems[strings.TrimPrefix(reference, prefix)]
			if !exists {
				t.Fatalf("OpenAPI path-item reference %q does not exist", reference)
			}
			pathItem = decodeOpenAPIPathItem(t, referenced)
		}
		for _, method := range []string{"delete", "get", "patch", "post", "put"} {
			rawOperation, exists := pathItem[method]
			if !exists {
				continue
			}
			var operation struct {
				OperationID string `json:"operationId"`
				RequestBody *struct {
					Required bool                       `json:"required"`
					Content  map[string]json.RawMessage `json:"content"`
				} `json:"requestBody"`
			}
			if err := json.Unmarshal(rawOperation, &operation); err != nil {
				t.Fatalf("decode OpenAPI operation %s %s: %v", method, path, err)
			}
			want := openAPIOperationExpectation{Method: strings.ToUpper(method), Path: path}
			if operation.RequestBody != nil {
				want.RequestBodyRequired = operation.RequestBody.Required
				for contentType := range operation.RequestBody.Content {
					want.ContentTypes = append(want.ContentTypes, contentType)
				}
				sort.Strings(want.ContentTypes)
			}
			expected[operation.OperationID] = want
		}
	}
	return expected
}

func decodeOpenAPIPathItem(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var pathItem map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pathItem); err != nil {
		t.Fatalf("decode OpenAPI path item: %v", err)
	}
	return pathItem
}

func operationContentTypes(metadata OperationMetadata) []string {
	var contentTypes []string
	for _, media := range metadata.RequestBody {
		contentTypes = append(contentTypes, media.ContentType)
	}
	sort.Strings(contentTypes)
	return contentTypes
}
