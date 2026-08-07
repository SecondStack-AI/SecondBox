package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestResourcesCheckLoadsStrictDocumentAndDoesNotMutate(t *testing.T) {
	spec := secondboxclient.ProfileRevisionSpec{Pool: "pool", Architecture: "amd64", RuntimeBundleDigest: "sha256:" + strings.Repeat("a", 64), ToolchainBundleDigest: "sha256:" + strings.Repeat("b", 64), Resources: secondboxclient.ResourcePolicy{CPUMillis: 1000, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30, ProcessLimit: 64, ConcurrentOperations: 4}, Startup: secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot}, Lifecycle: secondboxclient.LifecyclePolicy{InitialState: "running", DrainGraceSeconds: 1, IdleSeconds: 1, MaximumDurationSeconds: 1, LeaseSeconds: 1}, Retention: secondboxclient.RetentionPolicy{SnapshotRetentionSeconds: 1, ArtifactRetentionSeconds: 1}, Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: 1, MaximumBufferedOutputBytes: 1, StreamWindowBytes: 1, MaximumTransferBytes: 1, DataPlaneTransport: "proxied"}, Network: secondboxclient.NetworkPolicy{Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{}}, Ports: []secondboxclient.PortPolicy{}}
	digest, err := resourceapply.SpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	document := resourceapply.Document{SchemaVersion: resourceapply.SchemaVersion, RunnerPools: []resourceapply.RunnerPool{{Name: "pool", Architectures: []string{"amd64"}, Capabilities: []string{"local-workspace"}, CapacityPolicy: map[string]int64{"maxSandboxes": 1}, State: "ready", MutableFields: []string{}}}, Profiles: []resourceapply.Profile{{Name: "profile", Revisions: []resourceapply.ProfileRevision{{Number: 1, SpecDigest: digest, Spec: spec}}}}}
	data, err := resourceapply.Encode(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "resources.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			mutations++
		}
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"type":"about:blank","title":"not found","status":404,"code":"not_found","requestId":"request-1"}`))
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	handled, err := runOperationalCommand(t.Context(), cliSession{url: server.URL, token: "platform-token-at-least-24-bytes"}, []string{"resources", "check", "--file", path}, &output)
	if err != nil || !handled {
		t.Fatalf("handled=%t error=%v", handled, err)
	}
	if mutations != 0 || !strings.Contains(output.String(), `"action": "create"`) || strings.Contains(output.String(), `"applied": true`) {
		t.Fatalf("output=%s mutations=%d", output.String(), mutations)
	}
}
