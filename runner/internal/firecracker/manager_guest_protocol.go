package firecracker

import (
	"context"
	"errors"
	"fmt"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

// requestedGuestProtocolFeatureNames is the feature set every start negotiates,
// in contract vocabulary. It is stated once because a snapshot-resume
// template's compatibility key records it: a template captured by a runner that
// asks for a different feature set is a different template.
var requestedGuestProtocolFeatureNames = []string{
	"streaming_exec",
	"pty_resize",
	"descriptor_pinned_filesystem",
	"activity_events",
	"port_proxy",
}

func requestedGuestProtocolFeatures() ([]guestv1.GuestFeature, error) {
	features := make([]guestv1.GuestFeature, 0, len(requestedGuestProtocolFeatureNames))
	for _, name := range requestedGuestProtocolFeatureNames {
		feature, err := guestFeatureFromContractName(name)
		if err != nil {
			return nil, err
		}
		features = append(features, feature)
	}
	return features, nil
}

// NegotiateAssignmentGuest establishes the canonical assignment-bound data
// plane. Assignment readiness must not be reported until this succeeds.
func (m *Manager) NegotiateAssignmentGuest(
	ctx context.Context,
	backendReference string,
	assignmentID string,
	start assignmentGuestProtocolStart,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	inst := m.instances[backendReference]
	m.mu.Unlock()
	if inst == nil {
		return fmt.Errorf("negotiate guest protocol for assignment %q: backend instance %q not found", assignmentID, backendReference)
	}
	if inst.guestProtocolSession == nil ||
		inst.guestProtocolSession.Binding == nil ||
		inst.guestProtocolSession.Binding.InstanceId != inst.compartmentID ||
		inst.guestProtocolSession.Binding.SandboxId != inst.sandboxID ||
		inst.guestProtocolSession.Binding.SandboxGeneration != inst.sandboxGeneration ||
		inst.guestProtocolSession.Generation != currentGuestProtocolGeneration ||
		inst.guestProtocolSession.GuestBuildID != start.GuestBuildID ||
		inst.guestProtocolSession.ImageManifestDigest != start.ImageManifestDigest ||
		inst.guestProtocolSession.ToolchainManifestDigest != start.ToolchainManifestDigest {
		return fmt.Errorf("negotiate guest protocol for assignment %q: backend session is not ready", assignmentID)
	}
	return nil
}

func (m *Manager) negotiateInstanceGuest(
	ctx context.Context,
	inst *instance,
	opts runtimemanager.StartOpts,
) error {
	mandatory := make([]guestv1.GuestFeature, 0, len(opts.MandatoryGuestFeatures))
	for _, name := range opts.MandatoryGuestFeatures {
		feature, err := guestFeatureFromContractName(name)
		if err != nil {
			return err
		}
		mandatory = append(mandatory, feature)
	}
	requested, err := requestedGuestProtocolFeatures()
	if err != nil {
		return err
	}
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath:                         inst.vsockUDS,
		Port:                            inst.guestProtocolPort,
		InstanceID:                      inst.compartmentID,
		SandboxID:                       inst.sandboxID,
		SandboxGeneration:               inst.sandboxGeneration,
		ExpectedGuestBuildID:            opts.GuestBuildID,
		ExpectedImageManifestDigest:     opts.ImageManifestDigest,
		ExpectedToolchainManifestDigest: opts.ToolchainManifestDigest,
		RequestedFeatures:               requested,
		MandatoryFeatures:               mandatory,
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	current := m.instances[inst.id]
	if current != inst {
		m.mu.Unlock()
		closeErr := session.Close()
		return errors.Join(fmt.Errorf("backend instance exited during guest protocol negotiation"), closeErr)
	}
	inst.guestProtocolSession = session
	m.mu.Unlock()
	return nil
}
