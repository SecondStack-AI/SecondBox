package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestResolveCommandAliases(t *testing.T) {
	tests := []struct {
		args      []string
		operation string
		rest      []string
	}{
		{[]string{"profiles", "disable"}, "disableProfile", nil},
		{[]string{"runner-pools", "create"}, "createRunnerPool", nil},
		{[]string{"runner-pools", "update"}, "updateRunnerPool", nil},
		{[]string{"runners", "list"}, "listRunners", nil},
		{[]string{"runners", "get"}, "getRunner", nil},
		{[]string{"sandboxes", "restore"}, "restoreSandboxSnapshot", nil},
		{[]string{"exec", "stream"}, "createSandboxExecStream", nil},
		{[]string{"exec", "cancel"}, "cancelSandboxExecStream", nil},
		// `exec` itself is an operational command now; the buffered route stays
		// reachable through the generic operation escape hatch.
		{[]string{"operation", "executeSandboxCommand"}, "executeSandboxCommand", []string{}},
		{[]string{"shell", "create"}, "createSandboxTerminal", nil},
		{[]string{"files", "read"}, "readSandboxFile", nil},
		{[]string{"snapshots", "create"}, "createSandboxSnapshot", nil},
		{[]string{"operation", "getSandbox", "--path", "sandboxId=sandbox-1"}, "getSandbox", []string{"--path", "sandboxId=sandbox-1"}},
	}
	for _, test := range tests {
		operation, rest, err := resolveCommand(test.args)
		if err != nil {
			t.Fatalf("resolveCommand(%v): %v", test.args, err)
		}
		if operation != test.operation || !reflect.DeepEqual(rest, test.rest) {
			t.Errorf("resolveCommand(%v) = %q, %v; want %q, %v", test.args, operation, rest, test.operation, test.rest)
		}
	}
}

func TestCommandAliasesReferenceGeneratedOperations(t *testing.T) {
	for command, alias := range commandAliases {
		if _, found := secondboxclient.LookupOperation(alias.operation); !found {
			t.Errorf("command %q references unknown operation %q", command, alias.operation)
		}
	}
}

// TestParseOperationOptionsOmitsAbsentBody guards the interface-nil hazard:
// a nil *os.File assigned to the io.Reader field yields a non-nil interface
// whose first Read returns os.ErrInvalid.
func TestParseOperationOptionsOmitsAbsentBody(t *testing.T) {
	options, body, err := parseOperationOptions("listSandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		t.Fatalf("body file = %v; want none", body)
	}
	if options.Body != nil {
		t.Fatal("CallOptions.Body must be nil when no --body is supplied")
	}
}

func TestParseOperationOptionsCarriesSuppliedBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"profile":"p","metadata":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	options, body, err := parseOperationOptions("createSandbox", []string{"--body", path})
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("body file = nil; want the opened request file")
	}
	defer body.Close()
	if options.Body == nil {
		t.Fatal("CallOptions.Body must carry the supplied request file")
	}
	content, err := io.ReadAll(options.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != `{"profile":"p","metadata":{}}` {
		t.Errorf("request body = %q; want the file content", content)
	}
}

// newRecordingOperationServer reports the body each request actually carried.
func newRecordingOperationServer(t *testing.T, observed *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			content, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			*observed = string(content)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"items":[]}`))
		},
	))
	t.Cleanup(server.Close)
	return server
}

// TestGenericOperationRequestSucceedsWithoutBody reproduces the failure the
// typed-nil body caused: every bodyless operation failed with "invalid
// argument" before reaching the network.
func TestGenericOperationRequestSucceedsWithoutBody(t *testing.T) {
	var observed string
	server := newRecordingOperationServer(t, &observed)
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		server.URL, "token-1", "tenant-1", "subject-1", server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	options, _, err := parseOperationOptions("listSandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Request(context.Background(), "listSandboxes", options)
	if err != nil {
		t.Fatalf("bodyless operation must reach the server: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", response.StatusCode)
	}
	if observed != "" {
		t.Errorf("server observed body %q; want none", observed)
	}
}

func TestGenericOperationRequestSendsSuppliedBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	const request = `{"profile":"p","metadata":{}}`
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	var observed string
	server := newRecordingOperationServer(t, &observed)
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		server.URL, "token-1", "tenant-1", "subject-1", server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	options, body, err := parseOperationOptions("createSandbox", []string{"--body", path})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	options.Headers.Set("Idempotency-Key", "request-1")
	response, err := client.Request(context.Background(), "createSandbox", options)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if observed != request {
		t.Errorf("server observed body %q; want %q", observed, request)
	}
}
