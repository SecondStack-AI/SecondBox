//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioCLITargetShape(t *testing.T) {
	fixture := newScenarioFixture(t)
	if requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPUTE_BACKEND") != "firecracker" {
		t.Skip("SecondBox CLI target shape requires the qualified Firecracker stack")
	}
	cli := newScenarioCLI(t, fixture.baseURL,
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_APPLICATION_TOKEN"),
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TENANT_REF"),
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SUBJECT_REF"))
	// Bound the workflow as a whole; the build and failure cleanup have separate
	// bounds. CLI lifecycle verbs and waitForSandbox poll rather than sleeping.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 15*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	cpuCeiling, diskCeiling := spec.Resources.VCPUCount, spec.Resources.WorkspaceBytes
	spec.ResourceCeiling = contracts.ProfileResourceCeiling{"vcpuCount": &cpuCeiling, "memoryBytes": nil, "workspaceBytes": &diskCeiling}
	profile := createScenarioProfile(t, fixture, "scenario-cli-target-shape", spec)
	name := uniqueScenarioKey(t, "original")
	cloneName := uniqueScenarioKey(t, "clone")
	// Register by name before creating anything: run --keep can fail after
	// admission, before stdout provides an ID. The clone is cleaned up first.
	for _, reference := range []string{name, cloneName} {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanupCancel()
			page, err := fixture.subject.ListSandboxes(cleanupCtx, sb.SandboxListOptions{
				Metadata: sb.Metadata{contracts.SandboxNameMetadataKey: reference},
			})
			if err != nil {
				t.Errorf("SecondBox scenario CLI cleanup lookup: %v", err)
				return
			}
			for _, sandbox := range page.Items {
				cleanupScenarioSandbox(t, fixture.subject, sb.NewSandboxHandle(fixture.subject, sandbox))
			}
		})
	}
	if got := cli.success(t, ctx, "run", profile.Name, "--image", "registry.example/secondbox/test-agent:stable", "--keep", "--name", name, "--cpus", "1", "--memory", "1GiB", "--disk", "33MiB", "--", "/bin/sh", "-c", "echo hello"); got != "hello\n" {
		t.Fatalf("SecondBox scenario CLI run stdout=%q", got)
	}
	original := scenarioCLIJSON[sb.Sandbox](t, cli.success(t, ctx, "--output", "json", "get", name))
	if original.ID == "" || original.Metadata[contracts.SandboxNameMetadataKey] != name || original.Resources.VCPUCount != 1 || original.Resources.MemoryBytes != 1<<30 || original.Resources.WorkspaceBytes != 64<<20 {
		t.Fatalf("SecondBox scenario CLI get unexpected Sandbox: %+v", original)
	}
	originalHandle := sb.NewSandboxHandle(fixture.subject, original)
	refused := cli.run(t, ctx, "run", profile.Name, "--image", "registry.example/secondbox/test-agent:stable", "--cpus", "999", "--", "true")
	if refused.exitCode == 0 || refused.stdout != "" || !strings.Contains(refused.stderr, "resources_exceed_profile") || !strings.Contains(strings.ToLower(refused.stderr), "ceiling") {
		t.Fatalf("SecondBox scenario CLI resource refusal: %+v", refused)
	}
	local := t.TempDir()
	input, output := filepath.Join(local, "in.txt"), filepath.Join(local, "out.txt")
	payload := []byte("SecondBox CLI workspace round trip\nwith a second line\n")
	if err := os.WriteFile(input, payload, 0600); err != nil {
		t.Fatal(err)
	}
	written := scenarioCLIJSON[contracts.FileWriteResult](t, cli.success(t, ctx, "cp", input, name+":/workspace/in.txt"))
	if written.Path != "in.txt" || written.SizeBytes != int64(len(payload)) {
		t.Fatalf("SecondBox scenario CLI cp upload: %+v", written)
	}
	if got := cli.success(t, ctx, "exec", name, "--", "cat", "/workspace/in.txt"); got != string(payload) {
		t.Fatalf("SecondBox scenario CLI exec stdout=%q", got)
	}
	if got := cli.success(t, ctx, "cp", name+":/workspace/in.txt", output); got != "" {
		t.Fatalf("SecondBox scenario CLI cp download stdout=%q", got)
	}
	downloaded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatalf("SecondBox scenario CLI cp bytes=%q", downloaded)
	}

	// Snapshots require a stopped Sandbox; the friendly snapshot verb does not
	// implicitly stop it. Preserve that explicit lifecycle contract.
	scenarioCLITransition(t, ctx, cli, fixture, originalHandle, name, "stop", sb.SandboxStateStopped)
	snapshotOperation := scenarioCLIJSON[sb.Operation](t, cli.success(t, ctx, "snapshot", name, "--name", "golden"))
	if snapshotOperation.ID == "" || snapshotOperation.Snapshot == nil || snapshotOperation.Snapshot.Name != "golden" {
		t.Fatalf("SecondBox scenario CLI snapshot Operation: %+v", snapshotOperation)
	}
	snapshot := scenarioJSON[contracts.Snapshot](t, ctx, fixture.subject, "getSnapshot", sb.CallOptions{PathParameters: map[string]string{"snapshotId": snapshotOperation.Snapshot.ID}})
	if snapshot.State != "ready" || snapshot.SandboxID != original.ID {
		t.Fatalf("SecondBox scenario CLI snapshot not ready: %+v", snapshot)
	}
	created := scenarioCLIJSON[sb.Operation](t, cli.success(t, ctx, "create", profile.Name, "--memory", "1GiB", "--name", cloneName, "--from", name+"/golden"))
	if created.ID == "" || created.SandboxID == "" || created.SandboxID == original.ID {
		t.Fatalf("SecondBox scenario CLI clone Operation: %+v", created)
	}
	cloneHandle := sb.NewSandboxHandle(fixture.subject, sb.Sandbox{ID: created.SandboxID})
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	waitForSandbox(t, ctx, cloneHandle, sb.SandboxStateReady)
	// The running Profile also boots clones. Stop before explicitly exercising
	// start; create has no initial-state override.
	scenarioCLITransition(t, ctx, cli, fixture, cloneHandle, cloneName, "stop", sb.SandboxStateStopped)
	scenarioCLITransition(t, ctx, cli, fixture, cloneHandle, cloneName, "start", sb.SandboxStateReady)
	if got := cli.success(t, ctx, "exec", cloneName, "--", "cat", "/workspace/in.txt"); got != string(payload) {
		t.Fatalf("SecondBox scenario CLI Snapshot clone stdout=%q", got)
	}
	scenarioCLITransition(t, ctx, cli, fixture, originalHandle, name, "start", sb.SandboxStateReady)
	scenarioCLITransition(t, ctx, cli, fixture, originalHandle, name, "stop", sb.SandboxStateStopped)
	scenarioCLITransition(t, ctx, cli, fixture, originalHandle, name, "start", sb.SandboxStateReady)
	page := scenarioCLIJSON[sb.SandboxPage](t, cli.success(t, ctx, "ls", "--name", name))
	if len(page.Items) != 1 || page.Items[0].ID != original.ID || page.Items[0].State != sb.SandboxStateReady {
		t.Fatalf("SecondBox scenario CLI ls: %+v", page)
	}
	scenarioCLITransition(t, ctx, cli, fixture, cloneHandle, cloneName, "rm", sb.SandboxStateDeleted)
	scenarioCLITransition(t, ctx, cli, fixture, originalHandle, name, "rm", sb.SandboxStateDeleted)
}

// Piped lifecycle stdout is the admitted Operation, even though the CLI waits
// for completion. Assert both that public output and the resulting Sandbox.
func scenarioCLITransition(t *testing.T, ctx context.Context, cli scenarioCLI, fixture scenarioFixture, handle *sb.SandboxHandle, name, verb string, state sb.SandboxState) {
	t.Helper()
	args := []string{verb, name}
	if verb == "rm" {
		args = append(args, "--force")
	}
	operation := scenarioCLIJSON[sb.Operation](t, cli.success(t, ctx, args...))
	if operation.ID == "" || operation.SandboxID != handle.Snapshot().ID {
		t.Fatalf("SecondBox scenario CLI %s Operation: %+v", verb, operation)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	waitForSandbox(t, ctx, handle, state)
}
