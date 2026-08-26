package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/creack/pty"
)

// newSessionEnvironment isolates one test from the developer's own credentials.
func newSessionEnvironment(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(sessionPathEnvironment, path)
	t.Setenv(sessionURLEnvironment, "")
	t.Setenv(sessionTokenEnvironment, "")
	t.Setenv(sessionAuthorityEnvironment, "")
	t.Setenv(sessionTenantRefEnvironment, "")
	t.Setenv(sessionSubjectRefEnvironment, "")
	return path
}

func writeTestSessionFile(t *testing.T, path string, stored sessionFile, mode fs.FileMode) {
	t.Helper()
	content, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSessionPrefersFlagsThenEnvironmentThenConfiguration(t *testing.T) {
	path := newSessionEnvironment(t)
	writeTestSessionFile(t, path, sessionFile{
		URL:        "https://configuration.example.com",
		Token:      "configuration-token",
		TenantRef:  "configuration-tenant",
		SubjectRef: "configuration-subject",
	}, 0o600)
	t.Setenv(sessionTokenEnvironment, "environment-token")
	t.Setenv(sessionTenantRefEnvironment, "environment-tenant")

	session, err := resolveSession(cliSession{url: "https://flag.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if session.url != "https://flag.example.com" || session.origins.url != sessionOriginFlag {
		t.Errorf("url = %q (%s); want the flag value", session.url, session.origins.url)
	}
	if session.token != "environment-token" || session.origins.token != sessionOriginEnvironment {
		t.Errorf("token origin = %s; want the environment value", session.origins.token)
	}
	if session.tenantRef != "environment-tenant" ||
		session.origins.tenantRef != sessionOriginEnvironment {
		t.Errorf("tenantRef = %q (%s); want the environment value", session.tenantRef, session.origins.tenantRef)
	}
	if session.subjectRef != "configuration-subject" ||
		session.origins.subjectRef != sessionOriginConfiguration {
		t.Errorf("subjectRef = %q (%s); want the configuration value", session.subjectRef, session.origins.subjectRef)
	}
	if session.path != path {
		t.Errorf("path = %q; want %q", session.path, path)
	}
}

func TestResolveSessionWithoutConfigurationReportsUnsetValues(t *testing.T) {
	newSessionEnvironment(t)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatalf("absent configuration must not fail: %v", err)
	}
	origins := []sessionOrigin{
		session.origins.url, session.origins.token,
		session.origins.tenantRef, session.origins.subjectRef,
	}
	for _, origin := range origins {
		if origin != sessionOriginUnset {
			t.Errorf("origin = %s; want %s", origin, sessionOriginUnset)
		}
	}
}

func TestResolveSessionTrimsSurroundingWhitespace(t *testing.T) {
	newSessionEnvironment(t)
	t.Setenv(sessionURLEnvironment, "  https://spaced.example.com  ")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	if session.url != "https://spaced.example.com" {
		t.Errorf("url = %q; want the trimmed value", session.url)
	}
}

func TestResolveSessionRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, path string)
		want    string
	}{
		{
			name: "group or other readable",
			prepare: func(t *testing.T, path string) {
				writeTestSessionFile(t, path, sessionFile{URL: "https://a.example.com"}, 0o644)
			},
			want: "must not be readable by group or other",
		},
		{
			name: "symbolic link",
			prepare: func(t *testing.T, path string) {
				target := filepath.Join(filepath.Dir(path), "target.json")
				writeTestSessionFile(t, target, sessionFile{URL: "https://a.example.com"}, 0o600)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "must be a non-symbolic-link regular file",
		},
		{
			name: "unknown field",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"url":"https://a.example.com","extra":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "read configuration",
		},
		{
			name: "trailing content",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"url":"https://a.example.com"} {"url":"https://b.example.com"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "read configuration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := newSessionEnvironment(t)
			test.prepare(t, path)
			_, err := resolveSession(cliSession{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveSession error = %v; want one containing %q", err, test.want)
			}
		})
	}
}

func TestResolveSessionPathRejectsRelativeOverride(t *testing.T) {
	newSessionEnvironment(t)
	t.Setenv(sessionPathEnvironment, "relative/config.json")
	if _, err := resolveSession(cliSession{}); err == nil ||
		!strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("resolveSession error = %v; want an absolute-path rejection", err)
	}
}

func TestResolveSessionPathDefaultsToUserConfigurationDirectory(t *testing.T) {
	newSessionEnvironment(t)
	t.Setenv(sessionPathEnvironment, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	directory, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("user configuration directory is unavailable: %v", err)
	}
	path, err := resolveSessionPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(directory, "secondbox", "config.json"); path != want {
		t.Errorf("resolveSessionPath() = %q; want %q", path, want)
	}
}

// newVerifyingSessionServer accepts exactly the credential probe login issues.
func newVerifyingSessionServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/v1/sandboxes" || request.URL.Query().Get("limit") != "1" {
				t.Errorf("unexpected probe %s %s", request.Method, request.URL)
			}
			if request.Header.Get("Authorization") != "Bearer "+token {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"code":"unauthenticated"}`))
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"items":[]}`))
		},
	))
	t.Cleanup(server.Close)
	return server
}

func TestLoginVerifiesAndStoresCredentials(t *testing.T) {
	path := newSessionEnvironment(t)
	server := newVerifyingSessionServer(t, "good-token")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLoginCommand(context.Background(), session, []string{
		"--url", server.URL, "--token", "good-token",
		"--tenant-ref", "tenant-1", "--subject-ref", "subject-1",
	}, &output, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("configuration mode = %v; want 0600", info.Mode().Perm())
	}
	stored, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sessionFile{
		URL: server.URL, Token: "good-token",
		Authority: string(sessionAuthorityApplication),
		TenantRef: "tenant-1", SubjectRef: "subject-1",
	}
	if stored != want {
		t.Errorf("stored = %+v; want %+v", stored, want)
	}
	if !strings.Contains(output.String(), path) {
		t.Errorf("login output = %q; want it to name %q", output.String(), path)
	}
}

func TestPresentedLoginReportsStoredEqualsFormURL(t *testing.T) {
	path := newSessionEnvironment(t)
	server := newVerifyingSessionServer(t, "good-token")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	capabilities := cliui.ForWriter(&output, io.Discard)
	ctx := withPresentation(context.Background(), presentation{renderer: cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}})
	args := []string{"--url=" + server.URL, "--token=good-token", "--tenant-ref=tenant-1", "--subject-ref=subject-1"}
	if err := runLoginCommandPresented(ctx, session, args, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["url"] != server.URL || result["configuration"] != path {
		t.Fatalf("presented login = %#v", result)
	}
}

func TestLoginRejectedCredentialsAreNotStored(t *testing.T) {
	path := newSessionEnvironment(t)
	server := newVerifyingSessionServer(t, "good-token")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLoginCommand(context.Background(), session, []string{
		"--url", server.URL, "--token", "wrong-token",
		"--tenant-ref", "tenant-1", "--subject-ref", "subject-1",
	}, &output, server.Client())
	if err == nil || !strings.Contains(err.Error(), "verify credentials") {
		t.Fatalf("login error = %v; want a verification failure", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("configuration must not exist after a rejected login: %v", err)
	}
}

func TestPlatformLoginRejectsControllerCredentialWithTypedError(t *testing.T) {
	path := newSessionEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/tenants" {
			t.Errorf("platform verification path = %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"code":"authority_kind_forbidden","title":"wrong authority kind"}`))
	}))
	defer server.Close()
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	err = runAuthorityLoginCommand(context.Background(), session, sessionAuthorityPlatform, []string{"--url", server.URL, "--token", "controller-token"}, io.Discard, server.Client())
	var mismatch *cliSessionAuthorityError
	if !errors.As(err, &mismatch) || mismatch.Required != sessionAuthorityPlatform {
		t.Fatalf("platform login mismatch = %#v, %v", mismatch, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("wrong credential kind was stored: %v", err)
	}
}

func TestControllerLoginStoresOnlyTypedControllerCredential(t *testing.T) {
	path := newSessionEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/subjects" || request.Header.Get("Authorization") != "Bearer controller-token" {
			t.Errorf("controller verification request = %s %#v", request.URL.Path, request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runAuthorityLoginCommand(context.Background(), session, sessionAuthorityTenantController, []string{"--url", server.URL, "--token", "controller-token"}, io.Discard, server.Client()); err != nil {
		t.Fatal(err)
	}
	stored, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Authority != string(sessionAuthorityTenantController) || stored.TenantRef != "" || stored.SubjectRef != "" {
		t.Fatalf("stored controller session = %#v", stored)
	}
}

func TestLoginReplacesExistingConfigurationAtomically(t *testing.T) {
	path := newSessionEnvironment(t)
	writeTestSessionFile(t, path, sessionFile{
		URL: "https://stale.example.com", Token: "stale-token",
		TenantRef: "stale-tenant", SubjectRef: "stale-subject",
	}, 0o600)
	server := newVerifyingSessionServer(t, "good-token")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLoginCommand(context.Background(), session, []string{
		"--url", server.URL, "--token", "good-token",
		"--tenant-ref", "tenant-2", "--subject-ref", "subject-2",
	}, &output, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TenantRef != "tenant-2" || stored.Token != "good-token" {
		t.Errorf("stored = %+v; want the replacement credentials", stored)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("configuration directory = %v; want only the replaced file", entries)
	}
}

// TestLoginInheritsResolvedValues proves login can persist environment credentials.
func TestLoginInheritsResolvedValues(t *testing.T) {
	path := newSessionEnvironment(t)
	server := newVerifyingSessionServer(t, "good-token")
	t.Setenv(sessionURLEnvironment, server.URL)
	t.Setenv(sessionTokenEnvironment, "good-token")
	t.Setenv(sessionTenantRefEnvironment, "tenant-3")
	t.Setenv(sessionSubjectRefEnvironment, "subject-3")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runLoginCommand(context.Background(), session, nil, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	stored, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TenantRef != "tenant-3" || stored.SubjectRef != "subject-3" {
		t.Errorf("stored = %+v; want the environment credentials", stored)
	}
}

func TestLoginRequiresEveryCredential(t *testing.T) {
	newSessionEnvironment(t)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLoginCommand(context.Background(), session, []string{
		"--url", "https://a.example.com", "--token", "token-1",
	}, &output, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "login requires") {
		t.Fatalf("login error = %v; want a missing-credential rejection", err)
	}
}

func TestLoginAccessibleFormPromptsOnlyForMissingValues(t *testing.T) {
	path := newSessionEnvironment(t)
	server := newVerifyingSessionServer(t, "good-token")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	capabilities := cliui.ForWriter(&output, io.Discard)
	capabilities.Input.TTY = true
	capabilities.Output.TTY = true
	capabilities.Output.Width = 80
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	writeDone := make(chan error, 1)
	go func() { _, err := io.WriteString(master, "good-token\ntenant-form\nsubject-form\n"); writeDone <- err }()
	ctx := withPresentation(context.Background(), presentation{renderer: cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: cliui.OutputAuto, ColorMode: cliui.ColorNever}, accessible: true, input: input})
	if err := runLoginCommand(ctx, session, []string{"--url", server.URL}, &output, server.Client()); err != nil {
		t.Fatalf("%v; transcript %q", err, output.String())
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	stored, err := readSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != "good-token" || stored.TenantRef != "tenant-form" || stored.SubjectRef != "subject-form" {
		t.Fatalf("stored form values = %#v", stored)
	}
	transcript := output.String()
	if strings.Contains(transcript, "API endpoint") {
		t.Fatalf("form prompted for supplied URL: %q", transcript)
	}
	for _, prompt := range []string{"Application token", "Tenant reference", "Subject reference", path} {
		if !strings.Contains(transcript, prompt) {
			t.Fatalf("form transcript lacks %q: %q", prompt, transcript)
		}
	}
	if strings.Contains(transcript, "good-token") {
		t.Fatalf("secret appeared in transcript: %q", transcript)
	}
}

func TestLoginRejectsUnexpectedArguments(t *testing.T) {
	newSessionEnvironment(t)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLoginCommand(context.Background(), session, []string{"extra"}, &output, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "unexpected login arguments") {
		t.Fatalf("login error = %v; want an unexpected-argument rejection", err)
	}
}

func TestLogoutRemovesConfigurationAndIsIdempotent(t *testing.T) {
	path := newSessionEnvironment(t)
	writeTestSessionFile(t, path, sessionFile{URL: "https://a.example.com"}, 0o600)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runLogoutCommand(session, nil, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("configuration must be removed: %v", err)
	}
	if !strings.Contains(output.String(), "removed stored credentials") {
		t.Errorf("logout output = %q; want a removal report", output.String())
	}
	output.Reset()
	if err := runLogoutCommand(session, nil, &output); err != nil {
		t.Fatalf("repeated logout must succeed: %v", err)
	}
	if !strings.Contains(output.String(), "no stored credentials") {
		t.Errorf("repeated logout output = %q; want an absent-credential report", output.String())
	}
}

func TestWhoamiReportsOriginsAndNeverPrintsTheToken(t *testing.T) {
	path := newSessionEnvironment(t)
	writeTestSessionFile(t, path, sessionFile{
		URL: "https://configuration.example.com", Token: "secret-token-value",
		TenantRef: "tenant-4", SubjectRef: "subject-4",
	}, 0o600)
	t.Setenv(sessionURLEnvironment, "https://environment.example.com")
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWhoamiCommand(session, nil, &output); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if strings.Contains(rendered, "secret-token-value") {
		t.Fatalf("whoami output discloses the token: %q", rendered)
	}
	for _, want := range []string{
		path,
		"https://environment.example.com (environment)",
		"tenant-4 (configuration)",
		"subject-4 (configuration)",
		"token          present (configuration)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("whoami output = %q; want it to contain %q", rendered, want)
		}
	}
}

func TestWhoamiReportsUnsetValues(t *testing.T) {
	newSessionEnvironment(t)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWhoamiCommand(session, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "url            unset (unset)") ||
		!strings.Contains(output.String(), "token          absent (unset)") {
		t.Errorf("whoami output = %q; want unset reporting", output.String())
	}
}

func TestRunOperationalCommandRoutesSessionCommands(t *testing.T) {
	path := newSessionEnvironment(t)
	writeTestSessionFile(t, path, sessionFile{
		URL: "https://a.example.com", Token: "token-5",
		TenantRef: "tenant-5", SubjectRef: "subject-5",
	}, 0o600)
	session, err := resolveSession(cliSession{})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"whoami", "logout"} {
		var output bytes.Buffer
		handled, err := runOperationalCommand(
			context.Background(), session, []string{command}, &output,
		)
		if !handled {
			t.Fatalf("%s must be handled as an operational command", command)
		}
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
}

// TestRunReportsEveryCredentialSource proves the guidance names all three sources.
func TestRunReportsEveryCredentialSource(t *testing.T) {
	newSessionEnvironment(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{"sandboxes", "list"}, &output)
	if err == nil || !strings.Contains(err.Error(), sessionSourceHint) {
		t.Fatalf("run error = %v; want guidance naming every credential source", err)
	}
}
