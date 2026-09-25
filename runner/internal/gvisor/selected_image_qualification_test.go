//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const qualificationSelectedImageDigest = "sha256:5e1ec7ed5e1ec7ed5e1ec7ed5e1ec7ed5e1ec7ed5e1ec7ed5e1ec7ed5e1ec7ed"

// TestQualifiedGVisorBackendBootsClientSelectedImage proves a signed image's
// rootfs.ext4 becomes the sandbox root on a real host: the image lacks the
// gVisor mount targets, the supervisor creates them in the private clone,
// the pinned agent negotiates the image's signed identity, the Workspace
// stays writable, and the cached bytes are never modified.
func TestQualifiedGVisorBackendBootsClientSelectedImage(t *testing.T) {
	fixture := newQualificationFixture(t, "selected")
	backend, fence := fixture.backend, fixture.fence
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })
	readiness, err := backend.Readiness(t.Context())
	if err != nil || !readiness.Capabilities.GetClientSelectedImageReady() {
		t.Fatalf("selected-image readiness: %+v %v", readiness, err)
	}

	source := filepath.Join(t.TempDir(), "userspace")
	if output, err := exec.Command("cp", "-a", fixture.rootfs+"/.", source).CombinedOutput(); err != nil {
		t.Fatalf("copy qualification userspace: %v: %s", err, output)
	}
	// An ordinary microVM image lacks the gVisor-only targets.
	for _, target := range []string{guestAgentPath, guestSocketDirectory, guestRuntimePrivatePath} {
		if err := os.RemoveAll(filepath.Join(source, target)); err != nil {
			t.Fatal(err)
		}
	}
	marker := []byte("selected-image-userspace\n")
	if err := os.WriteFile(filepath.Join(source, "etc", "secondbox-selected-image"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	bundleDirectory := filepath.Join(fixture.imageRoot, strings.TrimPrefix(qualificationSelectedImageDigest, "sha256:"))
	if err := os.MkdirAll(bundleDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	rootfsImage := filepath.Join(bundleDirectory, "rootfs.ext4")
	if output, err := exec.Command("mkfs.ext4", "-q", "-F", "-d", source, rootfsImage, "256M").CombinedOutput(); err != nil {
		t.Fatalf("build selected rootfs.ext4: %v: %s", err, output)
	}
	cachedBefore := qualificationDigestFile(t, rootfsImage)
	bundle := fixture.publisher.WriteBundle(t, bundleDirectory, nil)

	assignment := fixture.command
	assignment.ExecutionImage = &runnerprotocol.ExecutionImage{Reference: "registry.example/agent@" + qualificationSelectedImageDigest}
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "client-selected-image")
	assignment.Assets = []*runnerprotocol.AssetReference{
		{ArtifactId: bundle.RuntimeArtifactID, ManifestDigest: bundle.RuntimeManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1, MandatoryGuestFeatures: []string{}},
		{ArtifactId: bundle.ToolchainArtifactID, ManifestDigest: bundle.ToolchainManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1, MandatoryGuestFeatures: []string{}},
	}
	instance, err := backend.StartAssignment(t.Context(), assignment, func(runnerprotocol.AssignmentProgressStage) error { return nil })
	if err != nil {
		t.Fatalf("boot client-selected image: %v", err)
	}
	if instance.RequestedImageReference != assignment.ExecutionImage.Reference || instance.ResolvedImageDigest != qualificationSelectedImageDigest {
		t.Fatalf("selected image Instance identity = %#v", instance)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}
	execute := func(script string) runnercontrol.BufferedExecResult {
		t.Helper()
		result, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
			Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", script}}},
			Cwd:     ".", OutputLimitBytes: 4096,
		})
		if err != nil {
			t.Fatalf("exec %q: %v", script, err)
		}
		return result
	}
	if result := execute("cat /etc/secondbox-selected-image"); !bytes.Equal(result.Stdout, marker) || result.Terminal.GetExitCode() != 0 {
		t.Fatalf("selected userspace marker = %q stderr=%q", result.Stdout, result.Stderr)
	}
	if result := execute("printf persisted > selected.txt && cat selected.txt && echo scratch > /etc/secondbox-overlay"); string(result.Stdout) != "persisted" || result.Terminal.GetExitCode() != 0 {
		t.Fatalf("Workspace and root overlay writes = %q stderr=%q", result.Stdout, result.Stderr)
	}

	fenceEvidence, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{
		Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()),
	})
	if err != nil || fenceEvidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED {
		t.Fatalf("selected image fence = %#v, %v", fenceEvidence, err)
	}
	if after := qualificationDigestFile(t, rootfsImage); after != cachedBefore {
		t.Fatal("launching the selected image modified the cached rootfs")
	}
	// Teardown leaves no named staging file beside the cache entry and the
	// shared digest-lock directory.
	entries, err := os.ReadDir(fixture.imageRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(bundleDirectory) && entry.Name() != ".locks" {
			t.Fatalf("selected image launch left %q in the cache root", entry.Name())
		}
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mounts, []byte(shortInstanceDirName(fence.InstanceId))) {
		t.Fatal("selected image root mount leaked into the Runner namespace")
	}
}
