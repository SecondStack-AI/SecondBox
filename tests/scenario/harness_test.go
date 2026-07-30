//go:build scenario_live

package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const (
	scenarioRunnerPool = "scenario-pool"
	scenarioRunnerID   = "scenario-runner"
)

type scenarioFixture struct {
	baseURL       string
	platformToken string
	admin         *secondboxclient.Client
	subject       *secondboxclient.Client
	httpClient    *http.Client
}

func newScenarioFixture(t *testing.T) scenarioFixture {
	t.Helper()
	baseURL := requireScenarioEnvironment(t, "SECONDBOX_LIVE_BASE_URL")
	platformToken := requireScenarioEnvironment(t, "SECONDBOX_PLATFORM_TOKEN")
	httpClient := &http.Client{Timeout: 70 * time.Second}
	admin, err := secondboxclient.NewSecondBoxClient(baseURL, platformToken, httpClient)
	if err != nil {
		t.Fatalf("SecondBox scenario administrative client: %v", err)
	}
	subject, err := secondboxclient.NewSecondBoxSubjectClient(
		baseURL,
		platformToken,
		"scenario-tenant",
		"scenario-subject",
		httpClient,
	)
	if err != nil {
		t.Fatalf("SecondBox scenario application client: %v", err)
	}
	return scenarioFixture{
		baseURL:       baseURL,
		platformToken: platformToken,
		admin:         admin,
		subject:       subject,
		httpClient:    httpClient,
	}
}

func requireScenarioEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("SecondBox scenario requires %s", name)
	}
	return value
}

func scenarioJSON[T any](
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) T {
	t.Helper()
	var result T
	if err := client.RequestJSON(ctx, operationID, options, &result); err != nil {
		t.Fatalf("SecondBox scenario %s: %v", operationID, err)
	}
	return result
}

func scenarioBody(t *testing.T, value any) io.Reader {
	t.Helper()
	body, err := secondboxclient.EncodeJSONBody(value)
	if err != nil {
		t.Fatalf("SecondBox scenario encode request: %v", err)
	}
	return body
}

func scenarioHeaders(idempotencyKey string) http.Header {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	return headers
}

func waitForSandbox(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	states ...secondboxclient.SandboxState,
) secondboxclient.Sandbox {
	t.Helper()
	if len(states) == 0 {
		t.Fatal("SecondBox scenario wait requires terminal states")
	}
	target := make(map[secondboxclient.SandboxState]struct{}, len(states))
	for _, state := range states {
		target[state] = struct{}{}
	}
	last := handle.Snapshot()
	for {
		if _, found := target[last.State]; found {
			return last
		}
		remaining := time.Until(deadlineFromContext(ctx))
		if remaining <= 0 {
			t.Fatalf(
				"SecondBox scenario Sandbox %s did not reach %v: last state=%s generation=%d",
				last.ID,
				states,
				last.State,
				last.Generation,
			)
		}
		bounded := min(remaining, 55*time.Second)
		observed, err := handle.Wait(ctx, states, bounded)
		if err != nil {
			var apiError *secondboxclient.APIError
			if errors.As(err, &apiError) &&
				apiError.Problem != nil &&
				apiError.Problem.Code == "wait_expired" {
				refreshed, refreshErr := handle.Refresh(ctx)
				if refreshErr != nil {
					t.Fatalf(
						"SecondBox scenario Sandbox %s refresh after wait expiry: %v",
						last.ID,
						refreshErr,
					)
				}
				last = refreshed
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf(
					"SecondBox scenario Sandbox %s wait ended: last state=%s: %v",
					last.ID,
					last.State,
					err,
				)
			}
			t.Fatalf(
				"SecondBox scenario Sandbox %s wait failed: last state=%s: %v",
				last.ID,
				last.State,
				err,
			)
		}
		last = observed
	}
}

func deadlineFromContext(ctx context.Context) time.Time {
	if deadline, found := ctx.Deadline(); found {
		return deadline
	}
	return time.Now().Add(5 * time.Minute)
}

func decodeScenarioJSON[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer response.Body.Close()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("SecondBox scenario decode response: %v", err)
	}
	return value
}

func TestScenarioDeploymentHealth(t *testing.T) {
	fixture := newScenarioFixture(t)
	for _, endpoint := range []struct {
		path string
		want string
	}{
		{path: "/healthz", want: "healthy"},
		{path: "/readyz", want: "ready"},
	} {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			fixture.baseURL+endpoint.path,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, err := fixture.httpClient.Do(request)
		if err != nil {
			t.Fatalf("SecondBox scenario %s: %v", endpoint.path, err)
		}
		body := decodeScenarioJSON[struct {
			Status string `json:"status"`
		}](t, response)
		if response.StatusCode != http.StatusOK || body.Status != endpoint.want {
			t.Fatalf(
				"SecondBox scenario %s status=%d body=%s, want status=200 body=%s",
				endpoint.path,
				response.StatusCode,
				body.Status,
				endpoint.want,
			)
		}
	}
}

func uniqueScenarioKey(t *testing.T, suffix string) string {
	t.Helper()
	name := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	return fmt.Sprintf("%s-%s-%d", name, suffix, time.Now().UnixNano())
}

func ensureScenarioRunnerPool(t *testing.T, fixture scenarioFixture) contracts.RunnerPool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := contracts.CreateRunnerPoolRequest{
		Name:          scenarioRunnerPool,
		State:         contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"},
		Capabilities: []string{
			"compute",
			"network-policy",
			"storage",
			"cleanup",
			"local-workspace",
		},
		CapacityPolicy: map[string]int64{
			"maximumInstances": 8,
		},
	}
	var created contracts.RunnerPool
	err := fixture.admin.RequestJSON(ctx, "createRunnerPool", secondboxclient.CallOptions{
		Body: scenarioBody(t, request),
	}, &created)
	if err == nil {
		return created
	}
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) ||
		apiError.StatusCode != http.StatusConflict ||
		apiError.Problem == nil ||
		apiError.Problem.Code != "state_conflict" {
		t.Fatalf("SecondBox scenario createRunnerPool: %v", err)
	}
	return scenarioJSON[contracts.RunnerPool](
		t,
		ctx,
		fixture.admin,
		"getRunnerPool",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"runnerPoolName": scenarioRunnerPool},
		},
	)
}

func scenarioProfileSpec(t *testing.T, initialState string) contracts.ProfileRevisionSpec {
	t.Helper()
	return contracts.ProfileRevisionSpec{
		Pool:                  scenarioRunnerPool,
		Architecture:          "amd64",
		RuntimeBundleDigest:   requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST"),
		ToolchainBundleDigest: requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST"),
		Resources: contracts.ResourcePolicy{
			CPUMillis:            1000,
			MemoryBytes:          256 << 20,
			WorkspaceBytes:       64 << 20,
			ProcessLimit:         128,
			ConcurrentOperations: 2,
		},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState:           initialState,
			DrainGraceSeconds:      5,
			IdleSeconds:            300,
			MaximumDurationSeconds: 1800,
			LeaseSeconds:           60,
		},
		Retention: contracts.RetentionPolicy{
			SnapshotLimit:            4,
			SnapshotRetentionSeconds: 3600,
			ArtifactRetentionSeconds: 3600,
		},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000,
			MaximumBufferedOutputBytes:  1 << 20,
			StreamWindowBytes:           65536,
			MaximumTransferBytes:        1 << 20,
			TerminalDetachSeconds:       30,
		},
		Network: contracts.NetworkPolicy{
			Mode:         "deny_all",
			Destinations: []contracts.NetworkDestination{},
		},
		Ports: []contracts.PortPolicy{},
	}
}

func createScenarioProfile(
	t *testing.T,
	fixture scenarioFixture,
	name string,
	spec contracts.ProfileRevisionSpec,
) contracts.Profile {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return scenarioJSON[contracts.Profile](
		t,
		ctx,
		fixture.admin,
		"createProfile",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders("profile-" + name),
			Body: scenarioBody(t, contracts.CreateProfileRequest{
				Name: name,
				Spec: spec,
			}),
		},
	)
}

func createScenarioSandbox(
	t *testing.T,
	fixture scenarioFixture,
	profile contracts.Profile,
	suffix string,
) (*secondboxclient.SandboxHandle, contracts.Operation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	operation := scenarioJSON[contracts.Operation](
		t,
		ctx,
		fixture.subject,
		"createSandbox",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders(uniqueScenarioKey(t, "create-"+suffix)),
			Body: scenarioBody(t, contracts.CreateSandboxRequest{
				Profile: profile.Name,
				Metadata: map[string]string{
					"scenario": suffix,
				},
			}),
		},
	)
	if operation.ID == "" || operation.SandboxID == "" {
		t.Fatalf("SecondBox scenario create operation = %#v", operation)
	}
	sandbox := scenarioJSON[contracts.Sandbox](
		t,
		ctx,
		fixture.subject,
		"getSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": operation.SandboxID},
		},
	)
	handle := secondboxclient.NewSandboxHandle(fixture.subject, sandbox)
	t.Cleanup(func() {
		cleanupScenarioSandbox(t, fixture.subject, handle)
	})
	return handle, operation
}

func cleanupScenarioSandbox(
	t *testing.T,
	client *secondboxclient.Client,
	handle *secondboxclient.SandboxHandle,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		var apiError *secondboxclient.APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return
		}
		t.Errorf("SecondBox scenario Sandbox cleanup refresh: %v", err)
		return
	}
	if sandbox.State == contracts.SandboxStateDeleted {
		return
	}
	var operation contracts.Operation
	for attempt := 0; attempt < 5; attempt++ {
		operation, err = handle.Delete(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, "cleanup-delete"),
			IfMatch:        sandboxRevisionETag(sandbox.Revision),
		})
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Errorf("SecondBox scenario Sandbox cleanup delete: %v", err)
			return
		}
		refreshed, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Errorf(
				"SecondBox scenario Sandbox cleanup refresh after revision conflict: %v",
				refreshErr,
			)
			return
		}
		sandbox = refreshed
	}
	if err != nil {
		t.Errorf("SecondBox scenario Sandbox cleanup delete remained revision-conflicted: %v", err)
		return
	}
	if _, err := client.WaitOperation(ctx, operation.ID, 100*time.Millisecond); err != nil {
		t.Errorf("SecondBox scenario Sandbox cleanup operation: %v", err)
		return
	}
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
}

func sandboxRevisionETag(revision int64) string {
	return `"revision-` + strconv.FormatInt(revision, 10) + `"`
}

func waitForScenarioOperation(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	operation contracts.Operation,
) contracts.Operation {
	t.Helper()
	if strings.TrimSpace(operation.ID) == "" {
		t.Fatalf("SecondBox scenario asynchronous Operation has no ID: %#v", operation)
	}
	terminal, err := client.WaitOperation(ctx, operation.ID, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("SecondBox scenario Operation %s: %v", operation.ID, err)
	}
	if terminal.ID != operation.ID ||
		terminal.State != contracts.OperationStateSucceeded ||
		terminal.CompletedAt == nil {
		t.Fatalf("SecondBox scenario terminal Operation = %#v", terminal)
	}
	return terminal
}

func stopScenarioSandbox(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	key string,
) secondboxclient.Sandbox {
	t.Helper()
	var (
		operation contracts.Operation
		err       error
	)
	for attempt := 0; attempt < 5; attempt++ {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh before stop: %v", refreshErr)
		}
		operation, err = handle.Stop(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, key),
			IfMatch:        sandboxRevisionETag(current.Revision),
		})
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("SecondBox scenario stop Sandbox: %v", err)
		}
	}
	if err != nil {
		t.Fatalf("SecondBox scenario stop Sandbox remained revision-conflicted: %v", err)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	return waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
}

func startScenarioSandbox(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	key string,
) secondboxclient.Sandbox {
	t.Helper()
	var (
		operation contracts.Operation
		err       error
	)
	for attempt := 0; attempt < 5; attempt++ {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh before start: %v", refreshErr)
		}
		operation, err = handle.Start(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, key),
			IfMatch:        sandboxRevisionETag(current.Revision),
		})
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("SecondBox scenario start Sandbox: %v", err)
		}
	}
	if err != nil {
		t.Fatalf("SecondBox scenario start Sandbox remained revision-conflicted: %v", err)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	return waitForSandbox(
		t,
		ctx,
		handle,
		secondboxclient.SandboxStateReady,
		secondboxclient.SandboxStateFailed,
	)
}

func scenarioCompose(t *testing.T, arguments ...string) {
	t.Helper()
	commandArguments := []string{
		"compose",
		"--project-name", requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPOSE_PROJECT"),
		"--file", requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPOSE_FILE"),
	}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(context.Background(), "docker", commandArguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("SecondBox scenario Compose %v: %v\n%s", arguments, err, output)
	}
}
