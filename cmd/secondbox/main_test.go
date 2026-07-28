package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestResolveCommandAliases(t *testing.T) {
	tests := []struct {
		args      []string
		operation string
		rest      []string
	}{
		{[]string{"auth", "check"}, "listProjects", []string{"--query", "limit=1"}},
		{[]string{"projects", "create", "--body", "project.json"}, "createProject", []string{"--body", "project.json"}},
		{[]string{"keys", "revoke"}, "revokeAPIKey", nil},
		{[]string{"profiles", "disable"}, "disableProfile", nil},
		{[]string{"runner-pools", "create"}, "createRunnerPool", nil},
		{[]string{"runner-pools", "update"}, "updateRunnerPool", nil},
		{[]string{"runners", "list"}, "listRunners", nil},
		{[]string{"runners", "get"}, "getRunner", nil},
		{[]string{"sandboxes", "checkpoint"}, "checkpointSandbox", nil},
		{[]string{"exec"}, "executeSandboxCommand", nil},
		{[]string{"shell", "create"}, "createSandboxTerminal", nil},
		{[]string{"files", "read"}, "readSandboxFile", nil},
		{[]string{"checkpoints", "create"}, "checkpointSandbox", nil},
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

func TestRunLogsTailIsBoundedAndNeedsNoAPICredentials(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("discard-this\nkeep-this\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}

	var output strings.Builder
	err := run(t.Context(), []string{
		"logs", "tail",
		"--path", logPath,
		"--bytes", "10",
	}, &output)
	if err != nil {
		t.Fatalf("run logs tail: %v", err)
	}
	if got, want := output.String(), "keep-this\n"; got != want {
		t.Fatalf("logs tail output = %q, want %q", got, want)
	}
}

func TestRunLogsFollowStreamsAppendsUntilContextCancellation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelOnSubstringWriter{
		cancel: cancel,
		match:  "appended\n",
		ready:  make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{
			"logs", "follow",
			"--path", logPath,
			"--bytes", "9",
			"--poll-interval", "5ms",
		}, writer)
	}()
	select {
	case <-writer.ready:
	case <-time.After(time.Second):
		t.Fatal("logs follow did not write the initial tail")
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open log fixture: %v", err)
	}
	if _, err := file.WriteString("appended\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append log fixture: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close log fixture: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run logs follow: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logs follow did not stream appended bytes")
	}
	if got, want := writer.String(), "existing\nappended\n"; got != want {
		t.Fatalf("logs follow output = %q, want %q", got, want)
	}
}

func TestRunLogsFollowReopensReplacedLog(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelOnSubstringWriter{
		cancel: cancel,
		match:  "replacement\n",
		ready:  make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{
			"logs", "follow",
			"--path", logPath,
			"--bytes", "9",
			"--poll-interval", "5ms",
		}, writer)
	}()
	select {
	case <-writer.ready:
	case <-time.After(time.Second):
		t.Fatal("logs follow did not write the initial tail")
	}
	rotatedPath := filepath.Join(directory, "secondbox.jsonl.1")
	if err := os.Rename(logPath, rotatedPath); err != nil {
		t.Fatalf("rotate log fixture: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("replacement\n"), 0o600); err != nil {
		t.Fatalf("write replacement log fixture: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run logs follow after replacement: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logs follow did not stream the replacement file")
	}
	if got, want := writer.String(), "existing\nreplacement\n"; got != want {
		t.Fatalf("logs follow replacement output = %q, want %q", got, want)
	}
}

func TestRunLogsFollowReadsAfterTruncation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelOnSubstringWriter{
		cancel: cancel,
		match:  "n\n",
		ready:  make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{
			"logs", "follow",
			"--path", logPath,
			"--bytes", "9",
			"--poll-interval", "5ms",
		}, writer)
	}()
	select {
	case <-writer.ready:
	case <-time.After(time.Second):
		t.Fatal("logs follow did not write the initial tail")
	}
	if err := os.WriteFile(logPath, []byte("n\n"), 0o600); err != nil {
		t.Fatalf("truncate log fixture: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run logs follow after truncation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logs follow did not stream the truncated file")
	}
	if got, want := writer.String(), "existing\nn\n"; got != want {
		t.Fatalf("logs follow truncation output = %q, want %q", got, want)
	}
}

func TestOperationalLogInputsRejectSymlinksAndFollowSymlinkSwap(t *testing.T) {
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "target.jsonl")
	linkPath := filepath.Join(directory, "secondbox.jsonl")
	if err := os.WriteFile(targetPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("create log symlink: %v", err)
	}
	if err := run(t.Context(), []string{
		"logs", "tail",
		"--path", linkPath,
		"--bytes", "9",
	}, io.Discard); err == nil {
		t.Fatal("logs tail accepted a symlink")
	}
	if _, err := readLogTail(linkPath, 9); err == nil {
		t.Fatal("diagnostic log reader accepted a symlink")
	}

	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("remove log symlink: %v", err)
	}
	if err := os.WriteFile(linkPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write followed log: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelOnSubstringWriter{
		cancel: cancel,
		match:  "never-written",
		ready:  make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{
			"logs", "follow",
			"--path", linkPath,
			"--bytes", "9",
			"--poll-interval", "5ms",
		}, writer)
	}()
	select {
	case <-writer.ready:
	case <-time.After(time.Second):
		t.Fatal("logs follow did not write the initial tail")
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("remove followed log: %v", err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("swap followed log to symlink: %v", err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("logs follow symlink swap error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logs follow did not reject a symlink swap")
	}
}

func TestRunDiagnosticsBundleIsBoundedChecksummedAndUnauthenticated(t *testing.T) {
	requests := make(chan *http.Request, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, request.URL.Path+"\n")
	}))
	defer server.Close()
	logPath := filepath.Join(t.TempDir(), "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("discard-this\nkeep-this\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "support.tar.gz")

	var output strings.Builder
	err := run(t.Context(), []string{
		"--url", server.URL,
		"--token", "must-not-be-sent",
		"diagnostics", "bundle",
		"--output", outputPath,
		"--control-plane-log", logPath,
		"--max-log-bytes", "10",
		"--http-timeout", "1s",
	}, &output)
	if err != nil {
		t.Fatalf("run diagnostics bundle: %v", err)
	}
	close(requests)
	var paths []string
	for request := range requests {
		paths = append(paths, request.URL.Path)
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("diagnostic probe Authorization = %q", authorization)
		}
	}
	if got, want := strings.Join(paths, ","), "/healthz,/readyz,/metrics"; got != want {
		t.Fatalf("diagnostic probe paths = %q, want %q", got, want)
	}

	files := readTarGzipFiles(t, outputPath)
	if got, want := string(files["control-plane.log.tail"]), "keep-this\n"; got != want {
		t.Fatalf("diagnostic log tail = %q, want %q", got, want)
	}
	for _, name := range []string{
		"healthz.body", "healthz.status",
		"readyz.body", "readyz.status",
		"metrics.body", "metrics.status",
		"SHA256SUMS",
	} {
		if _, found := files[name]; !found {
			t.Errorf("diagnostic bundle missing %q", name)
		}
	}
	verifyBundleChecksums(t, files)
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat diagnostic bundle: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("diagnostic bundle mode = %o, want %o", got, want)
	}
}

func TestCollectProbeBoundsResponseAndRecordsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", int(maximumProbeBodyBytes)+1)), maximumProbeBodyBytes+1)
	}))
	baseURL, err := parseProbeBaseURL(server.URL)
	if err != nil {
		server.Close()
		t.Fatalf("parse probe URL: %v", err)
	}
	body, status := collectProbe(t.Context(), server.Client(), baseURL, "metrics")
	server.Close()
	if got, want := int64(len(body)), maximumProbeBodyBytes; got != want {
		t.Fatalf("bounded probe body bytes = %d, want %d", got, want)
	}
	if got, want := status, "200 body_truncated"; got != want {
		t.Fatalf("bounded probe status = %q, want %q", got, want)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	body, status = collectProbe(cancelled, http.DefaultClient, baseURL, "healthz")
	if body != nil || status != "transport_error" {
		t.Fatalf("cancelled probe = body %q status %q", body, status)
	}
}

func TestRunOperationalCommandsRejectUnsafeBoundsAndOverwrite(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "secondbox.jsonl")
	if err := os.WriteFile(logPath, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	for name, args := range map[string][]string{
		"relative log path": {"logs", "tail", "--path", "relative.log", "--bytes", "1"},
		"zero log bytes":    {"logs", "tail", "--path", logPath, "--bytes", "0"},
		"oversize log tail": {"logs", "tail", "--path", logPath, "--bytes", "104857601"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context(), args, io.Discard); err == nil {
				t.Fatalf("run(%v) succeeded", args)
			}
		})
	}

	outputPath := filepath.Join(t.TempDir(), "existing.tar.gz")
	if err := os.WriteFile(outputPath, []byte("preserve"), 0o600); err != nil {
		t.Fatalf("write existing output: %v", err)
	}
	err := run(t.Context(), []string{
		"--url", "http://127.0.0.1:1",
		"diagnostics", "bundle",
		"--output", outputPath,
		"--control-plane-log", logPath,
		"--max-log-bytes", "1",
		"--http-timeout", "1s",
	}, io.Discard)
	if err == nil {
		t.Fatal("diagnostics bundle overwrote an existing output")
	}
	if contents, err := os.ReadFile(outputPath); err != nil || string(contents) != "preserve" {
		t.Fatalf("existing output changed: contents=%q error=%v", contents, err)
	}
}

func TestCommandAliasesReferenceGeneratedOperations(t *testing.T) {
	for command, alias := range commandAliases {
		if _, found := secondboxclient.LookupOperation(alias.operation); !found {
			t.Errorf("command %q references unknown operation %q", command, alias.operation)
		}
	}
}

type cancelOnSubstringWriter struct {
	builder strings.Builder
	cancel  context.CancelFunc
	match   string
	mutex   sync.Mutex
	once    sync.Once
	ready   chan struct{}
}

func (writer *cancelOnSubstringWriter) Write(contents []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	count, err := writer.builder.Write(contents)
	if strings.Contains(writer.builder.String(), "existing\n") {
		writer.once.Do(func() { close(writer.ready) })
	}
	if strings.Contains(writer.builder.String(), writer.match) {
		writer.cancel()
	}
	return count, err
}

func (writer *cancelOnSubstringWriter) String() string {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.builder.String()
}

func readTarGzipFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	files := map[string][]byte{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		files[strings.TrimPrefix(header.Name, "./")] = contents
	}
}

func verifyBundleChecksums(t *testing.T, files map[string][]byte) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("invalid checksum line %q", line)
		}
		contents, found := files[fields[1]]
		if !found {
			t.Fatalf("checksum references missing %q", fields[1])
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(contents))
		if fields[0] != sum {
			t.Fatalf("checksum for %q = %q, want %q", fields[1], fields[0], sum)
		}
	}
}
