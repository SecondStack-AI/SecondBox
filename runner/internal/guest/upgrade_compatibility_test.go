package microvmguest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

func TestGuestUpgradeGenerationSkewIsResolvedBeforeWorkspaceMutation(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		minimum   uint32
		maximum   uint32
		selected  uint32
		rejection guestv1.HandshakeRejectionKind
	}{
		{name: "window_includes_guest", minimum: 1, maximum: 3, selected: 1},
		{
			name: "window_excludes_guest", minimum: 2, maximum: 3,
			rejection: guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_VERSION_UNSUPPORTED,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			sentinelPath := filepath.Join(workspace, "upgrade-sentinel")
			if err := os.WriteFile(sentinelPath, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
				InstanceID:              "instance-1",
				SandboxID:               "sandbox-1",
				SandboxGeneration:       7,
				GuestBuildID:            "guest-build-1",
				ImageManifestDigest:     "sha256:image",
				ToolchainManifestDigest: "sha256:toolchain",
				HeartbeatInterval:       time.Second,
			}, workspace)
			defer cleanup()
			if err := stream.Send(&guestv1.RunnerToGuest{
				Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
					Binding:                         protocolTestBinding(),
					SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: testCase.minimum, Maximum: testCase.maximum},
					ExpectedImageManifestDigest:     "sha256:image",
					ExpectedToolchainManifestDigest: "sha256:toolchain",
				}},
			}); err != nil {
				t.Fatal(err)
			}
			response, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if testCase.rejection != guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_UNSPECIFIED {
				if got := response.GetRejection().GetKind(); got != testCase.rejection {
					t.Fatalf("guest protocol rejection = %s, want %s", got, testCase.rejection)
				}
			} else if got := response.GetWelcome().GetSelectedGeneration(); got != testCase.selected {
				t.Fatalf("selected guest protocol generation = %d, want %d", got, testCase.selected)
			}
			content, err := os.ReadFile(sentinelPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "unchanged" {
				t.Fatalf("workspace mutated during guest generation negotiation: %q", content)
			}
		})
	}
}
