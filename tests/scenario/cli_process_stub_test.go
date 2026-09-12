package scenario_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// This checks the actual binary, argument parsing, session isolation, guest
// streams and exit status without claiming to verify a compute backend.
func TestScenarioCLIProcessAgainstStub(t *testing.T) {
	const token = "scenario-stub-application-token"
	const name = "cli-stub-box"
	sandbox := sb.Sandbox{ID: "sbx_cli_stub", Profile: "scenario-cli-target-shape", State: sb.SandboxStateReady, Generation: 1, Revision: 2,
		Metadata: sb.Metadata{contracts.SandboxNameMetadataKey: name}, Resources: sb.SandboxResources{VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 64 << 20}}
	var mutex sync.Mutex
	var uploaded []byte
	var creates, executions, refusals int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-SecondBox-Tenant-Ref") != "stub-tenant" || r.Header.Get("X-SecondBox-Subject-Ref") != "stub-subject" {
			t.Error("SecondBox scenario CLI sent incorrect application authority")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Errorf("SecondBox scenario stub response: %v", err)
			}
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
			var request sb.CreateSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if request.Resources == nil || request.Resources.VCPUCount == nil {
				t.Error("SecondBox scenario missing requested resources")
				w.WriteHeader(400)
				return
			}
			if *request.Resources.VCPUCount == 999 {
				refusals++
				w.WriteHeader(http.StatusBadRequest)
				write(map[string]any{"status": 400, "code": "resources_exceed_profile", "title": "Requested resources exceed the Profile ceiling", "ceiling": sandbox.Resources})
				return
			}
			creates++
			if request.Profile != sandbox.Profile || request.Metadata[contracts.SandboxNameMetadataKey] != name || *request.Resources.VCPUCount != 1 || request.Resources.MemoryBytes == nil || *request.Resources.MemoryBytes != 1<<30 {
				t.Errorf("SecondBox scenario run request: %+v", request)
			}
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("SecondBox scenario missing create idempotency")
			}
			write(sb.Operation{ID: "op_cli_create", SandboxID: sandbox.ID, State: "pending"})
		case r.Method == "GET" && r.URL.Path == "/v1/sandboxes":
			if r.URL.Query().Get("metadata") != contracts.SandboxNameMetadataKey+"="+name {
				t.Errorf("SecondBox scenario name query: %s", r.URL.RawQuery)
			}
			write(sb.SandboxPage{Items: []sb.Sandbox{sandbox}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec"):
			executions++
			var request sb.BufferedExecRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			command := request.Command.ArgvCommand
			if command == nil {
				t.Error("SecondBox scenario expected argv command")
				w.WriteHeader(400)
				return
			}
			stdout, stderr, code := "", "", 0
			switch command.Executable {
			case "/bin/sh":
				if len(command.Arguments) != 2 || command.Arguments[0] != "-c" || command.Arguments[1] != "echo hello" {
					t.Errorf("SecondBox scenario shell arguments: %v", command.Arguments)
				}
				stdout = "hello\n"
			case "cat":
				if len(command.Arguments) != 1 || command.Arguments[0] != "/workspace/in.txt" {
					t.Errorf("SecondBox scenario cat arguments: %v", command.Arguments)
				}
				stdout = string(uploaded)
			case "/bin/false":
				code, stderr = 7, "guest refused\n"
			default:
				t.Errorf("SecondBox scenario unexpected executable: %s", command.Executable)
				w.WriteHeader(400)
				return
			}
			write(map[string]any{"kind": "exited", "exitCode": code, "elapsedMilliseconds": 1, "output": map[string]string{"stdoutBase64": base64.StdEncoding.EncodeToString([]byte(stdout)), "stderrBase64": base64.StdEncoding.EncodeToString([]byte(stderr))}})
		case strings.HasSuffix(r.URL.Path, "/files:exists"):
			write(map[string]bool{"exists": false})
		case strings.HasSuffix(r.URL.Path, "/files:stat"):
			write(map[string]any{"path": "in.txt", "kind": "file", "sizeBytes": len(uploaded)})
		case strings.HasSuffix(r.URL.Path, "/files"):
			if r.URL.Query().Get("path") != "in.txt" || r.Header.Get("SecondBox-Generation") != "1" {
				t.Error("SecondBox scenario incorrect file path or generation")
			}
			if r.Method == "PUT" {
				var err error
				uploaded, err = io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				digest := sha256.Sum256(uploaded)
				if r.Header.Get("Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":" {
					t.Error("SecondBox scenario incorrect upload digest")
				}
				write(contracts.FileWriteResult{Path: "in.txt", SizeBytes: int64(len(uploaded))})
			} else {
				if _, err := w.Write(uploaded); err != nil {
					t.Error(err)
				}
			}
		case r.Method == "GET" && (r.URL.Path == "/v1/sandboxes/"+sandbox.ID || r.URL.Path == "/v1/sandboxes/"+sandbox.ID+":wait"):
			write(sandbox)
		default:
			t.Errorf("SecondBox scenario unexpected stub request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	t.Setenv("SECONDBOX_URL", "http://ambient.invalid")
	t.Setenv("SECONDBOX_TOKEN", "ambient-platform-token")
	t.Setenv("SECONDBOX_AUTHORITY_KIND", "platform")
	t.Setenv("SECONDBOX_CONFIG", filepath.Join(t.TempDir(), "ambient.json"))
	cli := newScenarioCLI(t, server.URL, token, "stub-tenant", "stub-subject")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if got := cli.success(t, ctx, "run", sandbox.Profile, "--keep", "--name", name, "--cpus", "1", "--memory", "1GiB", "--", "/bin/sh", "-c", "echo hello"); got != "hello\n" {
		t.Fatalf("SecondBox scenario run stdout=%q", got)
	}
	got := scenarioCLIJSON[sb.Sandbox](t, cli.success(t, ctx, "--output", "json", "get", name))
	if got.ID != sandbox.ID || got.Resources.VCPUCount != 1 {
		t.Fatalf("SecondBox scenario get: %+v", got)
	}
	refused := cli.run(t, ctx, "run", sandbox.Profile, "--cpus", "999", "--", "true")
	if refused.exitCode == 0 || refused.stdout != "" || !strings.Contains(refused.stderr, "resources_exceed_profile") || !strings.Contains(refused.stderr, "ceiling") {
		t.Fatalf("SecondBox scenario refusal: %+v", refused)
	}
	payload := []byte("hello\x00world\n")
	input, output := filepath.Join(t.TempDir(), "in.txt"), filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(input, payload, 0600); err != nil {
		t.Fatal(err)
	}
	write := scenarioCLIJSON[contracts.FileWriteResult](t, cli.success(t, ctx, "cp", input, name+":/workspace/in.txt"))
	if write.SizeBytes != int64(len(payload)) {
		t.Fatalf("SecondBox scenario upload: %+v", write)
	}
	if got := cli.success(t, ctx, "exec", name, "--", "cat", "/workspace/in.txt"); got != string(payload) {
		t.Fatalf("SecondBox scenario exec stdout=%q", got)
	}
	if got := cli.success(t, ctx, "cp", name+":/workspace/in.txt", output); got != "" {
		t.Fatalf("SecondBox scenario download stdout=%q", got)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("SecondBox scenario download bytes=%q", data)
	}
	failed := cli.run(t, ctx, "exec", name, "--", "/bin/false")
	if failed.exitCode != 7 || failed.stdout != "" || failed.stderr != "guest refused\n" {
		t.Fatalf("SecondBox scenario guest failure: %+v", failed)
	}
	canceled, cancelCommand := context.WithCancel(ctx)
	cancelCommand()
	if _, err := cli.invoke(canceled, "get", name); err != context.Canceled {
		t.Fatalf("SecondBox scenario canceled subprocess: %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if creates != 1 || refusals != 1 || executions != 3 {
		t.Fatalf("SecondBox scenario stub creates=%d refusals=%d executions=%d", creates, refusals, executions)
	}
}
