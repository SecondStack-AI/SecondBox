package conformance

import (
	"context"
	"io"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

type OpenStream func(context.Context) (guestv1.GuestAgent_ConnectClient, io.Closer, error)

type CloseFunc func()

func (closeFunc CloseFunc) Close() error {
	closeFunc()
	return nil
}

// RunV1Negotiation proves the same canonical handshake behavior over any
// concrete gRPC transport supplied by the caller.
func RunV1Negotiation(t *testing.T, open OpenStream) {
	t.Helper()
	for _, testCase := range []struct {
		name       string
		generation *guestv1.ProtocolGenerationRange
		binding    *guestv1.ConnectionBinding
		mandatory  []guestv1.GuestFeature
		rejection  guestv1.HandshakeRejectionKind
	}{
		{
			name:       "welcome",
			generation: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			binding:    binding(7),
			mandatory: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
				guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			},
		},
		{
			name:       "future generation",
			generation: &guestv1.ProtocolGenerationRange{Minimum: 2, Maximum: 2},
			binding:    binding(7),
			rejection:  guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_VERSION_UNSUPPORTED,
		},
		{
			name:       "binding mismatch",
			generation: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			binding:    binding(8),
			rejection:  guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH,
		},
		{
			name:       "mandatory feature unavailable",
			generation: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			binding:    binding(7),
			mandatory:  []guestv1.GuestFeature{guestv1.GuestFeature(99)},
			rejection:  guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_FEATURE_UNSUPPORTED,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, closer, err := open(ctx)
			if err != nil {
				t.Fatalf("open guest protocol stream: %v", err)
			}
			defer closer.Close()
			if err := stream.Send(&guestv1.RunnerToGuest{
				Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
					Binding:                         testCase.binding,
					SupportedGenerations:            testCase.generation,
					RequestedFeatures:               append([]guestv1.GuestFeature(nil), testCase.mandatory...),
					MandatoryFeatures:               append([]guestv1.GuestFeature(nil), testCase.mandatory...),
					ExpectedImageManifestDigest:     "sha256:image",
					ExpectedToolchainManifestDigest: "sha256:toolchain",
				}},
			}); err != nil {
				t.Fatalf("send hello: %v", err)
			}
			response, err := stream.Recv()
			if err != nil {
				t.Fatalf("receive handshake response: %v", err)
			}
			if testCase.rejection == guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_UNSPECIFIED {
				welcome := response.GetWelcome()
				if welcome == nil ||
					welcome.SelectedGeneration != 1 ||
					welcome.GuestBuildId != "guest-build-1" ||
					welcome.HeartbeatIntervalMs == 0 {
					t.Fatalf("welcome = %#v", welcome)
				}
				return
			}
			if got := response.GetRejection().GetKind(); got != testCase.rejection {
				t.Fatalf("rejection = %v, want %v", got, testCase.rejection)
			}
		})
	}
}

func binding(generation uint64) *guestv1.ConnectionBinding {
	return &guestv1.ConnectionBinding{
		InstanceId:        "instance-1",
		SandboxId:         "sandbox-1",
		SandboxGeneration: generation,
		ConnectionNonce:   []byte("0123456789abcdef0123456789abcdef"),
	}
}
