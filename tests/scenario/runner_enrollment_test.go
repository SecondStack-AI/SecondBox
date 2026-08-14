//go:build scenario_live

package scenario_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioRunnerEnrollsThroughControlChannel(t *testing.T) {
	fixture := newScenarioFixture(t)
	pool := ensureScenarioRunnerPool(t, fixture)
	wantPoolCapabilities := []string{
		"cleanup", "compute", "local-workspace", "network-policy", "storage",
	}
	wantRunnerCapabilities := []string{
		"compute", "network-policy", "storage", "cleanup", "local-workspace",
	}
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "firecracker" {
		wantPoolCapabilities = append(wantPoolCapabilities[:4], "snapshot-resume", "storage")
		wantRunnerCapabilities = append(wantRunnerCapabilities, "snapshot-resume")
	}
	if pool.Name != scenarioRunnerPool ||
		pool.State != contracts.RunnerPoolStateReady ||
		pool.ReadyRunnerCount > 1 ||
		!slices.Equal(pool.Architectures, []string{"amd64"}) ||
		!slices.Equal(pool.Capabilities, wantPoolCapabilities) ||
		pool.CapacityPolicy["maximumInstances"] != 8 {
		t.Fatalf("SecondBox scenario created RunnerPool = %#v", pool)
	}

	runner := waitForScenarioRunner(t, fixture, 90*time.Second)
	if runner.ID != scenarioRunnerID ||
		runner.PoolName != scenarioRunnerPool ||
		runner.State != "ready" ||
		runner.CredentialState != "pre_shared" ||
		!slices.Equal(runner.Architectures, []string{"amd64"}) {
		t.Fatalf("SecondBox scenario enrolled Runner = %#v", runner)
	}
	for _, capability := range wantRunnerCapabilities {
		if !slices.Contains(runner.Capabilities, capability) {
			t.Fatalf("SecondBox scenario Runner capabilities = %v, missing %s", runner.Capabilities, capability)
		}
	}
	// Registration is rejected before persistence when any private Firecracker,
	// KVM, jailer, cgroup, network-policy, storage, or cleanup readiness failure
	// exists. A ready public projection therefore proves that list was empty
	// without leaking backend-specific readiness fields into the public schema.

	currentPool := scenarioJSON[contracts.RunnerPool](
		t,
		context.Background(),
		fixture.admin,
		"getRunnerPool",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"runnerPoolName": scenarioRunnerPool},
		},
	)
	if currentPool.ReadyRunnerCount != 1 {
		t.Fatalf("SecondBox scenario enrolled RunnerPool = %#v", currentPool)
	}
}

func TestScenarioNoReadyRunnerFailsAdmissionImmediately(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-no-capacity",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateStopped),
	)

	scenarioCompose(t, "stop", "--timeout", "30", "secondbox-runner")
	t.Cleanup(func() {
		scenarioCompose(t, "start", "secondbox-runner")
		waitForScenarioRunner(t, fixture, 90*time.Second)
	})
	waitForRunnerPoolReadyCount(t, fixture, 0, 30*time.Second)

	started := time.Now()
	var operation contracts.Operation
	err := fixture.subject.RequestJSON(
		context.Background(),
		"createSandbox",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders(uniqueScenarioKey(t, "no-capacity")),
			Body: scenarioBody(t, contracts.CreateSandboxRequest{
				Profile:  profile.Name,
				Metadata: map[string]string{"scenario": "no-capacity"},
			}),
		},
		&operation,
	)
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) ||
		apiError.StatusCode != http.StatusConflict ||
		apiError.Problem == nil ||
		apiError.Problem.Code != "execution_node_unavailable" ||
		!apiError.Problem.Retryable {
		t.Fatalf("SecondBox scenario no-capacity error = %#v, raw error=%v", apiError, err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("SecondBox scenario no-capacity admission took %s, want immediate typed failure", elapsed)
	}
}

func waitForScenarioRunner(
	t *testing.T,
	fixture scenarioFixture,
	timeout time.Duration,
) contracts.Runner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	query := make(url.Values)
	query.Set("pool", scenarioRunnerPool)
	for {
		var page contracts.RunnerPage
		err := fixture.admin.RequestJSON(ctx, "listRunners", secondboxclient.CallOptions{
			QueryParameters: query,
		}, &page)
		if err == nil {
			for _, runner := range page.Items {
				if runner.ID == scenarioRunnerID &&
					runner.State == "ready" &&
					runner.LastSeenAt != nil &&
					runner.LastSeenAt.After(runner.CreatedAt) {
					return scenarioJSON[contracts.Runner](
						t,
						ctx,
						fixture.admin,
						"getRunner",
						secondboxclient.CallOptions{
							PathParameters: map[string]string{"runnerId": scenarioRunnerID},
						},
					)
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox scenario Runner did not enroll: %v", errors.Join(err, ctx.Err()))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitForRunnerPoolReadyCount(
	t *testing.T,
	fixture scenarioFixture,
	count int64,
	timeout time.Duration,
) contracts.RunnerPool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last contracts.RunnerPool
	for {
		var err error
		err = fixture.admin.RequestJSON(ctx, "getRunnerPool", secondboxclient.CallOptions{
			PathParameters: map[string]string{"runnerPoolName": scenarioRunnerPool},
		}, &last)
		if err == nil && last.ReadyRunnerCount == count {
			return last
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario RunnerPool ready count=%d, want %d: %v",
				last.ReadyRunnerCount,
				count,
				errors.Join(err, ctx.Err()),
			)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
