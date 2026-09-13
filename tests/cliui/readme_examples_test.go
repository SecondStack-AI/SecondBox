package cliui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute each README CLI example through the built parser, stopping at the
// first API call. Lifecycle and stream tests qualify behavior separately.
func TestREADMECLIExamplesReachTransport(t *testing.T) {
	binary, _, working := buildBinaries(t)
	readme, err := os.ReadFile(filepath.Join(repositoryRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(readme), "## Using the CLI\n")
	if !ok {
		t.Fatal("missing CLI section")
	}
	section, _, ok = strings.Cut(section, "## SDKs\n")
	if !ok {
		t.Fatal("missing SDK boundary")
	}
	if err := os.Mkdir(filepath.Join(working, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(working, "src", "input.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	const marker = "README CLI example reached transport"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"authorization_denied","title":"`+marker+`"}`)
	}))
	defer server.Close()
	count := 0
	inShellBlock := false
	for _, line := range strings.Split(section, "\n") {
		if line == "```sh" {
			inShellBlock = true
			continue
		}
		if line == "```" {
			inShellBlock = false
			continue
		}
		if !inShellBlock || !strings.HasPrefix(line, "secondbox ") {
			continue
		}
		count++
		t.Run(line, func(t *testing.T) {
			script := `secondbox() {
 "$SECONDBOX_DOC_TEST_BINARY" --url "$SECONDBOX_DOC_TEST_URL" --token doc-test --authority-kind platform --tenant-ref doc-tenant --subject-ref doc-subject "$@"
}
` + line
			command := exec.Command("sh", "-c", script)
			command.Dir = working
			command.Env = append(os.Environ(), "SECONDBOX_DOC_TEST_BINARY="+binary, "SECONDBOX_DOC_TEST_URL="+server.URL, "SECONDBOX_CONFIG="+filepath.Join(working, "no-session.json"), "TERM=dumb")
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), marker) {
				t.Fatalf("example did not reach API: %v\n%s", err, output)
			}
		})
	}
	if count == 0 {
		t.Fatal("no executable CLI examples found")
	}
}
