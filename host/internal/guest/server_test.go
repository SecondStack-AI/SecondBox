package microvmguest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentcy/internal/registry"
	"agentcy/internal/runtimecontext"
	"agentcy/internal/sandboxlimits"
)

func TestHeartbeat(t *testing.T) {
	handler := Server{
		InstanceID: "vm-1",
		AgentID:    "agent-1",
		Now:        func() time.Time { return time.Date(2026, 6, 24, 1, 2, 3, 0, time.UTC) },
	}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/heartbeat", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["instanceId"] != "vm-1" || body["agentId"] != "agent-1" || body["healthy"] != true {
		t.Fatalf("heartbeat body = %#v", body)
	}
}

func TestBoundedCommandOutputBufferDiscardsExcess(t *testing.T) {
	buffer := newCommandOutputBuffer(32)
	payload := []byte(strings.Repeat("x", 100))
	if n, err := buffer.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if len(buffer.String()) > 32 || !strings.Contains(buffer.String(), "truncated") {
		t.Fatalf("bounded output = %q", buffer.String())
	}
}

func TestWorkspaceListAndRead(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifacts", "report.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := Server{WorkspaceDir: workspace}.Handler()

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/list?path=artifacts", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("artifacts/report.txt")) {
		t.Fatalf("list body = %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=artifacts/report.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("read status = %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != "5" {
		t.Fatalf("content length = %q", rr.Header().Get("Content-Length"))
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("read body = %q", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=artifacts/report.txt&maxBytes=3", nil))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize read status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "5 bytes") || !strings.Contains(rr.Body.String(), "3 bytes") {
		t.Fatalf("oversize read body = %s", rr.Body.String())
	}
}

func TestWorkspaceRejectsTraversal(t *testing.T) {
	handler := Server{WorkspaceDir: t.TempDir()}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=../../etc/passwd", nil))
	if rr.Code != http.StatusNotFound && rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceFileTransferRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	payload := make([]byte, 5<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(payload))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/workspace/write?path=artifacts/blob.bin", bytes.NewReader(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("write status = %d: %s", rr.Code, rr.Body.String())
	}
	var writeResp struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &writeResp); err != nil {
		t.Fatalf("decode write response: %v", err)
	}
	if writeResp.Size != int64(len(payload)) || writeResp.SHA256 != wantHash {
		t.Fatalf("write response = %#v, want size=%d sha=%s", writeResp, len(payload), wantHash)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=artifacts/blob.bin", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("read status = %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != strconv.Itoa(len(payload)) {
		t.Fatalf("content length = %q", rr.Header().Get("Content-Length"))
	}
	gotHash := fmt.Sprintf("%x", sha256.Sum256(rr.Body.Bytes()))
	if gotHash != wantHash {
		t.Fatalf("read sha = %s, want %s", gotHash, wantHash)
	}
}

func TestWorkspaceFileTransferEmptyFile(t *testing.T) {
	handler := Server{WorkspaceDir: t.TempDir()}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/workspace/write?path=empty.txt", bytes.NewReader(nil)))
	if rr.Code != http.StatusOK {
		t.Fatalf("write status = %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=empty.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("read status = %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != "0" || rr.Body.Len() != 0 {
		t.Fatalf("empty read length=%q bodyLen=%d", rr.Header().Get("Content-Length"), rr.Body.Len())
	}
}

func TestWorkspaceFileTransferMissingAndRejectedPaths(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=missing.bin", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d: %s", rr.Code, rr.Body.String())
	}

	if err := os.WriteFile(filepath.Join(workspace, "target.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(workspace, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/workspace/read?path=link.txt", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("symlink status = %d: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/workspace/write?path=../escape.txt", strings.NewReader("nope")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("write traversal status = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceFileTransferWriteRejectsOversizeAndCleansTemp(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/workspace/write?path=dir/too-big.bin&maxBytes=3", strings.NewReader("1234")))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize write status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "4 bytes") || !strings.Contains(rr.Body.String(), "3 bytes") {
		t.Fatalf("oversize write body = %s", rr.Body.String())
	}
	assertNoTransferTempFiles(t, filepath.Join(workspace, "dir"))
	if _, err := os.Stat(filepath.Join(workspace, "dir", "too-big.bin")); !os.IsNotExist(err) {
		t.Fatalf("target after failed oversize stat err = %v", err)
	}
}

func TestWorkspaceFileTransferAbortedBodyCleansTemp(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/workspace/write?path=dir/aborted.bin", &errReader{data: []byte("partial"), err: errors.New("client aborted")})
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("aborted write status = %d: %s", rr.Code, rr.Body.String())
	}
	assertNoTransferTempFiles(t, filepath.Join(workspace, "dir"))
	if _, err := os.Stat(filepath.Join(workspace, "dir", "aborted.bin")); !os.IsNotExist(err) {
		t.Fatalf("target after aborted write stat err = %v", err)
	}
}

func TestWorkspaceFileTransferConcurrentWritesDistinctPaths(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	const count = 8
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat(fmt.Sprintf("%02d", i), 1024)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/workspace/write?path=concurrent/%d.txt", i), strings.NewReader(body)))
			if rr.Code != http.StatusOK {
				errs <- fmt.Errorf("write %d status=%d body=%s", i, rr.Code, rr.Body.String())
				return
			}
			got, err := os.ReadFile(filepath.Join(workspace, "concurrent", fmt.Sprintf("%d.txt", i)))
			if err != nil {
				errs <- err
				return
			}
			if string(got) != body {
				errs <- fmt.Errorf("write %d body mismatch", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTailLogFileHonorsTailAndCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guest.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := tailLogFile(f, "2", 1<<20)
	if err != nil {
		t.Fatalf("tail log: %v", err)
	}
	if string(data) != "three\nfour\n" {
		t.Fatalf("tail data = %q", string(data))
	}
	data, err = tailLogFile(f, "", 5)
	if err != nil {
		t.Fatalf("cap log: %v", err)
	}
	if string(data) != "four\n" {
		t.Fatalf("capped data = %q", string(data))
	}
}

func TestFreezeThawUseConfiguredFreezer(t *testing.T) {
	freezer := &fakeFreezer{}
	handler := Server{WorkspaceDir: "/workspace", Freezer: freezer}.Handler()

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/workspace/freeze", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("freeze status = %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/workspace/thaw", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("thaw status = %d: %s", rr.Code, rr.Body.String())
	}
	if freezer.freezePath != "/workspace" || freezer.thawPath != "/workspace" {
		t.Fatalf("freezer paths = %#v", freezer)
	}
}

func TestSecretsApplyWritesPrivateFiles(t *testing.T) {
	privateDir := t.TempDir()
	handler := Server{RuntimePrivateDir: privateDir}.Handler()
	body := bytes.NewReader([]byte(`{"env":{"AGENT_PLATFORM_TOKEN":"secret-token"},"files":{"broker/token.json":"{\"token\":\"abc\"}","proxy-ca.crt":"ca-data"}}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/secrets/apply", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	envPath := filepath.Join(privateDir, "env.json")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !bytes.Contains(data, []byte("secret-token")) {
		t.Fatalf("env data = %s", string(data))
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env mode = %o", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o711 {
		t.Fatalf("runtime private dir mode = %o", rootInfo.Mode().Perm())
	}
	filePath := filepath.Join(privateDir, "broker", "token.json")
	data, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	if string(data) != `{"token":"abc"}` {
		t.Fatalf("secret file = %q", string(data))
	}
	info, err = os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %o", info.Mode().Perm())
	}
	caPath := filepath.Join(privateDir, "proxy-ca.crt")
	data, err = os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read proxy CA: %v", err)
	}
	if string(data) != "ca-data" {
		t.Fatalf("proxy CA = %q", string(data))
	}
	info, err = os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("proxy CA mode = %o", info.Mode().Perm())
	}
}

func TestRestoreHardenUsesConfiguredHardener(t *testing.T) {
	hardener := &fakeHardener{}
	handler := Server{Hardener: hardener}.Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/restore/harden", bytes.NewReader([]byte(`{"hostTime":"2026-06-24T22:00:00Z","entropyBase64":"ZnJlc2gtZW50cm9weQ=="}`))))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if hardener.input.HostTime.Format(time.RFC3339) != "2026-06-24T22:00:00Z" {
		t.Fatalf("host time = %s", hardener.input.HostTime.Format(time.RFC3339Nano))
	}
	if string(hardener.input.Entropy) != "fresh-entropy" {
		t.Fatalf("entropy = %q", string(hardener.input.Entropy))
	}
}

func TestToolExecRunsCommandInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	handler := Server{WorkspaceDir: workspace}.Handler()
	resp := postToolExec(t, handler, toolExecRequest{
		Operation:     toolOpExec,
		Command:       "sh",
		Args:          []string{"-c", "printf '%s|%s' \"$PWD\" \"$EXTRA\""},
		Cwd:           "dir/../sub",
		Env:           map[string]string{"EXTRA": "from-env"},
		TimeoutMillis: 1000,
	})
	if resp.Error != "" {
		t.Fatalf("tool error = %s", resp.Error)
	}
	want := filepath.Join(workspace, "sub") + "|from-env"
	if resp.Stdout != want {
		t.Fatalf("stdout = %q, want %q", resp.Stdout, want)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d", resp.ExitCode)
	}
}

func TestToolExecInheritsSafeRuntimeProxyEnv(t *testing.T) {
	workspace := t.TempDir()
	privateDir := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: privateDir}
	if err := server.applySecrets(SecretBundle{Env: map[string]string{
		"HTTP_PROXY":             "http://10.0.0.1:3128",
		"HTTPS_PROXY":            "http://10.0.0.1:3128",
		"NO_PROXY":               "localhost,127.0.0.1",
		"GIT_SSL_CAINFO":         "/runtime-private/proxy-ca.crt",
		"GIT_ASKPASS":            "/runtime-private/github-askpass",
		"GIT_CONFIG_GLOBAL":      "/runtime-private/gitconfig",
		"GIT_TERMINAL_PROMPT":    "0",
		"CURL_CA_BUNDLE":         "/runtime-private/proxy-ca.crt",
		"AG_FLUE_STORE_TOKEN":    "secret-token",
		"AGENT_PLATFORM_TOKEN":   "runtime-secret",
		"AGENTCY_RUNTIME_TOKEN":  "runtime-token",
		"GH_TOKEN":               "agentcy-proxy:github",
		"GITHUB_TOKEN":           "agentcy-proxy:github",
		"PLATFORM_API_URL":       "http://10.0.0.1:8081",
		"REQUESTS_CA_BUNDLE":     "/runtime-private/proxy-ca.crt",
		"SSL_CERT_FILE":          "/runtime-private/proxy-ca.crt",
		"NODE_EXTRA_CA_CERTS":    "/runtime-private/proxy-ca.crt",
		"AGENTCY_COMPARTMENT_ID": "cmp_123",
	}}); err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	resp := postToolExec(t, server.Handler(), toolExecRequest{
		Operation: toolOpExec,
		Command:   "sh",
		Args: []string{"-c", strings.Join([]string{
			"printf 'proxy=%s\\n' \"$HTTPS_PROXY\"",
			"printf 'gitca=%s\\n' \"$GIT_SSL_CAINFO\"",
			"printf 'askpass=%s\\n' \"$GIT_ASKPASS\"",
			"printf 'gitconfig=%s\\n' \"$GIT_CONFIG_GLOBAL\"",
			"printf 'gitprompt=%s\\n' \"$GIT_TERMINAL_PROMPT\"",
			"printf 'curlca=%s\\n' \"$CURL_CA_BUNDLE\"",
			"printf 'sslcert=%s\\n' \"$SSL_CERT_FILE\"",
			"printf 'token=%s\\n' \"$AG_FLUE_STORE_TOKEN\"",
			"printf 'runtime=%s\\n' \"$AGENTCY_RUNTIME_TOKEN\"",
			"printf 'ghtoken=%s\\n' \"$GH_TOKEN\"",
			"printf 'githubtoken=%s\\n' \"$GITHUB_TOKEN\"",
			"printf 'platform=%s\\n' \"$PLATFORM_API_URL\"",
			"printf 'extra=%s\\n' \"$EXTRA\"",
		}, "; ")},
		Env: map[string]string{"EXTRA": "request"},
	})
	if resp.Error != "" {
		t.Fatalf("tool error = %s", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "proxy=http://10.0.0.1:3128\n") ||
		!strings.Contains(resp.Stdout, "gitca=/runtime-private/proxy-ca.crt\n") ||
		!strings.Contains(resp.Stdout, "askpass=/runtime-private/github-askpass\n") ||
		!strings.Contains(resp.Stdout, "gitconfig=/runtime-private/gitconfig\n") ||
		!strings.Contains(resp.Stdout, "gitprompt=0\n") ||
		!strings.Contains(resp.Stdout, "curlca=/runtime-private/proxy-ca.crt\n") ||
		!strings.Contains(resp.Stdout, "sslcert=/runtime-private/proxy-ca.crt\n") ||
		!strings.Contains(resp.Stdout, "runtime=runtime-token\n") ||
		!strings.Contains(resp.Stdout, "ghtoken=agentcy-proxy:github\n") ||
		!strings.Contains(resp.Stdout, "githubtoken=agentcy-proxy:github\n") ||
		!strings.Contains(resp.Stdout, "platform=http://10.0.0.1:8081\n") ||
		!strings.Contains(resp.Stdout, "extra=request\n") {
		t.Fatalf("stdout missing safe env:\n%s", resp.Stdout)
	}
	if strings.Contains(resp.Stdout, "secret-token") || strings.Contains(resp.Stdout, "runtime-secret") {
		t.Fatalf("stdout leaked unsafe env:\n%s", resp.Stdout)
	}
}

func TestApplySecretsMakesGitHubAskpassExecutable(t *testing.T) {
	privateDir := t.TempDir()
	server := Server{WorkspaceDir: t.TempDir(), RuntimePrivateDir: privateDir}
	if err := server.applySecrets(SecretBundle{Files: map[string]string{
		"github-askpass": "#!/bin/sh\nprintf ok\n",
		"gitconfig":      "[credential]\n",
		"proxy-ca.crt":   "ca",
	}}); err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	assertMode := func(name string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(privateDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", name, got, want)
		}
	}
	assertMode("github-askpass", 0o700)
	assertMode("gitconfig", 0o600)
	assertMode("proxy-ca.crt", 0o644)
}

func TestApplySecretsMaterializesRuntimeContextInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir()}
	oauthDir := filepath.Join(workspace, "config", "oauth")
	if err := os.MkdirAll(oauthDir, 0o755); err != nil {
		t.Fatalf("mkdir oauth: %v", err)
	}
	staleGitHub := filepath.Join(oauthDir, "github.context.json")
	if err := os.WriteFile(staleGitHub, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale github: %v", err)
	}
	legacyRawNames := []string{
		"github.json",
		"jira.json",
		"notion.json",
		"tempo.json",
		"gitlab.json",
		"google-workspace.json",
		"google-workspace.credentials.json",
	}
	for _, name := range legacyRawNames {
		if err := os.WriteFile(filepath.Join(oauthDir, name), []byte("secret"), 0o600); err != nil {
			t.Fatalf("write legacy raw %s: %v", name, err)
		}
	}
	legacyTemp := filepath.Join(oauthDir, ".google-workspace.credentials.json.crash.tmp")
	if err := os.WriteFile(legacyTemp, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write legacy temp: %v", err)
	}
	nonMigratedContext := filepath.Join(oauthDir, "notion.context.json")
	if err := os.WriteFile(nonMigratedContext, []byte("broker"), 0o600); err != nil {
		t.Fatalf("write non-migrated context: %v", err)
	}

	err := server.applySecrets(SecretBundle{RuntimeContextProjection: runtimecontext.Projection{
		Files: []runtimecontext.File{{
			Path:    runtimecontext.PathGitHubContext,
			Content: `{"service":"github"}`,
		}},
		MigratedServicePaths:  []string{runtimecontext.PathGitHubContext},
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	got, err := os.ReadFile(staleGitHub)
	if err != nil {
		t.Fatalf("read github context: %v", err)
	}
	if string(got) != `{"service":"github"}` {
		t.Fatalf("github context = %q", got)
	}
	for _, name := range legacyRawNames {
		if _, err := os.Stat(filepath.Join(oauthDir, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy raw file %s should be removed, err=%v", name, err)
		}
	}
	if _, err := os.Stat(legacyTemp); !os.IsNotExist(err) {
		t.Fatalf("legacy temp file should be removed, err=%v", err)
	}
	if _, err := os.Stat(nonMigratedContext); err != nil {
		t.Fatalf("non-migrated context should be preserved: %v", err)
	}
}

func TestApplySecretsRuntimeContextPolicyNoneScrubsContextFiles(t *testing.T) {
	workspace := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir()}
	oauthDir := filepath.Join(workspace, "config", "oauth")
	if err := os.MkdirAll(oauthDir, 0o755); err != nil {
		t.Fatalf("mkdir oauth: %v", err)
	}
	for _, name := range []string{"github.context.json", "notion.context.json"} {
		if err := os.WriteFile(filepath.Join(oauthDir, name), []byte("stale"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	err := server.applySecrets(SecretBundle{RuntimeContextProjection: runtimecontext.Projection{
		EffectivePolicy:       registry.CredentialPolicyNone,
		OmittedPaths:          runtimecontext.AllProjectionPaths(),
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	for _, name := range []string{"github.context.json", "notion.context.json"} {
		if _, err := os.Stat(filepath.Join(oauthDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be scrubbed, err=%v", name, err)
		}
	}
}

func TestApplySecretsRuntimeContextScrubsOmittedMigratedContextFiles(t *testing.T) {
	workspace := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir()}
	oauthDir := filepath.Join(workspace, "config", "oauth")
	if err := os.MkdirAll(oauthDir, 0o755); err != nil {
		t.Fatalf("mkdir oauth: %v", err)
	}
	staleNotion := filepath.Join(oauthDir, "notion.context.json")
	if err := os.WriteFile(staleNotion, []byte("stale notion"), 0o600); err != nil {
		t.Fatalf("write stale notion: %v", err)
	}
	brokerJira := filepath.Join(oauthDir, "jira.context.json")
	if err := os.WriteFile(brokerJira, []byte("broker jira"), 0o600); err != nil {
		t.Fatalf("write broker jira: %v", err)
	}

	err := server.applySecrets(SecretBundle{RuntimeContextProjection: runtimecontext.Projection{
		OmittedPaths:          []string{runtimecontext.PathNotionContext},
		MigratedServicePaths:  []string{runtimecontext.PathNotionContext},
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	if _, err := os.Stat(staleNotion); !os.IsNotExist(err) {
		t.Fatalf("omitted migrated notion context should be scrubbed, err=%v", err)
	}
	if _, err := os.Stat(brokerJira); err != nil {
		t.Fatalf("non-migrated jira context should be preserved: %v", err)
	}
}

func TestApplySecretsRuntimeContextRecreatesSymlinkedConfigDirs(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir()}
	if err := os.Symlink(outside, filepath.Join(workspace, "config")); err != nil {
		t.Fatalf("symlink config: %v", err)
	}
	err := server.applySecrets(SecretBundle{RuntimeContextProjection: runtimecontext.Projection{
		Files: []runtimecontext.File{{
			Path:    runtimecontext.PathGitHubContext,
			Content: `{"service":"github"}`,
		}},
		MigratedServicePaths:  []string{runtimecontext.PathGitHubContext},
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(workspace, "config")); err != nil {
		t.Fatalf("lstat config: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("config was not recreated as a real directory: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(outside, "oauth", "github.context.json")); !os.IsNotExist(err) {
		t.Fatalf("runtime context escaped through symlink, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "config", "oauth", "github.context.json")); err != nil {
		t.Fatalf("github context missing under workspace: %v", err)
	}
}

func TestApplySecretsRuntimeContextRecreatesSymlinkedOAuthDir(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	server := Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(workspace, "config"), 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "config", "oauth")); err != nil {
		t.Fatalf("symlink oauth: %v", err)
	}
	err := server.applySecrets(SecretBundle{RuntimeContextProjection: runtimecontext.Projection{
		Files: []runtimecontext.File{{
			Path:    runtimecontext.PathGitHubContext,
			Content: `{"service":"github"}`,
		}},
		MigratedServicePaths:  []string{runtimecontext.PathGitHubContext},
		PartialRolloutVersion: runtimecontext.PartialRolloutVersionGitHub,
	}})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(workspace, "config", "oauth")); err != nil {
		t.Fatalf("lstat oauth: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("oauth was not recreated as a real directory: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(outside, "github.context.json")); !os.IsNotExist(err) {
		t.Fatalf("runtime context escaped through oauth symlink, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "config", "oauth", "github.context.json")); err != nil {
		t.Fatalf("github context missing under workspace: %v", err)
	}
}

func TestToolExecRunsImageHelperCommandsThroughExec(t *testing.T) {
	workspace := t.TempDir()
	binDir := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(binDir, "browser-helper")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nprintf 'helper:%s' \"$PWD\"\nprintf 'visited' > browser.out\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	handler := Server{WorkspaceDir: workspace}.Handler()
	resp := postToolExec(t, handler, toolExecRequest{
		Operation:     toolOpExec,
		Command:       "./bin/browser-helper",
		Cwd:           ".",
		TimeoutMillis: 1000,
	})
	if resp.Error != "" || resp.ExitCode != 0 {
		t.Fatalf("helper command response = %#v", resp)
	}
	if resp.Stdout != "helper:"+workspace {
		t.Fatalf("stdout = %q", resp.Stdout)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "browser.out"))
	if err != nil {
		t.Fatalf("read helper output: %v", err)
	}
	if string(data) != "visited" {
		t.Fatalf("helper output = %q", string(data))
	}
}

func TestToolExecFileOperationsStayInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()

	resp := postToolExec(t, handler, toolExecRequest{Operation: toolOpMkdir, Path: "notes/nested", Recursive: true})
	if resp.Error != "" {
		t.Fatalf("mkdir error = %s", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpWriteFile, Path: "notes/nested/readme.txt", Content: "hello"})
	if resp.Error != "" {
		t.Fatalf("write error = %s", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpReadFile, Path: "notes/nested/readme.txt"})
	if resp.Error != "" || resp.Content != "hello" {
		t.Fatalf("read response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpWriteFile, Path: "notes/nested/blob.bin", ContentBase64: base64.StdEncoding.EncodeToString([]byte{0, 1, 2})})
	if resp.Error != "" {
		t.Fatalf("write buffer error = %s", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpReadFileBuffer, Path: "notes/nested/blob.bin"})
	if resp.Error != "" || resp.ContentBase64 != "AAEC" {
		t.Fatalf("read buffer response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpStat, Path: "notes/nested/readme.txt"})
	if resp.Error != "" || resp.Stat["type"] != "file" || resp.Stat["path"] != "notes/nested/readme.txt" {
		t.Fatalf("stat response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpReaddir, Path: "notes/nested"})
	if resp.Error != "" || len(resp.Entries) != 2 {
		t.Fatalf("readdir response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpExists, Path: "notes/nested/readme.txt"})
	if resp.Error != "" || resp.Exists == nil || !*resp.Exists {
		t.Fatalf("exists response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpRm, Path: "notes", Recursive: true})
	if resp.Error != "" {
		t.Fatalf("rm error = %s", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpExists, Path: "notes/nested/readme.txt"})
	if resp.Error != "" || resp.Exists == nil || *resp.Exists {
		t.Fatalf("exists after rm response = %#v", resp)
	}
}

func TestToolExecWriteFileAtomicallyReplacesAndCleansTemporaryFile(t *testing.T) {
	workspace := t.TempDir()
	targetDir := filepath.Join(workspace, "state")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "value.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := postToolExec(t, Server{WorkspaceDir: workspace}.Handler(), toolExecRequest{
		Operation: toolOpWriteFile,
		Path:      "state/value.txt",
		Content:   "durable",
	})
	if resp.Error != "" {
		t.Fatalf("write response = %#v", resp)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "durable" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 644", info.Mode().Perm())
	}
	assertNoTransferTempFiles(t, targetDir)
}

func TestToolExecSessionWarmPersistenceAcrossCalls(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()

	resp := postToolExec(t, handler, toolExecRequest{
		Operation: toolOpWriteFile,
		Path:      "state/counter.txt",
		Content:   "1\n",
	})
	if resp.Error != "" {
		t.Fatalf("write error = %s", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{
		Operation: toolOpExec,
		Command:   "sh",
		Args:      []string{"-c", "printf '2\\n' >> state/counter.txt"},
		Cwd:       ".",
	})
	if resp.Error != "" || resp.ExitCode != 0 {
		t.Fatalf("append command response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{
		Operation: toolOpReadFile,
		Path:      "state/counter.txt",
	})
	if resp.Error != "" || resp.Content != "1\n2\n" {
		t.Fatalf("state did not persist across tool calls: %#v", resp)
	}
}

func TestToolExecReadFileRejectsRawReadLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "too-large.txt"), bytes.Repeat([]byte("a"), int(sandboxlimits.ToolReadRawBytes)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := postToolExec(t, Server{WorkspaceDir: workspace}.Handler(), toolExecRequest{
		Operation: toolOpReadFile,
		Path:      "too-large.txt",
	})
	if !strings.Contains(resp.Error, "workspace file exceeds read limit") {
		t.Fatalf("response = %#v, want clean read-limit error", resp)
	}
}

func TestToolExecLargeWriteAndNearCeilingReadRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	large := strings.Repeat("w", 2<<20)
	resp := postToolExec(t, handler, toolExecRequest{
		Operation: toolOpWriteFile,
		Path:      "large-write.txt",
		Content:   large,
	})
	if resp.Error != "" {
		t.Fatalf("large write response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{
		Operation: toolOpReadFile,
		Path:      "large-write.txt",
	})
	if resp.Error != "" || resp.Content != large {
		t.Fatalf("large read response error=%q contentLen=%d", resp.Error, len(resp.Content))
	}

	nearCeiling := bytes.Repeat([]byte("r"), int(sandboxlimits.ToolReadRawBytes))
	if err := os.WriteFile(filepath.Join(workspace, "near-ceiling.txt"), nearCeiling, 0o644); err != nil {
		t.Fatal(err)
	}
	resp = postToolExec(t, handler, toolExecRequest{
		Operation: toolOpReadFile,
		Path:      "near-ceiling.txt",
	})
	if resp.Error != "" || len(resp.Content) != len(nearCeiling) {
		t.Fatalf("near-ceiling read response error=%q contentLen=%d want=%d", resp.Error, len(resp.Content), len(nearCeiling))
	}
}

func TestToolExecReadFileRejectsEscapedResponseOverControlLimit(t *testing.T) {
	workspace := t.TempDir()
	// NUL bytes are under the raw read ceiling but JSON-escape to six bytes
	// each, so the marshaled response exceeds the control-client cap.
	if err := os.WriteFile(filepath.Join(workspace, "escaped.bin"), bytes.Repeat([]byte{0}, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := postToolExec(t, Server{WorkspaceDir: workspace}.Handler(), toolExecRequest{
		Operation: toolOpReadFile,
		Path:      "escaped.bin",
	})
	if !strings.Contains(resp.Error, "readFile response exceeds control response limit") {
		t.Fatalf("response = %#v, want clean response-limit error", resp)
	}
}

func TestToolExecRejectsWorkspaceEscapes(t *testing.T) {
	handler := Server{WorkspaceDir: t.TempDir()}.Handler()
	tests := []toolExecRequest{
		{Operation: toolOpReadFile, Path: "../secret"},
		{Operation: toolOpReadFile, Path: "/etc/passwd"},
		{Operation: toolOpWriteFile, Path: "ok\x00bad", Content: "nope"},
		{Operation: toolOpExec, Command: "pwd", Cwd: "../secret"},
	}
	for _, req := range tests {
		resp := postToolExec(t, handler, req)
		if resp.Error == "" {
			t.Fatalf("request %#v should have failed", req)
		}
	}
}

func TestToolExecCommandTimeout(t *testing.T) {
	handler := Server{WorkspaceDir: t.TempDir()}.Handler()
	resp := postToolExec(t, handler, toolExecRequest{
		Operation:     toolOpExec,
		Command:       "sh",
		Args:          []string{"-c", "sleep 1"},
		TimeoutMillis: 50,
	})
	if !resp.TimedOut || resp.Error == "" {
		t.Fatalf("timeout response = %#v", resp)
	}
}

func TestToolExecCommandErrorShapes(t *testing.T) {
	handler := Server{WorkspaceDir: t.TempDir()}.Handler()
	resp := postToolExec(t, handler, toolExecRequest{
		Operation: toolOpExec,
		Command:   "sh",
		Args:      []string{"-c", "echo failed >&2; exit 7"},
	})
	if resp.Error != "" || resp.ExitCode != 7 || resp.Stderr != "failed\n" {
		t.Fatalf("nonzero response = %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{
		Operation: toolOpExec,
		Command:   filepath.Join(t.TempDir(), "missing-command"),
	})
	if resp.Error == "" || resp.ExitCode != -1 {
		t.Fatalf("spawn failure response = %#v", resp)
	}
}

func TestToolExecFileOperationErrorShapes(t *testing.T) {
	workspace := t.TempDir()
	handler := Server{WorkspaceDir: workspace}.Handler()
	resp := postToolExec(t, handler, toolExecRequest{Operation: toolOpReadFile, Path: "missing.txt"})
	if resp.Error != "workspace file not found" {
		t.Fatalf("missing read error = %q", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpStat, Path: "missing.txt"})
	if resp.Error != "workspace path not found" {
		t.Fatalf("missing stat error = %q", resp.Error)
	}
	if err := os.WriteFile(filepath.Join(workspace, "plain.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpReaddir, Path: "plain.txt"})
	if resp.Error == "" || strings.Contains(resp.Error, "workspace path not found") {
		t.Fatalf("readdir non-not-found error = %q", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpStat, Path: strings.Repeat("x", 5000)})
	if resp.Error == "" || strings.Contains(resp.Error, "workspace path not found") {
		t.Fatalf("stat non-not-found error = %q", resp.Error)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpRm, Path: "missing.txt"})
	if resp.Error == "" {
		t.Fatalf("missing rm should fail: %#v", resp)
	}
	resp = postToolExec(t, handler, toolExecRequest{Operation: toolOpRm, Path: "missing.txt", Force: true})
	if resp.Error != "" {
		t.Fatalf("forced missing rm should not fail: %#v", resp)
	}
}

type fakeFreezer struct {
	freezePath string
	thawPath   string
}

func (f *fakeFreezer) Freeze(_ context.Context, workspaceDir string) error {
	f.freezePath = workspaceDir
	return nil
}

func (f *fakeFreezer) Thaw(_ context.Context, workspaceDir string) error {
	f.thawPath = workspaceDir
	return nil
}

type fakeHardener struct {
	input RestoreHardenInput
}

func (f *fakeHardener) Harden(_ context.Context, input RestoreHardenInput) error {
	f.input = input
	return nil
}

func postToolExec(t *testing.T, handler http.Handler, req toolExecRequest) toolExecResponse {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tool/exec", bytes.NewReader(data)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp toolExecResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

func assertNoTransferTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("left temp file %s in %s", entry.Name(), dir)
		}
	}
}
