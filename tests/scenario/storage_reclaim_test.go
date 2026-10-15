//go:build scenario_live

package scenario_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// TestScenarioWorkspaceStopReclaimsFreedBlocksAndKeepsSnapshot deletes guest
// files that a Snapshot still holds, writes and deletes scratch data, and
// proves the stop releases both from the Workspace's physical meters while
// the Snapshot still restores the deleted file byte for byte.
func TestScenarioWorkspaceStopReclaimsFreedBlocksAndKeepsSnapshot(t *testing.T) {
	const fileMiB = 16
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	// Application authorities admit at most 32 Profile grants, all taken. The
	// observation test's identical request replays to the same Profile.
	profile := createScenarioProfile(t, fixture, "scenario-storage-observation", scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning))
	handle, operation := createScenarioSandbox(t, fixture, profile, "storage-reclaim")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	waitForScenarioOperation(t, ctx, fixture.subject, operation)

	digestCommand := "sha256sum /workspace/snapshotted.bin | cut -d' ' -f1"
	written := executeScenarioCommand(t, ctx, handle,
		"dd if=/dev/urandom of=/workspace/snapshotted.bin bs=1048576 count=16 2>/dev/null && sync && "+digestCommand,
		1024, "storage-reclaim-write")
	if written.ExecExited == nil || written.ExecExited.ExitCode != 0 {
		t.Fatalf("write Snapshot file: %s", describeScenarioExecOutcome(written))
	}
	digest := strings.TrimSpace(decodeScenarioOutput(t, written.ExecExited.Output.StdoutBase64))
	if len(digest) != 64 {
		t.Fatalf("Snapshot file digest = %q", digest)
	}
	stopScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-stop-for-snapshot")
	snapshot, _ := createScenarioSnapshot(t, ctx, fixture, handle, "storage-reclaim-point", map[string]string{"scenario": "storage-reclaim"}, false)
	startScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-start-after-snapshot")

	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle,
		"rm /workspace/snapshotted.bin && dd if=/dev/urandom of=/workspace/scratch.bin bs=1048576 count=16 2>/dev/null && sync && rm /workspace/scratch.bin && sync",
		1024, "storage-reclaim-delete"), 0, "", "")
	before := waitScenarioStorageObservation(t, ctx, fixture, handle.Snapshot().ID, time.Now().UTC())
	stopped := stopScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-stop")
	after := waitScenarioStorageObservation(t, ctx, fixture, stopped.ID, stopped.UpdatedAt)
	if before.AllocatedBytes == nil || after.AllocatedBytes == nil {
		t.Fatalf("allocated storage unavailable: before=%+v after=%+v", before, after)
	}
	t.Logf("stop reclaim: allocatedBytes %d -> %d", *before.AllocatedBytes, *after.AllocatedBytes)
	// The deleted file's 16 MiB of blocks are freed whether ext4 reused them
	// for the scratch data (exclusive copies) or not (still shared).
	if released := *before.AllocatedBytes - *after.AllocatedBytes; released < (fileMiB-2)<<20 {
		t.Fatalf("stop released %d allocated bytes after deleting %d MiB", released, fileMiB)
	}
	if before.ExclusiveBytes != nil && after.ExclusiveBytes != nil {
		t.Logf("stop reclaim: exclusiveBytes %d -> %d", *before.ExclusiveBytes, *after.ExclusiveBytes)
		if released := *before.ExclusiveBytes - *after.ExclusiveBytes; released < (fileMiB-2)<<20 {
			t.Fatalf("stop released %d exclusive bytes after deleting %d MiB of scratch data", released, fileMiB)
		}
	} else {
		t.Logf("exclusive bytes unavailable: before=%s after=%s", before.ExclusiveReason, after.ExclusiveReason)
	}

	restoreScenarioSnapshotWithReplay(t, ctx, fixture, handle, snapshot.ID)
	startScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-start-restored")
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handle, digestCommand, 1024, "storage-reclaim-verify"), 0, digest+"\n", "")
}

func waitScenarioStorageObservation(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	sandboxID string,
	after time.Time,
) contracts.WorkspaceStorageObservation {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		observed := scenarioJSON[contracts.Sandbox](t, ctx, fixture.subject, "getSandbox", secondboxclient.CallOptions{PathParameters: map[string]string{"sandboxId": sandboxID}})
		storage := observed.Workspace.StorageObservation
		if storage.Status == "available" && storage.ObservedAt != nil && storage.ObservedAt.After(after) &&
			(storage.ExclusiveBytes != nil || storage.ExclusiveReason == "exclusive_extents_encoded" || os.Getenv("SECONDBOX_SCENARIO_HOST_PLATFORM") == "darwin") {
			return storage
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Runner storage observation did not arrive: %+v", storage)
		case <-ticker.C:
		}
	}
}
