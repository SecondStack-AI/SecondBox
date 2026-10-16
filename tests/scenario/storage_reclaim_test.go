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
// proves the stop releases both from the Workspace's physical meters, that the
// reclaimed Workspace boots with its retained files intact, and that the
// Snapshot still restores the deleted file byte for byte.
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

	digests := func(key string, command string) string {
		t.Helper()
		outcome := executeScenarioCommand(t, ctx, handle, "cd /workspace && "+command, 1024, key)
		if outcome.ExecExited == nil || outcome.ExecExited.ExitCode != 0 {
			t.Fatalf("%s: %s", key, describeScenarioExecOutcome(outcome))
		}
		return decodeScenarioOutput(t, outcome.ExecExited.Output.StdoutBase64)
	}
	// retained.bin is shared with the Snapshot; kept.bin is written after it,
	// next to the blocks the guest frees, so it is exclusive to the Workspace.
	snapshotDigests := digests("storage-reclaim-write",
		"dd if=/dev/urandom of=snapshotted.bin bs=1048576 count=16 2>/dev/null && "+
			"dd if=/dev/urandom of=retained.bin bs=1048576 count=4 2>/dev/null && sync && "+
			"sha256sum snapshotted.bin retained.bin")
	if strings.Count(snapshotDigests, "\n") != 2 {
		t.Fatalf("Snapshot file digests = %q", snapshotDigests)
	}
	stopScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-stop-for-snapshot")
	snapshot, _ := createScenarioSnapshot(t, ctx, fixture, handle, "storage-reclaim-point", map[string]string{"scenario": "storage-reclaim"}, false)
	startScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-start-after-snapshot")

	retainedDigests := digests("storage-reclaim-delete",
		"rm snapshotted.bin && dd if=/dev/urandom of=scratch.bin bs=1048576 count=16 2>/dev/null && "+
			"dd if=/dev/urandom of=kept.bin bs=1048576 count=4 2>/dev/null && sync && "+
			"rm scratch.bin && sync && sha256sum retained.bin kept.bin")
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

	// Boot the reclaimed image itself: a fresh guest reads the retained files
	// from disk, so any punch into live blocks would change their digests.
	startScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-start-reclaimed")
	if actual := digests("storage-reclaim-verify-retained", "test ! -e snapshotted.bin && sha256sum retained.bin kept.bin"); actual != retainedDigests {
		t.Fatalf("reclaimed Workspace files = %q, want %q", actual, retainedDigests)
	}
	stopScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-stop-for-restore")

	restoreScenarioSnapshotWithReplay(t, ctx, fixture, handle, snapshot.ID)
	startScenarioSandbox(t, ctx, fixture, handle, "storage-reclaim-start-restored")
	if actual := digests("storage-reclaim-verify-snapshot", "sha256sum snapshotted.bin retained.bin"); actual != snapshotDigests {
		t.Fatalf("restored Snapshot files = %q, want %q", actual, snapshotDigests)
	}
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
