package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func verbTestSession(server *httptest.Server) cliSession {
	return cliSession{url: server.URL, token: "test-token", tenantRef: "tenant-test", subjectRef: "subject-test", authority: sessionAuthorityApplication}
}

func assertVerbHeaders(t *testing.T, request *http.Request, lifecycle, generation, mutation bool) {
	t.Helper()
	if lifecycle && request.Header.Get("If-Match") != `"revision-2"` {
		t.Errorf("If-Match = %q", request.Header.Get("If-Match"))
	}
	if generation && request.Header.Get("SecondBox-Generation") != "4" {
		t.Errorf("generation = %q", request.Header.Get("SecondBox-Generation"))
	}
	if mutation && request.Header.Get("Idempotency-Key") == "" {
		t.Error("missing SDK idempotency key")
	}
	if request.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("authorization = %q", request.Header.Get("Authorization"))
	}
}

func TestLifecycleVerbsResolveAndWait(t *testing.T) {
	for _, command := range []string{"start", "stop", "rm", "delete"} {
		for _, reference := range []string{"mybox", "sbx_test1"} {
			for _, noWait := range []bool{false, true} {
				t.Run(command+"/"+reference+map[bool]string{false: "/wait", true: "/no-wait"}[noWait], func(t *testing.T) {
					target := "ready"
					if command == "stop" {
						target = "stopped"
					}
					if command == "rm" || command == "delete" {
						target = "deleted"
					}
					raw := " {\"id\":\"op_test\",\"sandboxId\":\"sbx_test1\",\"state\":\"pending\",\"future\":true}\n"
					mutations, polls, lists := 0, 0, 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.URL.Path == "/v1/sandboxes":
							lists++
							if r.URL.Query().Get("metadata") != contracts.SandboxNameMetadataKey+"=mybox" {
								t.Errorf("name query = %s", r.URL.RawQuery)
							}
							_, _ = io.WriteString(w, `{"items":[`+execSandboxJSON+`]}`)
						case r.URL.Path == "/v1/operations/op_test":
							polls++
							_, _ = io.WriteString(w, `{"id":"op_test","state":"succeeded"}`)
						case strings.HasSuffix(r.URL.Path, ":wait"):
							_, _ = io.WriteString(w, strings.ReplaceAll(execSandboxJSON, `"state":"ready"`, `"state":"`+target+`"`))
						case r.Method == "GET":
							_, _ = io.WriteString(w, execSandboxJSON)
						default:
							mutations++
							assertVerbHeaders(t, r, true, false, true)
							_, _ = io.WriteString(w, raw)
						}
					}))
					defer server.Close()
					args := []string{reference}
					if noWait {
						args = append(args, "--no-wait")
					}
					var output bytes.Buffer
					if err := runTestLifecycleVerb(t.Context(), verbTestSession(server), command, args, &output, server.Client()); err != nil {
						t.Fatal(err)
					}
					if output.String() != raw {
						t.Errorf("JSON = %q, want exact %q", output.String(), raw)
					}
					if mutations != 1 || (noWait && polls != 0) || (!noWait && polls != 1) {
						t.Errorf("mutations=%d polls=%d", mutations, polls)
					}
					if (reference == "mybox") != (lists == 1) {
						t.Errorf("lists=%d", lists)
					}
				})
			}
		}
	}
}

func TestCreateGetAndListVerbs(t *testing.T) {
	var created sb.CreateSandboxRequest
	rawList := ` {"items":[` + execSandboxJSON + `,` + strings.ReplaceAll(execSandboxJSON, `"state":"ready"`, `"state":"stopped"`) + `],"nextCursor":"next"} `
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			assertVerbHeaders(t, r, false, false, true)
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, ` {"id":"op_create","sandboxId":"sbx_test1"} `)
		} else if r.URL.Path == "/v1/sandboxes" {
			if r.URL.Query().Get("metadata") != contracts.SandboxNameMetadataKey+"=mybox" {
				t.Errorf("query = %v", r.URL.Query())
			}
			_, _ = io.WriteString(w, rawList)
		} else {
			_, _ = io.WriteString(w, execSandboxJSON)
		}
	}))
	defer server.Close()
	session := verbTestSession(server)
	var output bytes.Buffer
	if err := runTestLifecycleVerb(t.Context(), session, "create", []string{"durable-coding", "--name", "mybox", "--metadata", "team=dev", "--from", "snp_base"}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	if created.Profile != "durable-coding" || created.SourceSnapshotID != "snp_base" || created.Metadata[contracts.SandboxNameMetadataKey] != "mybox" || created.Metadata["team"] != "dev" {
		t.Fatalf("create = %#v", created)
	}
	for _, command := range []string{"ls", "list"} {
		output.Reset()
		if err := runTestLifecycleVerb(t.Context(), session, command, []string{"--name", "mybox"}, &output, server.Client()); err != nil {
			t.Fatal(err)
		}
		if output.String() != rawList {
			t.Errorf("list JSON changed: %q", output.String())
		}
	}
	output.Reset()
	if err := runTestLifecycleVerb(t.Context(), session, "get", []string{"mybox"}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	if output.String() != execSandboxJSON {
		t.Errorf("get JSON changed: %q", output.String())
	}
	output.Reset()
	ctx := withPresentation(t.Context(), presentation{renderer: cliui.Renderer{Output: &output, OutputMode: cliui.OutputPlain}})
	if err := runTestLifecycleVerb(ctx, session, "ls", []string{"--name", "mybox"}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "stopped") {
		t.Errorf("ls hides stopped Sandboxes: %s", output.String())
	}
}

func TestRemovalConfirmationOnlyOnTTY(t *testing.T) {
	for _, test := range []struct {
		name                 string
		tty, force, accepted bool
		input                string
	}{
		{name: "pipe", accepted: true}, {name: "TTY decline", tty: true, input: "n\n"}, {name: "TTY accept", tty: true, accepted: true, input: "y\n"}, {name: "force", tty: true, force: true, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = io.WriteString(w, execSandboxJSON)
					return
				}
				mutations++
				_, _ = io.WriteString(w, `{"id":"op_delete"}`)
			}))
			defer server.Close()
			var output, diagnostic bytes.Buffer
			input := strings.NewReader(test.input)
			ctx := withPresentation(context.Background(), presentation{input: input, accessible: true, renderer: cliui.Renderer{Output: &output, Diagnostic: &diagnostic, OutputMode: cliui.OutputJSON, Capabilities: cliui.Capabilities{Input: cliui.StreamCapabilities{TTY: test.tty}}}})
			args := []string{"sbx_test1", "--no-wait"}
			if test.force {
				args = append(args, "--force")
			}
			err := runTestLifecycleVerb(ctx, verbTestSession(server), "rm", args, &output, server.Client())
			if (err == nil) != test.accepted || (mutations == 1) != test.accepted {
				t.Fatalf("err=%v mutations=%d", err, mutations)
			}
			if (!test.tty || test.force) && diagnostic.Len() != 0 {
				t.Errorf("unexpected prompt %q", diagnostic.String())
			}
		})
	}
}
