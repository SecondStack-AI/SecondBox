//go:build linux

package gvisor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// A client-selected assignment names its image only by resolved digest.
var selectedImageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*/[A-Za-z0-9][A-Za-z0-9._/-]*@sha256:[a-f0-9]{64}$`)

// guestLaunchIdentity is what the launched guest agent is told and must echo,
// plus the image identity the Instance reports back to the control plane.
type guestLaunchIdentity struct {
	buildID            string
	imageDigest        string
	toolchainDigest    string
	mandatoryFeatures  []guestv1.GuestFeature
	requestedReference string
	resolvedDigest     string
}

// baseMandatoryGuestFeatures is what every gVisor Instance requires of its
// guest, whatever supplies the root.
var baseMandatoryGuestFeatures = []guestv1.GuestFeature{
	guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
	guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
	guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
	guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
}

func (backend *AssignmentBackend) fixedGuestIdentity() guestLaunchIdentity {
	manifest := backend.config.manifest
	return guestLaunchIdentity{
		buildID:           manifest.BackendBuildID,
		imageDigest:       manifest.Key.RuntimeManifestDigest,
		toolchainDigest:   manifest.Key.ToolchainManifestDigest,
		mandatoryFeatures: slices.Clone(baseMandatoryGuestFeatures),
	}
}

// validateSelectedImageAssignment checks, without I/O, that the pinned guest
// agent can serve the assignment's signed components. Their digests are
// matched to the locally verified bundle at start.
func (backend *AssignmentBackend) validateSelectedImageAssignment(assignment *runnerprotocol.AssignmentCommand) error {
	if !selectedImageReferencePattern.MatchString(assignment.ExecutionImage.GetReference()) {
		return fmt.Errorf("SecondBox gVisor assignment execution image must be a resolved digest reference")
	}
	manifest := backend.config.manifest
	if len(assignment.Assets) != 2 {
		return fmt.Errorf("SecondBox gVisor assignment must select exactly runtime and toolchain assets")
	}
	for _, asset := range assignment.Assets {
		if asset == nil || asset.Architecture != manifest.Key.GuestArchitecture ||
			asset.GuestProtocolGeneration != manifest.AgentProtocolGeneration {
			return fmt.Errorf("SecondBox gVisor selected image generation or architecture differs from the pinned guest agent")
		}
	}
	return nil
}

// stageSelectedImage verifies the cached signed bundle through the shared
// verifier, matches the assignment to its signed components, and reflinks its
// rootfs into an unnamed clone. The digest lock is held only until the clone
// exists; later cache eviction cannot reach the Instance.
func (backend *AssignmentBackend) stageSelectedImage(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (*os.File, guestLaunchIdentity, error) {
	prepared, err := backend.executionImages.VerifyLocal(ctx, assignment.ExecutionImage, progress)
	if err != nil {
		return nil, guestLaunchIdentity{}, artifactAssignment(fmt.Errorf("SecondBox gVisor selected image verification: %w", err))
	}
	defer prepared.Release()
	start, err := firecracker.ResolveSignedBundleGuestStart(assignment, prepared.Directory)
	if err != nil {
		return nil, guestLaunchIdentity{}, artifactAssignment(err)
	}
	imageFeatures, err := firecracker.GuestFeaturesFromContractNames(start.MandatoryFeatures)
	if err != nil {
		return nil, guestLaunchIdentity{}, artifactAssignment(err)
	}
	mandatory := slices.Clone(baseMandatoryGuestFeatures)
	for _, feature := range imageFeatures {
		if !slices.Contains(mandatory, feature) {
			mandatory = append(mandatory, feature)
		}
	}
	clone, err := backend.executionImages.CloneVerifiedRootfs(prepared)
	if err != nil {
		return nil, guestLaunchIdentity{}, artifactAssignment(err)
	}
	return clone, guestLaunchIdentity{
		// The pinned materialization agent runs, so it names the guest build;
		// the image names the userspace the guest reports.
		buildID:            backend.config.manifest.BackendBuildID,
		imageDigest:        start.ImageManifestDigest,
		toolchainDigest:    start.ToolchainManifestDigest,
		mandatoryFeatures:  mandatory,
		requestedReference: assignment.ExecutionImage.Reference,
		resolvedDigest:     prepared.ResolvedDigest,
	}, nil
}

// PrepareExecutionImage retrieves through the host-private fetcher exactly as
// the Firecracker Runner does.
func (backend *AssignmentBackend) PrepareExecutionImage(
	ctx context.Context,
	command *runnerprotocol.PrepareImageCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (*runnerprotocol.PrepareImageResult, error) {
	return executionimage.PrepareThroughFetcher(ctx, backend.config.ExecutionImageFetcherSocket, command, progress)
}
