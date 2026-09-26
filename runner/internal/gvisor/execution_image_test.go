//go:build linux

package gvisor

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage/executionimagetest"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const selectedImageTestDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func selectedImageTestBackend(t *testing.T, cacheRoot string, publisher *executionimagetest.Publisher) *AssignmentBackend {
	t.Helper()
	verifier, err := executionimage.NewVerifier(cacheRoot, publisher.PublicKeyPath, publisher.PublicKeySHA256)
	if err != nil {
		t.Fatal(err)
	}
	return &AssignmentBackend{
		config: validatedConfig{manifest: materialization.Manifest{
			Key: materialization.Key{
				BackendKind: materialization.BackendGVisor, GuestArchitecture: runtime.GOARCH,
				RuntimeManifestDigest:   "sha256:" + strings.Repeat("a", 64),
				ToolchainManifestDigest: "sha256:" + strings.Repeat("b", 64),
			},
			AgentProtocolGeneration: 1,
			BackendBuildID:          "secondbox-gvisor-test",
		}},
		executionImages: verifier,
	}
}

func publishSelectedImage(t *testing.T, cacheRoot string, publisher *executionimagetest.Publisher, rootfs []byte, features []string) executionimagetest.Bundle {
	t.Helper()
	directory := filepath.Join(cacheRoot, strings.TrimPrefix(selectedImageTestDigest, "sha256:"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "rootfs.ext4"), rootfs, 0o600); err != nil {
		t.Fatal(err)
	}
	return publisher.WriteBundle(t, directory, features)
}

func selectedImageAssignment(bundle executionimagetest.Bundle) *runnerprotocol.AssignmentCommand {
	return &runnerprotocol.AssignmentCommand{
		ExecutionImage: &runnerprotocol.ExecutionImage{Reference: "registry.example/agent@" + selectedImageTestDigest},
		Assets: []*runnerprotocol.AssetReference{
			{ArtifactId: bundle.RuntimeArtifactID, ManifestDigest: bundle.RuntimeManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1, MandatoryGuestFeatures: []string{}},
			{ArtifactId: bundle.ToolchainArtifactID, ManifestDigest: bundle.ToolchainManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1, MandatoryGuestFeatures: []string{}},
		},
	}
}

func noSelectedImageProgress(runnerprotocol.AssignmentProgressStage) error { return nil }

func TestSelectedImageAssignmentRequiresADigestAndThePinnedAgentGeneration(t *testing.T) {
	publisher := executionimagetest.NewPublisher(t)
	backend := selectedImageTestBackend(t, t.TempDir(), publisher)
	valid := selectedImageAssignment(executionimagetest.Bundle{
		RuntimeArtifactID: "runtime", RuntimeManifestDigest: "sha256:" + strings.Repeat("c", 64),
		ToolchainArtifactID: "toolchain", ToolchainManifestDigest: "sha256:" + strings.Repeat("d", 64),
	})
	if err := backend.validateSelectedImageAssignment(valid); err != nil {
		t.Fatalf("digest-pinned selected image was rejected: %v", err)
	}
	tagged := selectedImageAssignment(executionimagetest.Bundle{})
	tagged.Assets = valid.Assets
	tagged.ExecutionImage.Reference = "registry.example/agent:latest"
	if err := backend.validateSelectedImageAssignment(tagged); err == nil ||
		!strings.Contains(err.Error(), "resolved digest") {
		t.Fatalf("mutable tag error = %v", err)
	}
	newerGuest := selectedImageAssignment(executionimagetest.Bundle{})
	newerGuest.Assets = []*runnerprotocol.AssetReference{
		{ArtifactId: "runtime", ManifestDigest: valid.Assets[0].ManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 2},
		{ArtifactId: "toolchain", ManifestDigest: valid.Assets[1].ManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 2},
	}
	if err := backend.validateSelectedImageAssignment(newerGuest); err == nil ||
		!strings.Contains(err.Error(), "pinned guest agent") {
		t.Fatalf("unsupported guest generation error = %v", err)
	}
}

func TestStageSelectedImageRejectsAssetsTheBundleDidNotSign(t *testing.T) {
	cacheRoot := t.TempDir()
	publisher := executionimagetest.NewPublisher(t)
	bundle := publishSelectedImage(t, cacheRoot, publisher, []byte("selected-root"), nil)
	backend := selectedImageTestBackend(t, cacheRoot, publisher)
	assignment := selectedImageAssignment(bundle)
	assignment.Assets[0].ManifestDigest = "sha256:" + strings.Repeat("e", 64)
	if _, _, err := backend.stageSelectedImage(t.Context(), assignment, noSelectedImageProgress); err == nil ||
		!strings.Contains(err.Error(), "runtime asset") {
		t.Fatalf("unsigned runtime asset error = %v", err)
	}
	absent := selectedImageAssignment(bundle)
	absent.ExecutionImage.Reference = "registry.example/agent@sha256:" + strings.Repeat("3", 64)
	if _, _, err := backend.stageSelectedImage(t.Context(), absent, noSelectedImageProgress); err == nil ||
		!strings.Contains(err.Error(), "selected image verification") {
		t.Fatalf("uncached selected image error = %v", err)
	}
}

func TestStageSelectedImageClonesTheVerifiedRootAndReportsItsIdentity(t *testing.T) {
	parent := os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM")
	if parent == "" {
		t.Skip("real reflink qualification filesystem must be explicit")
	}
	cacheRoot, err := os.MkdirTemp(parent, "secondbox-gvisor-selected-image-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cacheRoot) })
	publisher := executionimagetest.NewPublisher(t)
	content := bytes.Repeat([]byte("selected-root"), 1024)
	bundle := publishSelectedImage(t, cacheRoot, publisher, content, []string{"activity_events"})
	backend := selectedImageTestBackend(t, cacheRoot, publisher)
	assignment := selectedImageAssignment(bundle)
	for _, asset := range assignment.Assets {
		asset.MandatoryGuestFeatures = []string{"activity_events"}
	}
	var stages []runnerprotocol.AssignmentProgressStage
	clone, identity, err := backend.stageSelectedImage(t.Context(), assignment, func(stage runnerprotocol.AssignmentProgressStage) error {
		stages = append(stages, stage)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if !slices.Equal(stages, []runnerprotocol.AssignmentProgressStage{runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY}) {
		t.Fatalf("progress = %v", stages)
	}
	cloned, err := io.ReadAll(clone)
	if err != nil || !bytes.Equal(cloned, content) {
		t.Fatalf("staged root differs from the verified rootfs: %v", err)
	}
	if identity.buildID != "secondbox-gvisor-test" ||
		identity.imageDigest != bundle.RuntimeManifestDigest || identity.toolchainDigest != bundle.ToolchainManifestDigest ||
		identity.requestedReference != "registry.example/agent@"+selectedImageTestDigest || identity.resolvedDigest != selectedImageTestDigest {
		t.Fatalf("selected image identity = %#v", identity)
	}
	if !slices.Contains(identity.mandatoryFeatures, guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS) ||
		!slices.Contains(identity.mandatoryFeatures, guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC) {
		t.Fatalf("mandatory guest features = %v", identity.mandatoryFeatures)
	}
	// The digest lock was released with the clone, so a later preparation
	// or eviction of the same digest is not blocked by the running Instance.
	prepared, err := backend.executionImages.VerifyLocal(t.Context(), assignment.ExecutionImage, noSelectedImageProgress)
	if err != nil {
		t.Fatalf("digest lock was retained after staging: %v", err)
	}
	prepared.Release()
}
