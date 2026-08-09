//go:build sdk_live

package sdklive_test

import (
	"context"
	"errors"
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

	handle, operation, err := applicationClient.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{
		Profile:  profile.Name,
		Metadata: secondboxclient.Metadata{"sdk": "go", "purpose": "live-contract"},
	}, "go-create-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if operation.ID == "" || operation.SandboxID == "" {
		t.Fatalf("Go SDK live Sandbox operation = %#v", operation)
	}

	sandbox := handle.Snapshot()
	if sandbox.Metadata["sdk"] != "go" || sandbox.ProfileRevisionID != profile.CurrentRevision.ID {
		t.Fatalf("Go SDK live Sandbox metadata or pinned profile revision = %#v", sandbox)
	}

	page, err := applicationClient.ListSandboxes(t.Context(), secondboxclient.SandboxListOptions{
		Metadata: secondboxclient.Metadata{"sdk": "go", "purpose": "live-contract"},
	})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != sandbox.ID {
		t.Fatalf("Go SDK high-level Sandbox list = %#v, %v", page, err)
	}
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
	profile, err := applicationClient.CreateProfile(t.Context(), secondboxclient.CreateProfileRequest{
		Name: profileName, Spec: liveProfileRevisionSpec(),
	}, "go-create-profile")
	if err != nil {
		t.Fatal(err)
	}
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
		Startup: secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{
			InitialState: "stopped", DrainGraceSeconds: 30, IdleSeconds: 300,
			MaximumDurationSeconds: 3600, LeaseSeconds: 60,
		},
		Retention: secondboxclient.RetentionPolicy{
			SnapshotLimit: 8, SnapshotRetentionSeconds: 86400,
		},
		Execution: secondboxclient.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000, MaximumBufferedOutputBytes: 1 << 20,
			StreamWindowBytes: 65536, MaximumTransferBytes: 1 << 30,
			TerminalDetachSeconds: 30, DataPlaneTransport: "proxied",
		},
		Network: secondboxclient.NetworkPolicy{
			Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{},
		},
		Ports: []secondboxclient.PortPolicy{},
	}
}
