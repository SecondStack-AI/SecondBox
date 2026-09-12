package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestSnapshotVerbs(t *testing.T) {
	for _, test := range []struct {
		command  string
		args     []string
		mutation string
	}{
		{"snapshot", []string{"mybox", "--name", "base"}, "create"},
		{"snapshots", []string{"sbx_test1"}, ""},
		{"restore", []string{"sbx_test1", "mybox/base"}, "restore"},
		{"restore", []string{"mybox", "snp_base"}, "restore"},
		{"snapshot", []string{"rm", "mybox/base"}, "delete"},
		{"snapshot", []string{"rm", "snp_base"}, "delete"},
	} {
		t.Run(test.command+strings.Join(test.args, "/"), func(t *testing.T) {
			var mutation string
			raw := " {\"id\":\"op_snapshot\",\"state\":\"pending\",\"snapshot\":{\"id\":\"snp_base\",\"state\":\"creating\"}}\n"
			snapshot := `{"id":"snp_base","sandboxId":"sbx_test1","name":"base","state":"ready"}`
			page := ` {"items":[` + snapshot + `]} `
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "DELETE":
					mutation = "delete"
					assertVerbHeaders(t, r, false, false, true)
					if r.URL.Path != "/v1/snapshots/snp_base" {
						t.Errorf("delete path=%s", r.URL.Path)
					}
					_, _ = io.WriteString(w, raw)
				case r.Method == "POST":
					assertVerbHeaders(t, r, true, false, true)
					if strings.HasSuffix(r.URL.Path, "/snapshots") {
						mutation = "create"
						var request sb.CreateSnapshotRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if request.Name != "base" {
							t.Errorf("name=%s", request.Name)
						}
					} else {
						mutation = "restore"
						var request sb.RestoreSnapshotRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if request.SnapshotID != "snp_base" {
							t.Errorf("snapshot=%s", request.SnapshotID)
						}
					}
					_, _ = io.WriteString(w, raw)
				case r.URL.Path == "/v1/operations/op_snapshot":
					_, _ = io.WriteString(w, `{"id":"op_snapshot","state":"succeeded"}`)
				case r.URL.Path == "/v1/snapshots/snp_base":
					_, _ = io.WriteString(w, snapshot)
				case r.URL.Path == "/v1/sandboxes":
					_, _ = io.WriteString(w, `{"items":[`+execSandboxJSON+`]}`)
				case strings.HasSuffix(r.URL.Path, "/snapshots"):
					_, _ = io.WriteString(w, page)
				default:
					_, _ = io.WriteString(w, execSandboxJSON)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			if err := runSnapshotVerb(t.Context(), verbTestSession(server), test.command, test.args, &output, server.Client()); err != nil {
				t.Fatal(err)
			}
			if mutation != test.mutation {
				t.Errorf("mutation=%s", mutation)
			}
			expected := raw
			if test.command == "snapshots" {
				expected = page
			}
			if output.String() != expected {
				t.Errorf("JSON=%q want=%q", output.String(), expected)
			}
		})
	}
}

func TestSnapshotReferencePaginationAndAmbiguity(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/snapshots") {
				_, _ = io.WriteString(w, execSandboxJSON)
				return
			}
			if r.URL.Query().Get("cursor") == "" {
				state := "deleted"
				if ambiguous {
					state = "ready"
				}
				_, _ = io.WriteString(w, `{"items":[{"id":"snp_old","name":"base","state":"`+state+`"}],"nextCursor":"second"}`)
			} else {
				_, _ = io.WriteString(w, `{"items":[{"id":"snp_new","name":"base","state":"ready"}]}`)
			}
		}))
		client, err := verbClient(verbTestSession(server), server.Client())
		if err != nil {
			t.Fatal(err)
		}
		id, err := resolveSnapshotReference(t.Context(), client, "sbx_test1/base")
		server.Close()
		if ambiguous {
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("err=%v", err)
			}
		} else if err != nil || id != "snp_new" {
			t.Fatalf("id=%s err=%v", id, err)
		}
	}
}
