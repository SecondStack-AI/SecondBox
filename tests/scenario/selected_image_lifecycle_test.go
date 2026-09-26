//go:build scenario_live

package scenario_test

import (
	"context"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioSelectedImagePreservesDigestAndWorkspace(t *testing.T) {
	switch requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPUTE_BACKEND") {
	case "firecracker", "gvisor":
	default:
		t.Skip("Selected execution images require the Firecracker or gVisor backend")
	}
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(t, fixture, "scenario-selected-image", scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning))
	image := contracts.ExecutionImage{Reference: requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_EXECUTION_IMAGE")}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	prepared, err := fixture.subject.PrepareImage(ctx, sb.PrepareImageRequest{Image: image, Profile: profile.Name}, uniqueScenarioKey(t, "prepare-image"))
	if err != nil {
		t.Fatal(err)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, prepared)
	created := scenarioJSON[sb.Operation](t, ctx, fixture.subject, "createSandbox", sb.CallOptions{
		Headers: scenarioHeaders(uniqueScenarioKey(t, "selected-image-create")),
		Body:    scenarioBody(t, contracts.CreateSandboxRequest{Profile: profile.Name, Image: image, Metadata: map[string]string{}}),
	})
	handle := sb.NewSandboxHandle(fixture.subject, sb.Sandbox{ID: created.SandboxID})
	t.Cleanup(func() { cleanupScenarioSandbox(t, fixture.subject, handle) })
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	ready := waitForSandbox(t, ctx, handle, sb.SandboxStateReady)
	if ready.Image.ResolvedDigest == "" {
		t.Fatal("Selected image has no durable digest")
	}
	cli := newScenarioCLI(t, fixture.baseURL,
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_APPLICATION_TOKEN"),
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TENANT_REF"),
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SUBJECT_REF"))
	// Every signed execution image carries the builder's guest entrypoint; no
	// fixed gVisor flat root does, so this proves the image supplies the root.
	cli.success(t, ctx, "exec", ready.ID, "--", "/bin/sh", "-c", "test -x /usr/local/bin/secondbox-runner-guest-entrypoint")
	cli.success(t, ctx, "exec", ready.ID, "--", "/bin/sh", "-c", "printf selected-image-persisted > /workspace/selected-image.txt")
	scenarioCLITransition(t, ctx, cli, fixture, handle, ready.ID, "stop", sb.SandboxStateStopped)
	scenarioCLITransition(t, ctx, cli, fixture, handle, ready.ID, "start", sb.SandboxStateReady)
	if handle.Snapshot().Image.ResolvedDigest != ready.Image.ResolvedDigest {
		t.Fatal("Omitted restart changed the selected image digest")
	}
	if got := cli.success(t, ctx, "exec", ready.ID, "--", "cat", "/workspace/selected-image.txt"); got != "selected-image-persisted" {
		t.Fatalf("Selected image Workspace persistence = %q", got)
	}
}
