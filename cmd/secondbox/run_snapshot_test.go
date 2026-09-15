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

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestRunAndCreateFromSnapshot(t *testing.T) {
	for _, command := range []string{"run", "create", "interactive"} {
		for _, reference := range []string{"golden/with-deps", "sbx_run1/with-deps", "snp_base"} {
			t.Run(command+"/"+reference, func(t *testing.T) {
				var server *httptest.Server
				var invoke func([]string) error
				if command == "interactive" {
					recorder := newTTYRunServer(t)
					server = recorder.server
					invoke = func(args []string) error { _, err := invokeTTYRun(t, recorder, append(args, "--tty")); return err }
				} else {
					recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
					server = recorder.server
					invoke = func(args []string) error {
						if command == "create" {
							return runTestLifecycleVerb(t.Context(), execTestSession(server.URL), command, args, io.Discard, server.Client())
						}
						_, _, err := invokeRun(t, recorder, append(args, "--", "true"))
						return err
					}
				}
				original := server.Config.Handler
				var created sb.CreateSandboxRequest
				lookups := 0
				server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == "GET" && r.URL.Path == "/v1/sandboxes":
						lookups++
						_, _ = io.WriteString(w, `{"items":[`+runSandboxJSON("stopped")+`]}`)
					case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/snapshots"):
						lookups++
						_, _ = io.WriteString(w, `{"items":[{"id":"snp_base","name":"with-deps","state":"ready"}]}`)
					default:
						if r.Method == "POST" && r.URL.Path == "/v1/sandboxes" {
							raw, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							if err := json.Unmarshal(raw, &created); err != nil {
								t.Error(err)
							}
							r.Body = io.NopCloser(bytes.NewReader(raw))
							if r.Header.Get("Idempotency-Key") == "" {
								t.Error("missing SDK request key")
							}
						}
						original.ServeHTTP(w, r)
					}
				})
				if err := invoke([]string{"durable-coding", "--from", reference, "--size", "small"}); err != nil {
					t.Fatal(err)
				}
				if created.SourceSnapshotID != "snp_base" || created.Resources == nil || *created.Resources.WorkspaceBytes != 4<<30 {
					t.Fatalf("create = %+v", created)
				}
				if (reference == "snp_base") != (lookups == 0) {
					t.Fatalf("lookups = %d", lookups)
				}
			})
		}
	}
}

func TestRunAndCreatePreserveSnapshotProblem(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"state_conflict","title":"Snapshot requires stopped committed disk state"}`)
			}))
			defer server.Close()
			args := []string{"durable-coding", "--from", "snp_base", "--disk", "4GiB"}
			var err error
			if command == "create" {
				err = runTestLifecycleVerb(t.Context(), verbTestSession(server), command, args, io.Discard, server.Client())
			} else {
				err = runTestRunCommand(t.Context(), verbTestSession(server), append(args, "--", "true"), execCommandEnvironment{stdout: io.Discard, stderr: io.Discard, httpClient: server.Client()}, sandboxShellEnvironment{})
			}
			var api *sb.APIError
			if !errors.As(err, &api) || api.Problem == nil || api.Problem.Code != sb.ProblemCodeStateConflict {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
