package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestCommandFailurePreservesExplicitPresentationModes(t *testing.T) {
	err := run(context.Background(), []string{"--output", "plain", "--color", "never", "unknown-command"}, &bytes.Buffer{})
	var presented *commandPresentationError
	if !errors.As(err, &presented) {
		t.Fatalf("failure lacks presentation contract: %v", err)
	}
	if presented.renderer.OutputMode != cliui.OutputPlain || presented.renderer.ColorMode != cliui.ColorNever {
		t.Fatalf("failure renderer = output %s color %s", presented.renderer.OutputMode, presented.renderer.ColorMode)
	}
}

func TestCancellationExitClassificationPreservesCleanupFailures(t *testing.T) {
	if !isOnlyContextCancellation(context.Canceled) || !isOnlyContextCancellation(fmt.Errorf("wrapped: %w", context.Canceled)) || !isOnlyContextCancellation(errors.Join(context.Canceled, fmt.Errorf("also canceled: %w", context.Canceled))) {
		t.Fatal("pure cancellation was not classified for exit 130")
	}
	cleanup := errors.New("Sandbox cleanup failed")
	if isOnlyContextCancellation(errors.Join(context.Canceled, cleanup)) {
		t.Fatal("cleanup failure joined with cancellation was swallowed")
	}
}

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

func TestEveryCommandHasOutputContract(t *testing.T) {
	for command := range commandAliases {
		if _, found := commandContracts[command]; !found {
			t.Errorf("command %q has no output contract", command)
		}
	}
	for _, command := range []string{
		"version", "login", "logout", "whoami", "run", "exec", "shell",
		"sandbox shell", "exec stream", "logs tail", "logs follow",
		"diagnostics bundle", "diagnostics egress-contexts", "timings sandbox", "timings operation",
		"timings summary", "resources check", "resources apply", "operation",
		"platform login", "controller login", "application login", "tenant",
		"controller-authority", "subject", "application-authority", "usage",
		"deployment usage",
	} {
		contract, found := commandContracts[command]
		if !found || contract.Command != command || contract.Output == "" || contract.ExitOwner == "" {
			t.Errorf("command %q has incomplete output contract: %#v", command, contract)
		}
	}
}

func TestGlobalPresentationFlags(t *testing.T) {
	newSessionEnvironment(t)
	var plain bytes.Buffer
	if err := run(context.Background(), []string{"--output", "plain", "--color", "never", "version"}, &plain); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain.Bytes(), []byte("SecondBox CLI\n")) || bytes.Contains(plain.Bytes(), []byte("\x1b")) {
		t.Fatalf("plain version output = %q", plain.String())
	}
	var machine bytes.Buffer
	if err := run(context.Background(), []string{"--output", "json", "version"}, &machine); err != nil {
		t.Fatal(err)
	}
	if got := machine.String(); got != "{\"version\":\"0.0.0-development\",\"sourceCommit\":\"development\"}\n" {
		t.Fatalf("JSON version output = %q", got)
	}
	if err := run(context.Background(), []string{"--color", "sometimes", "version"}, io.Discard); err == nil {
		t.Fatal("invalid color mode must fail")
	}
}

func TestRootHelpFormsExitSuccessfullyWithoutSession(t *testing.T) {
	for _, arguments := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		var output bytes.Buffer
		if err := run(context.Background(), arguments, &output); err != nil {
			t.Fatalf("secondbox %v help error = %v", arguments, err)
		}
		if !strings.Contains(output.String(), "SecondBox CLI\n\nUsage\n") || !strings.Contains(output.String(), "Global options\n") || strings.Contains(output.String(), "\x1b") {
			t.Fatalf("secondbox %v help output = %q", arguments, output.String())
		}
	}
}

func TestForcedColorHelpContainsRealANSI(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--color", "always", "help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\x1b[") || strings.Contains(output.String(), "�[") {
		t.Fatalf("forced-color help contains escaped or sanitized ANSI: %q", output.String())
	}
}

func TestBoundedAliasPlainViewAndMachinePassthrough(t *testing.T) {
	newSessionEnvironment(t)
	const response = "{ \"items\" : [{\"id\":\"sbx_123456789\",\"profile\":\"durable-coding\",\"state\":\"ready\",\"desiredState\":\"running\",\"generation\":2,\"revision\":7,\"workspace\":{},\"metadata\":{},\"createdAt\":\"2026-08-07T00:00:00Z\",\"updatedAt\":\"2026-08-07T00:00:00Z\",\"unknownFutureField\":true}] }\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, response)
	}))
	defer server.Close()
	credentials := []string{"--url", server.URL, "--token", "token", "--tenant-ref", "tenant", "--subject-ref", "subject"}
	var machine bytes.Buffer
	if err := run(context.Background(), append(append([]string{}, credentials...), "sandboxes", "list"), &machine); err != nil {
		t.Fatal(err)
	}
	if machine.String() != response {
		t.Fatalf("machine response changed:\n got %q\nwant %q", machine.String(), response)
	}
	var plain bytes.Buffer
	args := append([]string{"--output", "plain"}, credentials...)
	args = append(args, "sandboxes", "list")
	if err := run(context.Background(), args, &plain); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"sbx_123456789", "durable-coding", "ready"} {
		if !strings.Contains(plain.String(), value) {
			t.Fatalf("plain view lacks %q: %q", value, plain.String())
		}
	}
	if strings.Contains(plain.String(), "unknownFutureField") || strings.Contains(plain.String(), "\x1b") {
		t.Fatalf("unsafe or unknown presentation bytes: %q", plain.String())
	}
	var escapeHatch bytes.Buffer
	escapeArgs := append([]string{"--output", "plain"}, credentials...)
	escapeArgs = append(escapeArgs, "operation", "listSandboxes")
	if err := run(context.Background(), escapeArgs, &escapeHatch); err != nil {
		t.Fatal(err)
	}
	if escapeHatch.String() != response {
		t.Fatalf("generic operation escape hatch changed bytes: %q", escapeHatch.String())
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
