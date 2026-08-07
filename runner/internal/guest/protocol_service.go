package microvmguest

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

const (
	guestProtocolGeneration   uint32 = 1
	connectionNonceByteCount         = 32
	maxGuestHeartbeatInterval        = 60 * time.Second
)

// ProtocolIdentity is immutable boot identity used to bind a guest protocol
// connection to the exact runner assignment and signed image.
type ProtocolIdentity struct {
	InstanceID              string
	SandboxID               string
	SandboxGeneration       uint64
	GuestBuildID            string
	ImageManifestDigest     string
	ToolchainManifestDigest string
	HeartbeatInterval       time.Duration
}

// ProtocolService implements the canonical guest v1 bidirectional stream.
type ProtocolService struct {
	guestv1.UnimplementedGuestAgentServer
	server   Server
	identity ProtocolIdentity
}

func NewProtocolService(server Server, identity ProtocolIdentity) (*ProtocolService, error) {
	if strings.TrimSpace(server.WorkspaceDir) == "" || strings.TrimSpace(server.RuntimePrivateDir) == "" {
		return nil, fmt.Errorf("guest protocol workspace and runtime-private mount paths are required")
	}
	if strings.TrimSpace(identity.InstanceID) == "" ||
		strings.TrimSpace(identity.SandboxID) == "" ||
		identity.SandboxGeneration == 0 ||
		strings.TrimSpace(identity.GuestBuildID) == "" ||
		strings.TrimSpace(identity.ImageManifestDigest) == "" ||
		strings.TrimSpace(identity.ToolchainManifestDigest) == "" {
		return nil, fmt.Errorf("guest protocol boot identity is incomplete")
	}
	if identity.HeartbeatInterval < time.Millisecond || identity.HeartbeatInterval > maxGuestHeartbeatInterval {
		return nil, fmt.Errorf("guest protocol heartbeat interval must be from 1ms through %s", maxGuestHeartbeatInterval)
	}
	return &ProtocolService{server: server, identity: identity}, nil
}

// NewTemplateProtocolService builds the protocol service of an identity-neutral
// template guest. Its identity arrives later through the control endpoint's one
// permitted assignment bind, so every handshake before that bind is rejected.
func NewTemplateProtocolService(server Server) (*ProtocolService, error) {
	if strings.TrimSpace(server.WorkspaceDir) == "" || strings.TrimSpace(server.RuntimePrivateDir) == "" {
		return nil, fmt.Errorf("guest protocol workspace and runtime-private mount paths are required")
	}
	if server.Assignment == nil {
		return nil, fmt.Errorf("template guest protocol requires an assignment gate")
	}
	return &ProtocolService{server: server}, nil
}

// boundIdentity resolves the identity this service negotiates against. A
// template-mode service has none until its assignment bind succeeds.
func (s *ProtocolService) boundIdentity() (ProtocolIdentity, bool) {
	if s.server.Assignment == nil {
		return s.identity, true
	}
	return s.server.Assignment.Identity()
}

func (s *ProtocolService) Connect(stream guestv1.GuestAgent_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive guest protocol hello: %w", err)
	}
	hello := first.GetHello()
	if hello == nil {
		return s.rejectHandshake(stream, guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_INVALID_HELLO, "first frame must be hello")
	}
	identity, bound := s.boundIdentity()
	if !bound {
		return s.rejectHandshake(
			stream,
			guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH,
			"guest has no assignment identity",
		)
	}
	rejection, enabled := s.negotiate(identity, hello)
	if rejection != nil {
		return s.rejectHandshake(stream, rejection.Kind, rejection.SafeDetail)
	}
	if err := stream.Send(&guestv1.GuestToRunner{
		Message: &guestv1.GuestToRunner_Welcome{Welcome: &guestv1.Welcome{
			Binding:                 cloneConnectionBinding(hello.Binding),
			SelectedGeneration:      guestProtocolGeneration,
			EnabledFeatures:         enabled,
			GuestBuildId:            identity.GuestBuildID,
			ImageManifestDigest:     identity.ImageManifestDigest,
			ToolchainManifestDigest: identity.ToolchainManifestDigest,
			HeartbeatIntervalMs:     uint64(identity.HeartbeatInterval.Milliseconds()),
		}},
	}); err != nil {
		return fmt.Errorf("send guest protocol welcome: %w", err)
	}
	return s.serveNegotiated(stream, hello.Binding, enabled)
}

func (s *ProtocolService) negotiate(
	identity ProtocolIdentity,
	hello *guestv1.Hello,
) (*guestv1.HandshakeRejection, []guestv1.GuestFeature) {
	if hello.Binding == nil ||
		strings.TrimSpace(hello.Binding.InstanceId) == "" ||
		strings.TrimSpace(hello.Binding.SandboxId) == "" ||
		hello.Binding.SandboxGeneration == 0 ||
		len(hello.Binding.ConnectionNonce) != connectionNonceByteCount ||
		hello.SupportedGenerations == nil ||
		hello.SupportedGenerations.Minimum == 0 ||
		hello.SupportedGenerations.Maximum < hello.SupportedGenerations.Minimum {
		return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_INVALID_HELLO, "hello is incomplete"), nil
	}
	if hello.Binding.InstanceId != identity.InstanceID ||
		hello.Binding.SandboxId != identity.SandboxID ||
		hello.Binding.SandboxGeneration != identity.SandboxGeneration {
		return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH, "connection binding does not match boot identity"), nil
	}
	if hello.SupportedGenerations.Minimum > guestProtocolGeneration ||
		hello.SupportedGenerations.Maximum < guestProtocolGeneration {
		return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_VERSION_UNSUPPORTED, "protocol generation is unsupported"), nil
	}
	if hello.ExpectedImageManifestDigest != identity.ImageManifestDigest {
		return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_IMAGE_MISMATCH, "image manifest digest does not match boot identity"), nil
	}
	if hello.ExpectedToolchainManifestDigest != identity.ToolchainManifestDigest {
		return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_TOOLCHAIN_MISMATCH, "toolchain manifest digest does not match boot identity"), nil
	}

	supported := map[guestv1.GuestFeature]bool{
		guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC:               true,
		guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE:                   true,
		guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM: true,
		guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS:              true,
		guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY:                   true,
	}
	for _, feature := range hello.MandatoryFeatures {
		if feature == guestv1.GuestFeature_GUEST_FEATURE_UNSPECIFIED || !supported[feature] {
			return handshakeRejection(guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_FEATURE_UNSUPPORTED, "mandatory guest feature is unsupported"), nil
		}
	}
	enabled := make([]guestv1.GuestFeature, 0, len(hello.RequestedFeatures))
	seen := map[guestv1.GuestFeature]bool{}
	for _, feature := range hello.RequestedFeatures {
		if supported[feature] && !seen[feature] {
			enabled = append(enabled, feature)
			seen[feature] = true
		}
	}
	for _, feature := range hello.MandatoryFeatures {
		if !seen[feature] {
			enabled = append(enabled, feature)
			seen[feature] = true
		}
	}
	return nil, enabled
}

func (s *ProtocolService) rejectHandshake(
	stream guestv1.GuestAgent_ConnectServer,
	kind guestv1.HandshakeRejectionKind,
	detail string,
) error {
	if err := stream.Send(&guestv1.GuestToRunner{
		Message: &guestv1.GuestToRunner_Rejection{Rejection: handshakeRejection(kind, detail)},
	}); err != nil {
		return fmt.Errorf("send guest protocol handshake rejection: %w", err)
	}
	return nil
}

func handshakeRejection(kind guestv1.HandshakeRejectionKind, detail string) *guestv1.HandshakeRejection {
	return &guestv1.HandshakeRejection{Kind: kind, SafeDetail: detail}
}

func cloneConnectionBinding(binding *guestv1.ConnectionBinding) *guestv1.ConnectionBinding {
	if binding == nil {
		return nil
	}
	return &guestv1.ConnectionBinding{
		InstanceId:        binding.InstanceId,
		SandboxId:         binding.SandboxId,
		SandboxGeneration: binding.SandboxGeneration,
		ConnectionNonce:   bytes.Clone(binding.ConnectionNonce),
	}
}
