package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDiagnosticsBundleBoundsContentAndUsesCredentialOnlyForAuthorizedProbes(t *testing.T) {
	const platformToken = "diagnostic-platform-token-at-least-24-bytes"
	var mu sync.Mutex
	authorizationByPath := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		mu.Lock()
		authorizationByPath[request.URL.Path] = request.Header.Get("Authorization")
		mu.Unlock()
		if request.URL.Path == "/v1/timings" {
			if request.URL.Query().Get("windowSeconds") != "300" {
				t.Errorf("timing window = %q", request.URL.Query().Get("windowSeconds"))
			}
			if request.Header.Get("X-SecondBox-Tenant-Ref") != "secondbox" ||
				request.Header.Get("X-SecondBox-Subject-Ref") != "secondbox-admin" {
				t.Errorf("timing ownership headers = %#v", request.Header)
			}
			_, _ = io.WriteString(writer, `{"windowSeconds":300,"observedAt":"2026-07-29T12:00:00Z","boot":{"count":0},"bootStages":[],"exec":{"count":0},"execSeries":[],"api":{"count":0},"apiSeries":[],"operations":[]}`)
			return
		}
		if request.URL.Path == "/v1/diagnostics/egress-contexts" {
			_, _ = io.WriteString(writer, `{"ready":true,"truncated":false,"requirements":[],"runners":[],"activeAssignments":[]}`)
			return
		}
		_, _ = io.WriteString(writer, strings.Repeat("p", 64))
	}))
	defer server.Close()

	logPath := filepath.Join(t.TempDir(), "control-plane.jsonl")
	if err := os.WriteFile(logPath, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "support.tar.gz")
	var output bytes.Buffer
	err := runDiagnosticsBundleCommand(
		context.Background(), server.URL, platformToken,
		[]string{
			"--output", outputPath,
			"--control-plane-log", logPath,
			"--max-log-bytes", "17",
			"--max-probe-bytes", "32",
			"--http-timeout", "2s",
			"--timing-window", "5m",
		},
		&output,
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), outputPath) {
		t.Fatalf("bundle output = %q", output.String())
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %o, want 600", info.Mode().Perm())
	}
	files := readDiagnosticArchive(t, outputPath)
	if len(files["control-plane.log.tail"]) != 17 {
		t.Fatalf("log tail bytes = %d, want 17", len(files["control-plane.log.tail"]))
	}
	if string(files["timing-summary.status"]) != "truncated\n" ||
		len(files["timing-summary.json"]) != 32 {
		t.Fatalf(
			"bounded timing body/status = %d/%q",
			len(files["timing-summary.json"]), files["timing-summary.status"],
		)
	}
	if _, exists := files["SHA256SUMS"]; !exists {
		t.Fatal("bundle omitted SHA256SUMS")
	}
	for name, content := range files {
		if bytes.Contains(content, []byte(platformToken)) {
			t.Fatalf("bundle entry %s contains the platform token", name)
		}
		for _, retired := range []string{
			"SECONDBOX_APPLICATION_" + "AUTHORITIES_JSON",
			"application_" + "authorities_file",
			"application-" + "authorities.json",
		} {
			if bytes.Contains(content, []byte(retired)) {
				t.Fatalf("bundle entry %s retained static authority surface %q", name, retired)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if authorizationByPath[path] != "" {
			t.Errorf("%s received authorization", path)
		}
	}
	if authorizationByPath["/v1/timings"] != "Bearer "+platformToken {
		t.Errorf("timing authorization = %q", authorizationByPath["/v1/timings"])
	}
	if authorizationByPath["/v1/diagnostics/egress-contexts"] != "Bearer "+platformToken {
		t.Errorf("egress preflight authorization = %q", authorizationByPath["/v1/diagnostics/egress-contexts"])
	}
	if got := string(files["egress-context-preflight.status"]); got != "truncated\n" {
		t.Errorf("egress preflight status = %q", got)
	}
}

func TestEgressContextDiagnosticsIsReadOnlyPlatformProbe(t *testing.T) {
	const body = `{"ready":false,"truncated":false,"requirements":[{"tenantRef":"tenant-a","egressContext":"customer-a","profileName":"coding","profileRevisionId":"prv_coding_4","poolName":"default","compatibleRunnerIds":[],"status":"runner_context_unavailable"}],"runners":[],"activeAssignments":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/diagnostics/egress-contexts" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer platform-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(writer, body)
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := runEgressContextDiagnosticsCommand(
		t.Context(), server.URL, "platform-token", nil, &output, server.Client(),
	); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != body+"\n" {
		t.Fatalf("output = %q", got)
	}
}

func readDiagnosticArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	compressor, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	reader := tar.NewReader(compressor)
	files := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return files
}
