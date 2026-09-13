package integration_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestSandboxRequestedResourcesHTTPAndQuota(t *testing.T) {
	for _, initialState := range []string{"running", "stopped"} {
		t.Run(initialState, func(t *testing.T) {
			quota := generousQuota()
			quota.MaxVCPUCount = 6
			quota.MaxMemoryBytes = 1536 << 20
			controlPlane, databaseStore := newControlPlaneFixture(t, quota)
			admin := fixtureAdmin(t, controlPlane)
			tenant, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "resources-"+initialState)
			profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-resources-"+initialState)
			spec := profile.CurrentRevision.Spec
			spec.Resources.VCPUCount = 4
			spec.Lifecycle.InitialState = initialState
			profile, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			principal := authenticateCredential(t, controlPlane, credential)
			handler, err := api.NewHandler(api.HandlerConfig{Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaximumDataPlaneBodyBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			server := contractServer(t, handler)
			t.Cleanup(server.Close)
			pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			before, err := databaseStore.GetDeploymentUsage(t.Context(), 100, "", now)
			if err != nil {
				t.Fatal(err)
			}
			ceiling := contracts.SandboxResources{VCPUCount: 4, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30}
			for index, resources := range []map[string]int64{
				{"vcpuCount": 2, "memoryBytes": 512 << 20, "workspaceBytes": 2 << 30},
				{"vcpuCount": 4, "memoryBytes": 1 << 30, "workspaceBytes": 8 << 30},
			} {
				response := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "resource-create-"+strconv.Itoa(index), map[string]any{"profile": profile.Name, "metadata": map[string]string{}, "resources": resources})
				if response.StatusCode != http.StatusAccepted {
					t.Fatalf("create status=%d body=%s", response.StatusCode, readResponse(t, response))
				}
				var operation contracts.Operation
				decodeResponseJSON(t, response, &operation)
				get := authenticatedJSONRequest(t, http.MethodGet, server.URL+"/v1/sandboxes/"+operation.SandboxID, credential, "", nil)
				var sandbox contracts.Sandbox
				decodeResponseJSON(t, get, &sandbox)
				want := contracts.SandboxResources{VCPUCount: resources["vcpuCount"], MemoryBytes: resources["memoryBytes"], WorkspaceBytes: resources["workspaceBytes"]}
				if sandbox.Resources != want || sandbox.Workspace.SizeBytes != want.WorkspaceBytes {
					t.Fatalf("resolved sandbox = %+v", sandbox)
				}
				var payload []byte
				if err := pool.QueryRow(t.Context(), `SELECT command.payload FROM secondbox.runner_commands AS command JOIN secondbox.lifecycle_effects AS effect ON effect.command_id=command.id WHERE effect.sandbox_id=$1 AND effect.kind='local_workspace_create'`, sandbox.ID).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var message runnerv1.ControlPlaneToRunner
				if err := proto.Unmarshal(payload, &message); err != nil {
					t.Fatal(err)
				}
				if message.GetLocalWorkspace().GetLogicalCapacityBytes() != uint64(want.WorkspaceBytes) {
					t.Fatalf("workspace command = %+v", message.GetLocalWorkspace())
				}
				if initialState == "stopped" {
					usage, err := databaseStore.GetSubjectUsage(t.Context(), tenant.ID, account.ID)
					if err != nil {
						t.Fatal(err)
					}
					expectedCPU := int64(index * 2)
					if usage.Usage.VCPUCount != expectedCPU {
						t.Fatalf("stopped create reserved CPU: %+v", usage)
					}
					completeFixtureSandboxCreation(t, sandbox.ID)
					sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
					if err != nil {
						t.Fatal(err)
					}
					response = lifecycleHTTPRequest(t, server.URL, credential, http.MethodPost, "/v1/sandboxes/"+sandbox.ID+":start", "resource-start-"+strconv.Itoa(index), strconv.FormatInt(sandbox.Revision, 10), "", nil)
					if response.StatusCode != http.StatusAccepted {
						t.Fatalf("start status=%d body=%s", response.StatusCode, readResponse(t, response))
					}
					response.Body.Close()
				}
			}
			response := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "resource-over", map[string]any{"profile": profile.Name, "metadata": map[string]string{}, "resources": map[string]int64{"vcpuCount": 5}})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("above ceiling status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var problem contracts.Problem
			decodeResponseJSON(t, response, &problem)
			requested := ceiling
			requested.VCPUCount = 5
			if problem.Code != "resources_exceed_profile" || problem.Ceiling == nil || !reflect.DeepEqual(*problem.Ceiling, contracts.SandboxResourceRequest{VCPUCount: &ceiling.VCPUCount, MemoryBytes: &ceiling.MemoryBytes, WorkspaceBytes: &ceiling.WorkspaceBytes}) || problem.Requested == nil || *problem.Requested != requested {
				t.Fatalf("ceiling problem = %+v", problem)
			}
			usage, err := databaseStore.GetSubjectUsage(t.Context(), tenant.ID, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if usage.Usage.Sandboxes != 2 || usage.Usage.VCPUCount != 6 || usage.Usage.MemoryBytes != 1536<<20 {
				t.Fatalf("subject usage = %+v", usage)
			}
			tenantUsage, err := databaseStore.GetTenantUsage(t.Context(), tenant.ID, 100, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if tenantUsage.Usage.VCPUCount != 6 || tenantUsage.Usage.MemoryBytes != 1536<<20 {
				t.Fatalf("tenant usage = %+v", tenantUsage)
			}
			after, err := databaseStore.GetDeploymentUsage(t.Context(), 100, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if after.Usage.VCPUCount-before.Usage.VCPUCount != 6 || after.Usage.MemoryBytes-before.Usage.MemoryBytes != 1536<<20 {
				t.Fatalf("deployment usage before=%+v after=%+v", before.Usage, after.Usage)
			}
			var operations, effects, workspaces int
			if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM secondbox.operations WHERE tenant_ref=$1 AND kind='create'),(SELECT count(*) FROM secondbox.lifecycle_effects e JOIN secondbox.sandboxes s ON s.id=e.sandbox_id WHERE s.tenant_ref=$1),(SELECT count(*) FROM secondbox.workspaces WHERE tenant_ref=$1)`, tenant.ID).Scan(&operations, &effects, &workspaces); err != nil {
				t.Fatal(err)
			}
			if operations != 2 || effects != 2 || workspaces != 2 {
				t.Fatalf("refused create left intent: operations=%d effects=%d workspaces=%d", operations, effects, workspaces)
			}
		})
	}
}

func TestSandboxPartialResourcesPinRevisionAndIdempotency(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "resources-partial")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-resources-partial")
	principal := authenticateCredential(t, controlPlane, credential)
	memory := int64(512 << 20)
	request := contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}, Resources: &contracts.SandboxResourceRequest{MemoryBytes: &memory}}
	first, _, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-partial", request)
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.SandboxResources{VCPUCount: 1, MemoryBytes: memory, WorkspaceBytes: 8 << 30}
	if first.Resources != want {
		t.Fatalf("partial resources = %+v", first.Resources)
	}
	spec := profile.CurrentRevision.Spec
	spec.Resources = contracts.ResourcePolicy{VCPUCount: 4, MemoryBytes: 4 << 30, WorkspaceBytes: 50 << 30, ConcurrentOperations: 4}
	if _, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	replay, created, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-partial", request)
	if err != nil {
		t.Fatal(err)
	}
	if created || replay.ID != first.ID || replay.Resources != want || replay.ProfileRevisionID != first.ProfileRevisionID {
		t.Fatalf("replayed Sandbox = %+v", replay)
	}
	persisted, err := controlPlane.GetSandbox(t.Context(), principal, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Resources != want {
		t.Fatalf("persisted resources = %+v", persisted.Resources)
	}
	request.Resources = nil
	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-partial", request); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("changed resource request error = %v", err)
	}
	defaults, _, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-new-defaults", request)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Resources != (contracts.SandboxResources{VCPUCount: 4, MemoryBytes: 4 << 30, WorkspaceBytes: 50 << 30}) {
		t.Fatalf("default resources = %+v", defaults.Resources)
	}
}

func TestSandboxRequestedResourcesFitSmallerHomeRunner(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "resources-placement")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-resources-placement")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	const poolName = "resources-placement-pool"
	const runnerID = "resources-placement-runner"
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: poolName, State: contracts.RunnerPoolStateReady, Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"}, ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	seedFixtureHomeRunner(t, poolName, runnerID)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.runners SET capacity_json='{"VCPUCount":2,"MemoryBytes":1073741824,"DiskBytes":4294967296,"Instances":10,"Operations":10}' WHERE id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}
	spec := profile.CurrentRevision.Spec
	spec.Pool = poolName
	spec.Resources = contracts.ResourcePolicy{VCPUCount: 4, MemoryBytes: 4 << 30, WorkspaceBytes: 8 << 30, ConcurrentOperations: 4}
	if _, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	cpu, memory, disk := int64(2), int64(1<<30), int64(2<<30)
	request := contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}, Resources: &contracts.SandboxResourceRequest{VCPUCount: &cpu, MemoryBytes: &memory, WorkspaceBytes: &disk}}
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-placement", request)
	if err != nil {
		t.Fatal(err)
	}
	var home string
	if err := pool.QueryRow(t.Context(), `SELECT home_runner_id FROM secondbox.workspaces WHERE sandbox_id=$1`, sandbox.ID).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != runnerID {
		t.Fatalf("home = %q", home)
	}
	request.Resources = nil
	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "resources-placement-default", request); !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		t.Fatalf("Profile-sized placement = %v", err)
	}
}

func TestSandboxFlexibleResourcesHTTPQuotaAndResume(t *testing.T) {
	quota := generousQuota()
	quota.MaxVCPUCount = 3
	quota.MaxMemoryBytes = 1536 << 20
	controlPlane, databaseStore := newControlPlaneFixture(t, quota)
	admin := fixtureAdmin(t, controlPlane)
	tenant, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "resources-flexible")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-resources-flexible")
	spec := profile.CurrentRevision.Spec
	spec.Resources.MemoryBytes = 64 << 20
	spec.Resources.WorkspaceBytes = 1 << 30
	spec.Lifecycle.InitialState = contracts.SandboxDesiredStateRunning
	diskCeiling := int64(8 << 30)
	spec.ResourceCeiling = contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": &diskCeiling}
	if _, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaximumDataPlaneBodyBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)
	response := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "flexible-create", map[string]any{"profile": profile.Name, "metadata": map[string]string{}, "resources": map[string]int64{"vcpuCount": 2, "memoryBytes": 1 << 30, "workspaceBytes": 3 << 30}})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	get := authenticatedJSONRequest(t, http.MethodGet, server.URL+"/v1/sandboxes/"+operation.SandboxID, credential, "", nil)
	var sandbox contracts.Sandbox
	decodeResponseJSON(t, get, &sandbox)
	if sandbox.Resources != (contracts.SandboxResources{VCPUCount: 2, MemoryBytes: 1 << 30, WorkspaceBytes: 4 << 30}) || sandbox.Workspace.SizeBytes != 4<<30 {
		t.Fatalf("resolved resources=%+v workspace=%+v", sandbox.Resources, sandbox.Workspace)
	}
	for _, test := range []struct {
		name      string
		resources map[string]int64
		status    int
		code      string
	}{
		{"unaligned memory", map[string]int64{"memoryBytes": 1<<30 + 1}, http.StatusBadRequest, "invalid_request"},
		{"unaligned disk", map[string]int64{"workspaceBytes": 1<<30 + 1}, http.StatusBadRequest, "invalid_request"},
		{"unaligned disk before clamp", map[string]int64{"workspaceBytes": 7<<30 + 1}, http.StatusBadRequest, "invalid_request"},
		{"cpu quota", map[string]int64{"vcpuCount": 2}, http.StatusTooManyRequests, "quota_exceeded"},
		{"memory quota", map[string]int64{"memoryBytes": 1 << 30}, http.StatusTooManyRequests, "quota_exceeded"},
		{"rounded disk ceiling", map[string]int64{"workspaceBytes": 8<<30 + 1<<20}, http.StatusBadRequest, "resources_exceed_profile"},
	} {
		response := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "flexible-"+strings.ReplaceAll(test.name, " ", "-"), map[string]any{"profile": profile.Name, "metadata": map[string]string{}, "resources": test.resources})
		if response.StatusCode != test.status {
			t.Fatalf("%s status=%d body=%s", test.name, response.StatusCode, readResponse(t, response))
		}
		var problem contracts.Problem
		decodeResponseJSON(t, response, &problem)
		if problem.Code != test.code {
			t.Fatalf("%s problem=%+v", test.name, problem)
		}
		if test.code == "invalid_request" && (len(problem.Details) != 1 || !strings.Contains(problem.Details[0].Reason, "whole MiB") || !strings.HasPrefix(problem.Details[0].Field, "resources.") || !strings.Contains(problem.Title, "whole MiB")) {
			t.Fatalf("alignment detail=%+v", problem)
		}
		if test.code == "resources_exceed_profile" && (problem.Ceiling == nil || problem.Ceiling.VCPUCount != nil || problem.Ceiling.MemoryBytes != nil || *problem.Ceiling.WorkspaceBytes != diskCeiling || problem.Requested.WorkspaceBytes != 16<<30) {
			t.Fatalf("effective ceiling=%+v", problem)
		}
	}
	usage, err := databaseStore.GetSubjectUsage(t.Context(), tenant.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage.Sandboxes != 1 || usage.Usage.VCPUCount != 2 || usage.Usage.MemoryBytes != 1<<30 {
		t.Fatalf("refusal changed quota=%+v", usage)
	}
	spec.ResourceCeiling = nil
	spec.Startup.Mode = contracts.StartupModeSnapshotResume
	if _, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	response = authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "resume-refused", map[string]any{"profile": profile.Name, "metadata": map[string]string{}, "resources": map[string]int64{"memoryBytes": 128 << 20}})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var problem contracts.Problem
	decodeResponseJSON(t, response, &problem)
	if problem.Code != "resources_fixed_by_profile" || problem.Ceiling == nil || *problem.Ceiling.MemoryBytes != spec.Resources.MemoryBytes || *problem.Ceiling.WorkspaceBytes != spec.Resources.WorkspaceBytes || *problem.Ceiling.VCPUCount != spec.Resources.VCPUCount {
		t.Fatalf("resume problem=%+v", problem)
	}
}
