package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestParseByteSize(t *testing.T) {
	for text, want := range map[string]int64{"1": 1, "9223372036854775807": 9223372036854775807, "4GiB": 4 << 30, "4gIb": 4 << 30, "8m": 8 << 20, "2MiB": 2 << 20, "3K": 3 << 10, "7kiB": 7 << 10, "2G": 2 << 30} {
		got, err := parseByteSize(text)
		if err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "0", "-1", "+1", "1.5GiB", "1GB", " 1", "1 ", "GiB", "9223372036854775808", "8589934592GiB"} {
		if _, err := parseByteSize(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestResourceOptions(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		want    string
		invalid bool
	}{
		{name: "omitted", want: "null"},
		{name: "partial", args: []string{"--memory", "4GiB"}, want: `{"memoryBytes":4294967296}`},
		{name: "small", args: []string{"--size", "small"}, want: `{"vcpuCount":1,"memoryBytes":1073741824,"workspaceBytes":4294967296}`},
		{name: "medium", args: []string{"--size", "medium"}, want: `{"vcpuCount":2,"memoryBytes":4294967296,"workspaceBytes":17179869184}`},
		{name: "override before preset", args: []string{"--cpus", "2", "--memory", "4g", "--disk", "20g", "--size", "large"}, want: `{"vcpuCount":2,"memoryBytes":4294967296,"workspaceBytes":21474836480}`},
		{name: "unknown", args: []string{"--size", "huge"}, invalid: true},
		{name: "empty size", args: []string{"--size="}, invalid: true},
		{name: "empty memory", args: []string{"--memory="}, invalid: true},
		{name: "zero cpus", args: []string{"--cpus", "0"}, invalid: true},
		{name: "fractional cpus", args: []string{"--cpus", "1.5"}, invalid: true},
		{name: "negative disk", args: []string{"--disk", "-1"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags := verbFlags("create")
			var options resourceOptions
			options.register(flags)
			if err := flags.Parse(test.args); err != nil {
				t.Fatal(err)
			}
			request, err := options.resolve(flags)
			if test.invalid {
				if err == nil {
					t.Fatal("accepted invalid resources")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(request)
			if err != nil || string(raw) != test.want {
				t.Fatalf("resources = %s, %v; want %s", raw, err, test.want)
			}
		})
	}
}

func TestRunAndCreateSendResources(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
			args := []string{"durable-coding", "--size", "large", "--cpus", "2", "--memory", "4GiB", "--disk", "20GiB"}
			var err error
			if command == "run" {
				_, _, err = invokeRun(t, recorder, append(args, "--", "true"))
			} else {
				err = runLifecycleVerb(t.Context(), execTestSession(recorder.server.URL), command, args, io.Discard, recorder.server.Client())
			}
			if err != nil {
				t.Fatal(err)
			}
			resource := recorder.create.Resources
			if resource == nil || *resource.VCPUCount != 2 || *resource.MemoryBytes != 4<<30 || *resource.WorkspaceBytes != 20<<30 {
				t.Fatalf("request = %+v", resource)
			}
		})
	}
}

func TestCreationCeilingProblemPresentation(t *testing.T) {
	for _, command := range []string{"run", "create", "interactive"} {
		t.Run(command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"code":"resources_exceed_profile","title":"Requested resources exceed Profile","ceiling":{"vcpuCount":4,"memoryBytes":8589934592,"workspaceBytes":53687091200},"requested":{"vcpuCount":8,"memoryBytes":8589934592,"workspaceBytes":53687091200}}`)
			}))
			defer server.Close()
			var err error
			if command == "create" {
				err = runLifecycleVerb(t.Context(), verbTestSession(server), command, []string{"durable-coding", "--cpus", "8"}, io.Discard, server.Client())
			} else {
				args := []string{"durable-coding", "--cpus", "8", "--", "true"}
				if command == "interactive" {
					args = []string{"durable-coding", "--cpus", "8", "--tty"}
				}
				err = runRunCommand(t.Context(), verbTestSession(server), args, execCommandEnvironment{stdout: io.Discard, stderr: io.Discard, httpClient: server.Client()}, sandboxShellEnvironment{})
			}
			var api *sb.APIError
			if !errors.As(err, &api) || api.Problem.Code != sb.ProblemCodeResourcesExceedProfile {
				t.Fatalf("error = %v", err)
			}
			var diagnostic bytes.Buffer
			renderer := cliui.Renderer{Output: io.Discard, Diagnostic: &diagnostic, Capabilities: cliui.ForWriter(io.Discard, &diagnostic), OutputMode: cliui.OutputPlain}
			presented := &commandPresentationError{cause: err, renderer: renderer}
			if err := renderer.WriteError(presented, presented.hint()); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"resources_exceed_profile", "Profile", "durable-coding", "--cpus 4 --memory 8589934592 --disk 34359738368"} {
				if !strings.Contains(diagnostic.String(), want) {
					t.Errorf("diagnostic %q lacks %q", diagnostic.String(), want)
				}
			}
		})
	}
}

func TestResolvedResourceViews(t *testing.T) {
	sandbox := sb.Sandbox{ID: "sbx_retained", Resources: sb.SandboxResources{VCPUCount: 2, MemoryBytes: 4 << 30, WorkspaceBytes: 20 << 30}}
	var output bytes.Buffer
	capabilities := cliui.ForWriter(&output, &output)
	capabilities.Diagnostic.TTY = true
	renderer := cliui.Renderer{Output: &output, Diagnostic: &output, Capabilities: capabilities, OutputMode: cliui.OutputPlain}
	ctx := withPresentation(t.Context(), presentation{renderer: renderer})
	raw, err := json.Marshal(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderBoundedOperation("getSandbox", raw, renderer); err != nil {
		t.Fatal(err)
	}
	if err := writeRetainedSandbox(ctx, &output, sandbox); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), sandboxResourceSummary(sandbox.Resources)) != 2 || !strings.Contains(output.String(), "requested disk rounds up to a power of two") {
		t.Fatalf("views = %q", output.String())
	}
}

func TestCreationResourceProblemHints(t *testing.T) {
	pointer := func(value int64) *int64 { return &value }
	for _, test := range []struct {
		name         string
		code         sb.ProblemCode
		ceiling      sb.SandboxResourceRequest
		want, absent []string
	}{
		{"fixed resume", sb.ProblemCodeResourcesFixedByProfile, sb.SandboxResourceRequest{VCPUCount: pointer(4), MemoryBytes: pointer(1 << 30), WorkspaceBytes: pointer(50 << 30)}, []string{`Profile "resume-profile"`, "fixed size", "--cpus 4 --memory 1073741824 --disk 53687091200", "omit resource flags"}, []string{"or lower"}},
		{"unbounded compute", sb.ProblemCodeResourcesExceedProfile, sb.SandboxResourceRequest{WorkspaceBytes: pointer(50 << 30)}, []string{`Profile "resume-profile"`, "53687091200 Workspace bytes", "--disk 34359738368"}, []string{"--cpus", "--memory", "0 vCPU", "0 memory"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := &commandPresentationError{cause: &sandboxCreationError{profile: "resume-profile", cause: &sb.APIError{Problem: &sb.Problem{Code: test.code, Ceiling: &test.ceiling}}}}
			hint := failure.hint()
			for _, want := range test.want {
				if !strings.Contains(hint, want) {
					t.Errorf("hint=%q missing=%q", hint, want)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(hint, absent) {
					t.Errorf("hint=%q includes=%q", hint, absent)
				}
			}
		})
	}
}

func TestCreateRendersResourceAlignmentRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request sb.CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Resources == nil || request.Resources.MemoryBytes == nil || *request.Resources.MemoryBytes != 1<<30+1 {
			t.Errorf("CLI changed requested bytes: %+v", request.Resources)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"invalid_request","title":"SecondBox resources.memoryBytes must use whole MiB (multiples of 1048576 bytes)","details":[{"field":"resources.memoryBytes","reason":"must use whole MiB (multiples of 1048576 bytes)"}]}`)
	}))
	defer server.Close()
	err := runLifecycleVerb(t.Context(), verbTestSession(server), "create", []string{"durable-coding", "--memory", "1073741825"}, io.Discard, server.Client())
	if err == nil {
		t.Fatal("unaligned request accepted")
	}
	var diagnostic bytes.Buffer
	renderer := cliui.Renderer{Output: io.Discard, Diagnostic: &diagnostic, Capabilities: cliui.ForWriter(io.Discard, &diagnostic), OutputMode: cliui.OutputPlain}
	if err := renderer.WriteError(err, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"invalid_request", "resources.memoryBytes", "whole MiB", "1048576"} {
		if !strings.Contains(diagnostic.String(), want) {
			t.Errorf("diagnostic=%q lacks %q", diagnostic.String(), want)
		}
	}
}
