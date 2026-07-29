//go:build sdk_live

package sdklive_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const composeRunnerPoolName = "compose-live-pool"

func TestGoSDKLiveControlPlaneContract(t *testing.T) {
	fixture := newGoLiveSubjectFixture(t)
	applicationClient := fixture.applicationClient
	profile := fixture.profile

	operation := requestLiveJSON[secondboxclient.Operation](
		t,
		applicationClient,
		"createSandbox",
		secondboxclient.CallOptions{
			Headers: liveIdempotencyHeaders("go-create-sandbox"),
			Body: encodeLiveJSON(t, secondboxclient.CreateSandboxRequest{
				Profile: profile.Name,
				Metadata: secondboxclient.Metadata{
					"sdk":     "go",
					"purpose": "live-contract",
				},
			}),
		},
	)
	if operation.ID == "" || operation.SandboxID == "" {
		t.Fatalf("Go SDK live Sandbox operation = %#v", operation)
	}

	sandbox := requestLiveJSON[secondboxclient.Sandbox](
		t,
		applicationClient,
		"getSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": string(operation.SandboxID)},
		},
	)
	if sandbox.Metadata["sdk"] != "go" || sandbox.ProfileRevisionID != profile.CurrentRevision.ID {
		t.Fatalf("Go SDK live Sandbox metadata or pinned profile revision = %#v", sandbox)
	}

	handle := secondboxclient.NewSandboxHandle(applicationClient, sandbox)
	refreshed, err := handle.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != sandbox.ID || handle.Snapshot().Metadata["purpose"] != "live-contract" {
		t.Fatalf("Go SDK SandboxHandle refresh = %#v", refreshed)
	}

	var missing secondboxclient.Sandbox
	err = applicationClient.RequestJSON(context.Background(), "getSandbox", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": "sbx_missing_live_contract"},
	}, &missing)
	var apiFailure *secondboxclient.APIError
	if !errors.As(err, &apiFailure) {
		t.Fatalf("Go SDK missing Sandbox error = %T %v, want *APIError", err, err)
	}
	if apiFailure.StatusCode != http.StatusNotFound || apiFailure.Problem == nil ||
		apiFailure.Problem.Code != "not_found" || apiFailure.Problem.RequestID == "" {
		t.Fatalf("Go SDK structured API error = %#v", apiFailure)
	}
}

type goLiveSubjectFixture struct {
	applicationClient *secondboxclient.Client
	profile           secondboxclient.Profile
}

func newGoLiveSubjectFixture(t *testing.T) goLiveSubjectFixture {
	t.Helper()
	baseURL := requireLiveTestEnvironment(t, "SECONDBOX_LIVE_BASE_URL")
	platformToken := requireLiveTestEnvironment(t, "SECONDBOX_LIVE_PLATFORM_TOKEN")
	httpClient := &http.Client{Timeout: 10 * time.Second}

	applicationClient, err := secondboxclient.NewSecondBoxSubjectClient(
		baseURL, platformToken, "sdk-live-go", "sdk-live-go-subject", httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	profileName := secondboxclient.ProfileName("go-sdk-live")
	profile := requestLiveJSON[secondboxclient.Profile](
		t,
		applicationClient,
		"createProfile",
		secondboxclient.CallOptions{
			Headers: liveIdempotencyHeaders("go-create-profile"),
			Body: encodeLiveJSON(t, secondboxclient.CreateProfileRequest{
				Name: profileName,
				Spec: liveProfileRevisionSpec(),
			}),
		},
	)
	if profile.CurrentRevision.ID == "" || profile.Name != profileName {
		t.Fatalf("Go SDK live Profile = %#v", profile)
	}

	return goLiveSubjectFixture{
		applicationClient: applicationClient,
		profile:           profile,
	}
}

func requireLiveTestEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("SecondBox live SDK test requires %s", name)
	}
	return value
}

func requestLiveJSON[T any](
	t *testing.T,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) T {
	t.Helper()
	var result T
	if err := client.RequestJSON(context.Background(), operationID, options, &result); err != nil {
		t.Fatalf("SecondBox Go SDK live %s failed: %v", operationID, err)
	}
	return result
}

func encodeLiveJSON(t *testing.T, value any) io.Reader {
	t.Helper()
	body, err := secondboxclient.EncodeJSONBody(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func liveIdempotencyHeaders(value string) http.Header {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", value)
	return headers
}

func liveProfileRevisionSpec() secondboxclient.ProfileRevisionSpec {
	return secondboxclient.ProfileRevisionSpec{
		Pool:                  composeRunnerPoolName,
		Architecture:          "amd64",
		RuntimeBundleDigest:   "sha256:" + strings.Repeat("a", 64),
		ToolchainBundleDigest: "sha256:" + strings.Repeat("b", 64),
		Resources: secondboxclient.ResourcePolicy{
			CPUMillis: 1000, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30,
			ProcessLimit: 128, ConcurrentOperations: 4,
		},
		Lifecycle: secondboxclient.LifecyclePolicy{
			InitialState: "stopped", DrainGraceSeconds: 30, IdleSeconds: 300,
			MaximumDurationSeconds: 3600, LeaseSeconds: 60,
		},
		Retention: secondboxclient.RetentionPolicy{
			SnapshotLimit: 8, SnapshotRetentionSeconds: 86400,
			ArtifactRetentionSeconds: 86400,
		},
		Execution: secondboxclient.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000, MaximumBufferedOutputBytes: 1 << 20,
			StreamWindowBytes: 65536, MaximumTransferBytes: 1 << 30,
			TerminalDetachSeconds: 30,
		},
		Network: secondboxclient.NetworkPolicy{
			Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{},
		},
		Ports: []secondboxclient.PortPolicy{},
	}
}
