package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestInstalledGuestSmokeUsesRealCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "secondbox")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../secondbox")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, output)
	}
	for _, test := range []struct {
		name     string
		context  bool
		exitCode int
	}{{"both-profiles", true, 0}, {"guest-failure", true, 7}} {
		t.Run(test.name, func(t *testing.T) {
			advertisesContext := test.context
			plan := install.InstallPlan{OperationID: "install_0123456789abcdef", CLI: install.CLIPlan{ConfigPath: filepath.Join(t.TempDir(), "config.json"), TenantRef: "local", SubjectRef: "local-operator"}, Paths: []install.PlannedPath{{Name: "secondbox-binary", Path: binary}}}
			runner := contracts.Runner{ID: "runner-0123456789abcdef", PoolName: standardresources.PoolAMD64, State: "ready", CredentialState: "pre_shared", Architectures: []string{standardresources.ArchitectureAMD64}, Capabilities: []string{"compute", "network-policy", "storage", "cleanup", "local-workspace"}, Capacity: map[string]int64{"VCPUCount": install.DurableCodingVCPUCount, "MemoryBytes": install.DurableCodingMemoryBytes, "DiskBytes": install.MinimumWorkspaceBytes, "Instances": 1, "Operations": install.DurableCodingConcurrentOperations}}
			if advertisesContext {
				runner.SupportedEgressContexts = []string{expectedInstallerComposeProject(plan)}
			}
			var mutex sync.Mutex
			var profiles []string
			executions, deletions := 0, 0
			sandboxes := map[string]contracts.Sandbox{}
			deletionPolls := map[string]int{}
			assignmentPolls := map[string]int{}
			lastActiveAssignments := int64(0)
			placementRefusals := 0
			currentID := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutex.Lock()
				defer mutex.Unlock()
				if r.Header.Get("Authorization") != "Bearer platform-test-token" || (strings.HasPrefix(r.URL.Path, "/v1/sandboxes") && (r.Header.Get("X-SecondBox-Tenant-Ref") != "local" || r.Header.Get("X-SecondBox-Subject-Ref") != "local-operator")) {
					t.Errorf("incorrect installed session headers: %v", r.Header)
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/runner-pools/" + standardresources.PoolAMD64:
					_ = json.NewEncoder(w).Encode(contracts.RunnerPool{Name: standardresources.PoolAMD64, State: contracts.RunnerPoolStateReady, ReadyRunnerCount: 1})
				case "GET /v1/runners/" + runner.ID:
					_ = json.NewEncoder(w).Encode(runner)
				case "GET /v1/diagnostics/egress-contexts":
					// The deleted Sandbox's assignment lingers for two preflight polls
					// after the deleted state; the next create must wait for it.
					active := int64(0)
					for id, sandbox := range sandboxes {
						if sandbox.State == "deleting" || (sandbox.State == "deleted" && assignmentPolls[id] < 2) {
							active = 1
							if sandbox.State == "deleted" {
								assignmentPolls[id]++
							}
						}
					}
					lastActiveAssignments = active
					context := expectedInstallerComposeProject(plan)
					_ = json.NewEncoder(w).Encode(contracts.EgressContextPreflight{
						Ready:        true,
						Requirements: []contracts.EgressContextRequirement{{TenantRef: "local", ProfileName: "durable-coding", PoolName: runner.PoolName, EgressContext: &context, CompatibleRunnerIDs: []string{runner.ID}, Status: "ready"}},
						Runners:      []contracts.EgressContextRunner{{RunnerID: runner.ID, PoolName: runner.PoolName, State: "ready", Connected: true, AdvertisedContexts: []string{context}}},
						ActiveAssignments: func() []contracts.EgressContextAssignmentGroup {
							if active == 0 {
								return nil
							}
							return []contracts.EgressContextAssignmentGroup{{EgressContext: &context, RunnerID: runner.ID, State: "active", Count: active}}
						}(),
					})
				case "POST /v1/sandboxes":
					if lastActiveAssignments != 0 {
						t.Errorf("create issued while %d assignment(s) were still active", lastActiveAssignments)
					}
					// The Runner's reported reservation lags one heartbeat: refuse the
					// second create once, and expect the smoke to retry it.
					if len(profiles) == 1 && placementRefusals == 0 {
						placementRefusals++
						w.WriteHeader(503)
						_, _ = w.Write([]byte(`{"type":"about:blank","title":"Sandbox home runner is unavailable","status":503,"code":"home_runner_unavailable","requestId":"req_1","retryable":false}`))
						return
					}
					var request secondboxclient.CreateSandboxRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if currentID != "" && deletionPolls[currentID] < 3 {
						t.Error("next create raced prior deletion")
						w.WriteHeader(429)
						return
					}
					profiles = append(profiles, request.Profile)
					currentID = "sbx_smoke_" + strconv.Itoa(len(profiles))
					sandboxes[currentID] = contracts.Sandbox{ID: currentID, Profile: request.Profile, ProfileRevisionID: "prv_1", State: "ready", DesiredState: "running", Generation: 1, Revision: 1, Metadata: map[string]string(request.Metadata)}
					_ = json.NewEncoder(w).Encode(contracts.Operation{ID: "op_create", SandboxID: currentID, Kind: "create", State: "pending"})
				case "GET /v1/sandboxes":
					var items []contracts.Sandbox
					for _, sandbox := range sandboxes {
						if r.URL.Query().Get("metadata") == contracts.SandboxNameMetadataKey+"="+sandbox.Metadata[contracts.SandboxNameMetadataKey] {
							items = append(items, sandbox)
						}
					}
					_ = json.NewEncoder(w).Encode(contracts.SandboxPage{Items: items})
				case "GET /v1/sandboxes/" + currentID:
					sandbox := sandboxes[currentID]
					if sandbox.State == "deleting" {
						deletionPolls[currentID]++
						if deletionPolls[currentID] >= 3 {
							sandbox.State = "deleted"
							sandboxes[currentID] = sandbox
							if len(profiles) == 2 {
								w.WriteHeader(404)
								_, _ = w.Write([]byte(`{"code":"not_found","title":"Resource not found"}`))
								return
							}
						}
					}
					_ = json.NewEncoder(w).Encode(sandbox)
				case "POST /v1/sandboxes/" + currentID + "/exec":
					var request secondboxclient.BufferedExecRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.Command.ArgvCommand == nil || request.Command.ArgvCommand.Executable != "/bin/echo" || !slices.Equal(request.Command.ArgvCommand.Arguments, []string{"hello"}) {
						t.Errorf("guest command=%#v", request.Command)
					}
					executions++
					_, _ = w.Write([]byte(`{"kind":"exited","exitCode":` + strconv.Itoa(test.exitCode) + `,"elapsedMilliseconds":5,"output":{"stdoutBase64":"aGVsbG8K","stderrBase64":""}}`))
				case "DELETE /v1/sandboxes/" + currentID:
					deletions++
					sandbox := sandboxes[currentID]
					sandbox.State = "deleting"
					sandboxes[currentID] = sandbox
					_ = json.NewEncoder(w).Encode(contracts.Operation{ID: "op_delete", SandboxID: currentID, Kind: "delete", State: "pending"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			content, err := json.Marshal(map[string]string{"url": server.URL, "token": "platform-test-token", "authorityKind": "platform", "tenantRef": "local", "subjectRef": "local-operator"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plan.CLI.ConfigPath, content, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SECONDBOX_TOKEN", "ambient-wrong-authority")
			t.Setenv("SECONDBOX_TENANT_REF", "ambient-wrong-tenant")
			evidence, err := runInstalledSmoke(t.Context(), plan)
			if test.exitCode != 0 {
				mutex.Lock()
				defer mutex.Unlock()
				if err == nil || evidence["agent-compartment-isolated.exitStatus"] != "7" || evidence["agent-compartment-isolated.stdout"] != "hello\n" || executions != 1 || deletions != 1 {
					t.Fatalf("failed guest smoke: evidence=%v error=%v executions=%d deletions=%d", evidence, err, executions, deletions)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mutex.Lock()
			defer mutex.Unlock()
			want := []string{"agent-compartment-isolated"}
			if advertisesContext {
				want = append(want, "durable-coding")
			}
			if !slices.Equal(profiles, want) || executions != len(want) || deletions != len(want) || placementRefusals != 1 || evidence["durable-coding.placementRetries"] != "1" {
				t.Fatalf("profiles=%v execs=%d deletes=%d refusals=%d evidence=%v", profiles, executions, deletions, placementRefusals, evidence)
			}
			for _, profile := range want {
				if evidence[profile+".stdout"] != "hello\n" || evidence[profile+".exitStatus"] != "0" || evidence[profile+".deletionWait"] == "" || evidence[profile+".deletion"] == "" || deletionPolls[evidence[profile+".sandboxId"]] != 3 {
					t.Fatalf("guest evidence=%v", evidence)
				}
			}
			for _, value := range evidence {
				if strings.Contains(value, "platform-test-token") {
					t.Fatal("evidence exposed token")
				}
			}
		})
	}
}
