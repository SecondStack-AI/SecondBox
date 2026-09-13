package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestCopyFilesAndDirectories(t *testing.T) {
	payload := []byte("hello\x00world\n")
	digest := sha256.Sum256(payload)
	uploaded := map[string][]byte{}
	directories := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := r.URL.Query().Get("path")
		switch {
		case strings.HasSuffix(r.URL.Path, "/files:exists"):
			assertVerbHeaders(t, r, false, true, false)
			_, _ = io.WriteString(w, `{"exists":false}`)
		case r.URL.Path == "/v1/sandboxes":
			_, _ = io.WriteString(w, `{"items":[`+execSandboxJSON+`]}`)
		case strings.HasSuffix(r.URL.Path, "/files") && r.Method == "PUT":
			assertVerbHeaders(t, r, false, true, true)
			content, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			uploaded[remote] = content
			if r.Header.Get("Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":" {
				t.Errorf("digest=%s", r.Header.Get("Digest"))
			}
			_, _ = io.WriteString(w, ` {"path":"`+remote+`","sizeBytes":12} `)
		case strings.HasSuffix(r.URL.Path, "/files"):
			assertVerbHeaders(t, r, false, true, false)
			_, _ = w.Write(payload)
		case strings.HasSuffix(r.URL.Path, "/files:stat"):
			assertVerbHeaders(t, r, false, true, false)
			kind := "file"
			if remote == "src" {
				kind = "directory"
			}
			_, _ = io.WriteString(w, `{"path":"`+remote+`","kind":"`+kind+`","sizeBytes":12}`)
		case strings.HasSuffix(r.URL.Path, "/directories") && r.Method == "GET":
			assertVerbHeaders(t, r, false, true, false)
			_, _ = io.WriteString(w, `{"path":"src","entries":[{"path":"src/file","kind":"file","sizeBytes":12}]}`)
		case strings.HasSuffix(r.URL.Path, "/directories") && r.Method == "POST":
			assertVerbHeaders(t, r, false, true, true)
			var request sb.CreateDirectoryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			directories = append(directories, request.Path)
			w.WriteHeader(204)
		default:
			_, _ = io.WriteString(w, execSandboxJSON)
		}
	}))
	defer server.Close()
	session := verbTestSession(server)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, recursive := range []bool{false, true} {
		var output bytes.Buffer
		source, destination := filepath.Join(local, "file"), "mybox:/workspace/file"
		args := []string{source, destination}
		if recursive {
			args = []string{"-r", local, "sbx_test1:/workspace/src"}
		}
		if err := runCopyVerb(t.Context(), session, args, &output, server.Client()); err != nil {
			t.Fatal(err)
		}
		remote := "file"
		if recursive {
			remote = "src/file"
		}
		if !bytes.Equal(uploaded[remote], payload) {
			t.Errorf("uploaded=%v", uploaded)
		}
		download := filepath.Join(t.TempDir(), "out")
		args = []string{destination, download}
		if recursive {
			args = []string{"mybox:/workspace/src", download, "-r"}
		}
		if err := runCopyVerb(t.Context(), session, args, &output, server.Client()); err != nil {
			t.Fatal(err)
		}
		if recursive {
			download = filepath.Join(download, "file")
		}
		content, err := os.ReadFile(download)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, payload) {
			t.Errorf("download=%q", content)
		}
	}
	if len(directories) != 1 || directories[0] != "src" {
		t.Errorf("directories=%v", directories)
	}
	var output bytes.Buffer
	if err := runListFilesVerb(t.Context(), session, []string{"mybox", "/workspace/src"}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"src/file"`) {
		t.Errorf("listing=%s", output.String())
	}
	if err := runCopyVerb(t.Context(), session, []string{local, "mybox:/workspace/src"}, &output, server.Client()); err == nil || !strings.Contains(err.Error(), "-r") {
		t.Errorf("directory without -r: %v", err)
	}
}

func TestCopyRejectsInvalidOperandsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"a:/workspace/file", "b:/workspace/file"}, {"a", "b"}, {"a:relative", "b"}, {"a:/etc/passwd", "b"}, {"a:/workspace/../etc/passwd", "b"}, {"a:/workspace/file"},
	} {
		if err := runCopyVerb(t.Context(), cliSession{}, args, io.Discard, http.DefaultClient); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
