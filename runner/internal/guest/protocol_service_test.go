package microvmguest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	guestconformance "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol/conformance"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestProtocolServiceNegotiatesBoundFeatureWindow(t *testing.T) {
	stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
		HeartbeatInterval:       time.Second,
	}, t.TempDir())
	defer cleanup()

	binding := protocolTestBinding()
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:              binding,
			SupportedGenerations: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			RequestedFeatures: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
				guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
				guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
			},
			MandatoryFeatures: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			},
			ExpectedImageManifestDigest:     "sha256:image",
			ExpectedToolchainManifestDigest: "sha256:toolchain",
		}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive welcome: %v", err)
	}
	welcome := frame.GetWelcome()
	if welcome == nil {
		t.Fatalf("first response = %#v, want welcome", frame)
	}
	if welcome.SelectedGeneration != 1 ||
		welcome.Binding.InstanceId != binding.InstanceId ||
		welcome.Binding.SandboxGeneration != binding.SandboxGeneration ||
		welcome.GuestBuildId != "guest-build-1" ||
		welcome.ImageManifestDigest != "sha256:image" {
		t.Fatalf("welcome = %#v", welcome)
	}
	if !protocolFeatureEnabled(welcome.EnabledFeatures, guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC) ||
		!protocolFeatureEnabled(welcome.EnabledFeatures, guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM) ||
		!protocolFeatureEnabled(welcome.EnabledFeatures, guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY) {
		t.Fatalf("enabled features = %v", welcome.EnabledFeatures)
	}
}

func TestProtocolServiceBufconnTransportConformance(t *testing.T) {
	guestconformance.RunV1Negotiation(t, func(context.Context) (guestv1.GuestAgent_ConnectClient, io.Closer, error) {
		stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
			InstanceID:              "instance-1",
			SandboxID:               "sandbox-1",
			SandboxGeneration:       7,
			GuestBuildID:            "guest-build-1",
			ImageManifestDigest:     "sha256:image",
			ToolchainManifestDigest: "sha256:toolchain",
			HeartbeatInterval:       time.Second,
		}, t.TempDir())
		return stream, guestconformance.CloseFunc(cleanup), nil
	})
}

func TestProtocolServiceMatchesFrozenHandshakeFixtures(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/guest/v1/fixtures/protocol_cases.json")
	if err != nil {
		t.Fatalf("read frozen guest protocol fixtures: %v", err)
	}
	var fixture struct {
		HandshakeCases []struct {
			Name               string   `json:"name"`
			GuestMinimum       uint32   `json:"guest_minimum"`
			GuestMaximum       uint32   `json:"guest_maximum"`
			MandatoryFeatures  []string `json:"mandatory_features"`
			BindingMatches     bool     `json:"binding_matches"`
			SelectedGeneration uint32   `json:"selected_generation"`
			Rejection          string   `json:"rejection"`
		} `json:"handshake_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode frozen guest protocol fixtures: %v", err)
	}
	for _, testCase := range fixture.HandshakeCases {
		t.Run(testCase.Name, func(t *testing.T) {
			stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
				InstanceID:              "instance-1",
				SandboxID:               "sandbox-1",
				SandboxGeneration:       7,
				GuestBuildID:            "guest-build-1",
				ImageManifestDigest:     "sha256:image",
				ToolchainManifestDigest: "sha256:toolchain",
				HeartbeatInterval:       time.Second,
			}, t.TempDir())
			defer cleanup()
			binding := protocolTestBinding()
			if !testCase.BindingMatches {
				binding.SandboxGeneration++
			}
			mandatory := make([]guestv1.GuestFeature, 0, len(testCase.MandatoryFeatures))
			for _, name := range testCase.MandatoryFeatures {
				mandatory = append(mandatory, protocolTestFeature(t, name))
			}
			if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
				Binding:                         binding,
				SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: testCase.GuestMinimum, Maximum: testCase.GuestMaximum},
				RequestedFeatures:               mandatory,
				MandatoryFeatures:               mandatory,
				ExpectedImageManifestDigest:     "sha256:image",
				ExpectedToolchainManifestDigest: "sha256:toolchain",
			}}}); err != nil {
				t.Fatalf("send fixture hello: %v", err)
			}
			response, err := stream.Recv()
			if err != nil {
				t.Fatalf("receive fixture response: %v", err)
			}
			if testCase.Rejection == "" {
				if response.GetWelcome().GetSelectedGeneration() != testCase.SelectedGeneration {
					t.Fatalf("welcome = %#v", response.GetWelcome())
				}
				return
			}
			want := map[string]guestv1.HandshakeRejectionKind{
				"version_unsupported": guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_VERSION_UNSUPPORTED,
				"feature_unsupported": guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_FEATURE_UNSUPPORTED,
				"binding_mismatch":    guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH,
			}[testCase.Rejection]
			if got := response.GetRejection().GetKind(); got != want {
				t.Fatalf("rejection = %v, want %v", got, want)
			}
		})
	}
}

func TestGuestProtocolFrozenDescriptorAndFixtureHashes(t *testing.T) {
	descriptor, err := os.ReadFile("../../../contracts/guest/v1/guest.descriptor.pb")
	if err != nil {
		t.Fatal(err)
	}
	digestFile, err := os.ReadFile("../../../contracts/guest/v1/guest.descriptor.sha256")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(digestFile))
	if len(fields) != 2 {
		t.Fatalf("descriptor digest file = %q", digestFile)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(descriptor)); got != fields[0] {
		t.Fatalf("descriptor digest = %s, want %s", got, fields[0])
	}
	fixture, err := os.ReadFile("../../../contracts/guest/v1/fixtures/protocol_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	const frozenFixtureSHA256 = "ca2a7848c8427c2390624027f66234ced66062218d005ee9df6cfed6e27c9271"
	if got := fmt.Sprintf("%x", sha256.Sum256(fixture)); got != frozenFixtureSHA256 {
		t.Fatalf("frozen fixture digest = %s, want %s", got, frozenFixtureSHA256)
	}
}

func TestProtocolServiceRejectsMismatchedBindingBeforeReady(t *testing.T) {
	stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
		HeartbeatInterval:       time.Second,
	}, t.TempDir())
	defer cleanup()

	binding := protocolTestBinding()
	binding.SandboxGeneration++
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:                         binding,
			SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			ExpectedImageManifestDigest:     "sha256:image",
			ExpectedToolchainManifestDigest: "sha256:toolchain",
		}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive rejection: %v", err)
	}
	if got := frame.GetRejection().GetKind(); got != guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH {
		t.Fatalf("rejection kind = %v", got)
	}
}

func TestProtocolServiceRejectsUnsupportedMandatoryFeature(t *testing.T) {
	stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
		HeartbeatInterval:       time.Second,
	}, t.TempDir())
	defer cleanup()

	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:                         protocolTestBinding(),
			SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			RequestedFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature(5)},
			MandatoryFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature(5)},
			ExpectedImageManifestDigest:     "sha256:image",
			ExpectedToolchainManifestDigest: "sha256:toolchain",
		}},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive rejection: %v", err)
	}
	if got := frame.GetRejection().GetKind(); got != guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_FEATURE_UNSUPPORTED {
		t.Fatalf("rejection kind = %v", got)
	}
}

func TestNewProtocolServiceRejectsIncompleteIdentityAndHeartbeat(t *testing.T) {
	_, err := NewProtocolService(Server{WorkspaceDir: t.TempDir()}, ProtocolIdentity{})
	if err == nil {
		t.Fatal("expected incomplete boot identity to fail")
	}
	_, err = NewProtocolService(Server{WorkspaceDir: t.TempDir()}, ProtocolIdentity{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       1,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
	})
	if err == nil {
		t.Fatal("expected missing heartbeat interval to fail")
	}
}

func TestProtocolServiceExecSeparatesShellAndArgvWithTypedTerminals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request *guestv1.ExecRequest
		want    string
	}{
		{
			name:    "shell",
			request: &guestv1.ExecRequest{Command: &guestv1.ExecRequest_Shell{Shell: "printf shell"}, OutputLimitBytes: 1024},
			want:    "shell",
		},
		{
			name: "argv",
			request: &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf argv"}}},
				OutputLimitBytes: 1024,
			},
			want: "argv",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
			defer cleanup()
			op := protocolTestOperationBinding(binding, "exec-"+tc.name, 1)
			if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
				Binding: op,
				Payload: &guestv1.ExecFrame_Request{Request: tc.request},
			}}}); err != nil {
				t.Fatalf("send exec request: %v", err)
			}
			if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
				t.Fatalf("admission = %#v", admission)
			}
			op.Sequence = 2
			if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
				Binding: op,
				Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 1024}},
			}}}); err != nil {
				t.Fatalf("send output credit: %v", err)
			}
			var output bytes.Buffer
			for {
				frame := receiveProtocolExec(t, stream)
				if chunk := frame.GetOutput(); chunk != nil {
					output.Write(chunk.Data)
				}
				if terminal := frame.GetTerminal(); terminal != nil {
					if terminal.Kind != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || terminal.ExitCode != 0 {
						t.Fatalf("terminal = %#v", terminal)
					}
					break
				}
			}
			if output.String() != tc.want {
				t.Fatalf("output = %q, want %q", output.String(), tc.want)
			}
		})
	}
}

func TestProtocolServicePTYRoutesCreditResizeInputAndTerminal(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t,
		t.TempDir(),
		[]guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
		},
	)
	defer cleanup()
	op := protocolTestOperationBinding(binding, "pty-routing", 1)
	sendProtocolExecRequest(t, stream, op, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "stty size; read ready; stty size"},
		OutputLimitBytes: 1024,
		Streaming:        true,
		Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
	})
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("PTY admission = %#v", admission)
	}
	sendPTY := func(sequence uint64, frame *guestv1.PtyFrame) {
		t.Helper()
		frame.Binding = protocolTestOperationBinding(binding, "pty-routing", sequence)
		if err := stream.Send(&guestv1.RunnerToGuest{
			Message: &guestv1.RunnerToGuest_Pty{Pty: frame},
		}); err != nil {
			t.Fatal(err)
		}
	}
	sendPTY(2, &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 1024}}})
	first, err := stream.Recv()
	if err != nil || first.GetPty().GetOutput() == nil ||
		!strings.Contains(strings.ReplaceAll(string(first.GetPty().GetOutput().Data), "\r", ""), "24 80") {
		t.Fatalf("initial PTY output = %#v terminal=%#v err=%v", first, first.GetExec().GetTerminal(), err)
	}
	sendPTY(3, &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Resize{Resize: &guestv1.PtyResize{Rows: 40, Columns: 120}}})
	sendPTY(4, &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Input{Input: &guestv1.PtyInput{Data: []byte("ready\n")}}})
	var output bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if chunk := frame.GetPty().GetOutput(); chunk != nil {
			output.Write(chunk.Data)
		}
		if terminal := frame.GetPty().GetTerminal(); terminal != nil {
			if terminal.Kind != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
				t.Fatalf("PTY terminal = %#v", terminal)
			}
			break
		}
	}
	if !strings.Contains(strings.ReplaceAll(output.String(), "\r", ""), "40 120") {
		t.Fatalf("resized PTY output = %q", output.String())
	}
}

func TestProtocolServicePTYCancelAndDeadlineReturnTypedTerminal(t *testing.T) {
	tests := map[string]struct {
		deadline func() uint64
		control  *guestv1.PtyFrame
		want     guestv1.ExecTerminalKind
	}{
		"cancel": {
			control: &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Cancel{
				Cancel: &guestv1.ExecCancel{Reason: "test cancellation"},
			}},
			want: guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
		},
		"deadline": {
			deadline: func() uint64 { return uint64(time.Now().Add(50 * time.Millisecond).UnixMilli()) },
			want:     guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED,
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
				t,
				t.TempDir(),
				[]guestv1.GuestFeature{
					guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
					guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
				},
			)
			defer cleanup()
			operation := protocolTestOperationBinding(binding, "pty-"+name, 1)
			var deadline uint64
			if testCase.deadline != nil {
				deadline = testCase.deadline()
			}
			sendProtocolExecRequest(t, stream, operation, &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 60"},
				DeadlineUnixMs:   deadline,
				OutputLimitBytes: 1024,
				Streaming:        true,
				Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
			})
			if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
				t.Fatalf("PTY admission = %#v", admission)
			}
			if testCase.control != nil {
				testCase.control.Binding = protocolTestOperationBinding(binding, "pty-"+name, 2)
				if err := stream.Send(&guestv1.RunnerToGuest{
					Message: &guestv1.RunnerToGuest_Pty{Pty: testCase.control},
				}); err != nil {
					t.Fatal(err)
				}
			}
			response, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			terminal := response.GetPty().GetTerminal()
			if terminal.GetKind() != testCase.want {
				t.Fatalf("PTY terminal = %#v, want %s", terminal, testCase.want)
			}
			if second := response.GetExec(); second != nil {
				t.Fatalf("PTY terminal was routed as Exec: %#v", second)
			}
		})
	}
}

func TestProtocolServicePTYRejectsExecFrameInput(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t,
		t.TempDir(),
		[]guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
		},
	)
	defer cleanup()
	operation := protocolTestOperationBinding(binding, "pty-exec-input", 1)
	sendProtocolExecRequest(t, stream, operation, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "cat"},
		OutputLimitBytes: 1024,
		Streaming:        true,
		Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
	})
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("PTY admission = %#v", admission)
	}
	operation.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
			Binding: operation,
			Payload: &guestv1.ExecFrame_Input{Input: &guestv1.ExecInput{
				Data: []byte("wrong frame kind"),
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	streamClosed := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				streamClosed <- err
				return
			}
		}
	}()
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("ExecFrame input for a PTY was accepted")
	}
}

func TestProtocolServicePTYRejectsInvalidInitialDimensionsAtAdmission(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t,
		t.TempDir(),
		[]guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
		},
	)
	defer cleanup()
	sendProtocolExecRequest(
		t,
		stream,
		protocolTestOperationBinding(binding, "pty-invalid-dimensions", 1),
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "true"},
			OutputLimitBytes: 1024,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 0, Columns: 80},
		},
	)
	admission := receiveProtocolExec(t, stream).GetAdmission()
	if admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_INVALID_REQUEST ||
		admission.GetSpawnFailureReason() != guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE {
		t.Fatalf("invalid PTY dimensions admission = %#v", admission)
	}
}

func TestProtocolServicePTYStreamDisconnectKillsGuestProcess(t *testing.T) {
	workspace := t.TempDir()
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t,
		workspace,
		[]guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
		},
	)
	sendProtocolExecRequest(
		t,
		stream,
		protocolTestOperationBinding(binding, "pty-disconnect", 1),
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "echo $$ > pty-disconnect.pid; exec sleep 60"},
			OutputLimitBytes: 1024,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
		},
	)
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		cleanup()
		t.Fatalf("PTY admission = %#v", admission)
	}
	pidPath := filepath.Join(workspace, "pty-disconnect.pid")
	var pid int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(value)))
			if err != nil {
				cleanup()
				t.Fatalf("parse PTY process ID: %v", err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		cleanup()
		t.Fatal("PTY process did not publish its process ID")
	}
	cleanup()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PTY process %d remained after guest protocol disconnect", pid)
}

func TestProtocolServiceExecRejectsInputAfterEOF(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
	defer cleanup()
	operation := protocolTestOperationBinding(binding, "exec-input-after-eof", 1)
	sendProtocolExecRequest(t, stream, operation, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "cat"},
		OutputLimitBytes: 1024,
		Streaming:        true,
	})
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("admission = %#v", admission)
	}
	operation.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: operation,
		Payload: &guestv1.ExecFrame_Input{Input: &guestv1.ExecInput{
			Data: []byte("complete"), EndOfInput: true,
		}},
	}}}); err != nil {
		t.Fatalf("send EOF: %v", err)
	}
	operation.Sequence = 3
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: operation,
		Payload: &guestv1.ExecFrame_Input{Input: &guestv1.ExecInput{
			Data: []byte("late"),
		}},
	}}}); err != nil {
		t.Fatalf("send input after EOF: %v", err)
	}
	streamClosed := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				streamClosed <- err
				return
			}
		}
	}()
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("input after EOF did not close the protocol stream")
	}
}

func TestProtocolServiceExecReportsSpawnDeadlineCancelAndOutputExhaustion(t *testing.T) {
	t.Run("spawn failed", func(t *testing.T) {
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
		defer cleanup()
		sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "spawn", 1), &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/missing/secondbox-command"}}},
			OutputLimitBytes: 1024,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED ||
			terminal.GetSpawnFailureReason() != guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND {
			t.Fatalf("terminal = %#v", terminal)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		workspace := t.TempDir()
		executable := filepath.Join(workspace, "not-executable")
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, workspace)
		defer cleanup()
		sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "permission", 1), &guestv1.ExecRequest{
			Command: &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{
				Argument: []string{executable},
			}},
			OutputLimitBytes: 1024,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED ||
			terminal.GetSpawnFailureReason() != guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED {
			t.Fatalf("terminal = %#v", terminal)
		}
	})

	t.Run("signal exit evidence", func(t *testing.T) {
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
		defer cleanup()
		sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "signal", 1), &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "kill -TERM $$"},
			OutputLimitBytes: 1024,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		terminal := receiveProtocolExec(t, stream).GetTerminal()
		if terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
			terminal.GetExitCode() != 143 ||
			terminal.GetSignal() != 15 {
			t.Fatalf("terminal = %#v", terminal)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
		defer cleanup()
		sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "deadline", 1), &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/sh", "-c", "sleep 5"}}},
			DeadlineUnixMs:   uint64(time.Now().Add(50 * time.Millisecond).UnixMilli()),
			OutputLimitBytes: 1024,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED {
			t.Fatalf("terminal = %#v", terminal)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
		defer cleanup()
		op := protocolTestOperationBinding(binding, "cancel", 1)
		sendProtocolExecRequest(t, stream, op, &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/sh", "-c", "sleep 5"}}},
			OutputLimitBytes: 1024,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		op.Sequence = 2
		if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
			Binding: op,
			Payload: &guestv1.ExecFrame_Cancel{Cancel: &guestv1.ExecCancel{Reason: "test cancellation"}},
		}}}); err != nil {
			t.Fatalf("send cancel: %v", err)
		}
		if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
			t.Fatalf("terminal = %#v", terminal)
		}
	})

	t.Run("output exhausted", func(t *testing.T) {
		stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
		defer cleanup()
		operation := protocolTestOperationBinding(binding, "output", 1)
		sendProtocolExecRequest(t, stream, operation, &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "printf 123456789"},
			OutputLimitBytes: 4,
		})
		_ = receiveProtocolExec(t, stream).GetAdmission()
		operation.Sequence = 2
		if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
			Binding: operation,
			Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 4}},
		}}}); err != nil {
			t.Fatalf("send exhausted output credit: %v", err)
		}
		if output := receiveProtocolExec(t, stream).GetOutput(); string(output.GetData()) != "1234" {
			t.Fatalf("bounded partial output = %#v", output)
		}
		if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
			t.Fatalf("terminal = %#v", terminal)
		}
	})
}

func TestProtocolServiceExecRejectsMalformedArgvCwdAndEnvironment(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "linked-cwd")); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name    string
		request *guestv1.ExecRequest
		reason  guestv1.SpawnFailureReason
	}{
		{
			name: "empty argv",
			request: &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{}},
				OutputLimitBytes: 1024,
			},
			reason: guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE,
		},
		{
			name: "symlink cwd",
			request: &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/true"}}},
				Cwd:              "linked-cwd",
				OutputLimitBytes: 1024,
			},
			reason: guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD,
		},
		{
			name: "invalid environment",
			request: &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/true"}}},
				Environment:      []*guestv1.EnvironmentEntry{{Name: "BAD=NAME", Value: []byte("value")}},
				OutputLimitBytes: 1024,
			},
			reason: guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stream, binding, cleanup := openNegotiatedProtocolTestStream(t, workspace)
			defer cleanup()
			sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "malformed-"+testCase.name, 1), testCase.request)
			if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_INVALID_REQUEST ||
				admission.GetSpawnFailureReason() != testCase.reason {
				t.Fatalf("admission = %#v", admission)
			}
		})
	}
}

func TestProtocolServiceExecCancellationKillsProcessGroupDescendant(t *testing.T) {
	workspace := t.TempDir()
	stream, binding, cleanup := openNegotiatedProtocolTestStream(t, workspace)
	defer cleanup()
	op := protocolTestOperationBinding(binding, "process-group", 1)
	sendProtocolExecRequest(t, stream, op, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 30 & echo $! > child.pid; wait"},
		OutputLimitBytes: 1024,
	})
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("admission = %#v", admission)
	}
	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(workspace, "child.pid"))
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("descendant PID was not written")
	}
	op.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: op,
		Payload: &guestv1.ExecFrame_Cancel{Cancel: &guestv1.ExecCancel{Reason: "test descendant cancellation"}},
	}}}); err != nil {
		t.Fatalf("send cancellation: %v", err)
	}
	if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("terminal = %#v", terminal)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		err := unix.Kill(childPID, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived process-group cancellation", childPID)
}

func TestProtocolServiceExecOutputWaitsForSlowClientCredit(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
	defer cleanup()
	op := protocolTestOperationBinding(binding, "slow-credit", 1)
	sendProtocolExecRequest(t, stream, op, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf 12345678"},
		OutputLimitBytes: 1024,
	})
	_ = receiveProtocolExec(t, stream).GetAdmission()
	received := make(chan *guestv1.ExecFrame, 1)
	go func() {
		frame, _ := stream.Recv()
		received <- frame.GetExec()
	}()
	select {
	case frame := <-received:
		t.Fatalf("received output without credit: %#v", frame)
	case <-time.After(75 * time.Millisecond):
	}
	op.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: op,
		Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 4}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if chunk := (<-received).GetOutput(); string(chunk.GetData()) != "1234" {
		t.Fatalf("first credited output = %#v", chunk)
	}
	op.Sequence = 3
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: op,
		Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 4}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if chunk := receiveProtocolExec(t, stream).GetOutput(); string(chunk.GetData()) != "5678" {
		t.Fatalf("second credited output = %#v", chunk)
	}
	if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestProtocolServiceDescriptorPinnedBinaryFileRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(t, workspace, []guestv1.GuestFeature{
		guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
		guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
	})
	defer cleanup()
	content := []byte{0, 1, 2, 0xff, 'x'}
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	writeBinding := protocolTestOperationBinding(binding, "file-write", 1)
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: writeBinding,
		Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
			Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
			WorkspaceRelativePath: "nested/data.bin",
			ExpectedSize:          uint64(len(content)),
			ExpectedChecksum:      checksum,
			CreateMode:            0o640,
		}},
	}}}); err != nil {
		t.Fatalf("send file write request: %v", err)
	}
	writeBinding.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: writeBinding,
		Payload: &guestv1.FileFrame_Chunk{Chunk: &guestv1.FileChunk{Offset: 0, Data: content}},
	}}}); err != nil {
		t.Fatalf("send file chunk: %v", err)
	}
	if terminal := receiveProtocolFile(t, stream).GetTerminal(); terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("write terminal = %#v", terminal)
	}

	readBinding := protocolTestOperationBinding(binding, "file-read", 1)
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: readBinding,
		Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
			Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
			WorkspaceRelativePath: "nested/data.bin",
		}},
	}}}); err != nil {
		t.Fatalf("send file read request: %v", err)
	}
	metadata := receiveProtocolFile(t, stream).GetMetadata()
	if metadata.GetSize() != uint64(len(content)) || metadata.GetChecksum() != checksum {
		t.Fatalf("metadata = %#v", metadata)
	}
	readBinding.Sequence = 2
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: readBinding,
		Payload: &guestv1.FileFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: uint64(len(content))}},
	}}}); err != nil {
		t.Fatalf("send exact file credit: %v", err)
	}
	var got bytes.Buffer
	for {
		frame := receiveProtocolFile(t, stream)
		if chunk := frame.GetChunk(); chunk != nil {
			got.Write(chunk.Data)
		}
		if terminal := frame.GetTerminal(); terminal != nil {
			if terminal.Kind != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
				t.Fatalf("read terminal = %#v", terminal)
			}
			break
		}
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("read = %v, want %v", got.Bytes(), content)
	}
	if info, err := os.Stat(filepath.Join(workspace, "nested", "data.bin")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("written mode: info=%v err=%v", info, err)
	}
}

func TestProtocolServiceFileRejectsSymlinkTraversal(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(t, workspace, []guestv1.GuestFeature{
		guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
	})
	defer cleanup()
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: protocolTestOperationBinding(binding, "file-symlink", 1),
		Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
			Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
			WorkspaceRelativePath: "escape/secret",
		}},
	}}}); err != nil {
		t.Fatalf("send file request: %v", err)
	}
	if terminal := receiveProtocolFile(t, stream).GetTerminal(); terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_SYMLINK_REJECTED {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestProtocolServiceFilesystemOperationsHaveExactTerminals(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "existing", "item.txt"), []byte("item"), 0o600); err != nil {
		t.Fatal(err)
	}
	stream, binding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(t, workspace, []guestv1.GuestFeature{
		guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
	})
	defer cleanup()

	stat := runProtocolMetadataOperation(t, stream, binding, "stat", guestv1.FileOperation_FILE_OPERATION_STAT, "existing/item.txt")
	if !stat.Exists || stat.Size != 4 {
		t.Fatalf("stat metadata = %#v", stat)
	}
	list := runProtocolMetadataOperation(t, stream, binding, "list", guestv1.FileOperation_FILE_OPERATION_LIST_DIRECT_CHILDREN, "existing")
	if len(list.DirectChildren) != 1 || list.DirectChildren[0] != "item.txt" {
		t.Fatalf("list metadata = %#v", list)
	}
	exists := runProtocolMetadataOperation(t, stream, binding, "exists", guestv1.FileOperation_FILE_OPERATION_EXISTS, "existing/item.txt")
	if !exists.Exists {
		t.Fatalf("exists metadata = %#v", exists)
	}
	runProtocolTerminalOperation(t, stream, binding, "mkdir", guestv1.FileOperation_FILE_OPERATION_MKDIR, "created/nested", true, false)
	if info, err := os.Stat(filepath.Join(workspace, "created", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("mkdir result: info=%v err=%v", info, err)
	}
	runProtocolTerminalOperation(t, stream, binding, "remove", guestv1.FileOperation_FILE_OPERATION_REMOVE, "created", true, false)
	if _, err := os.Stat(filepath.Join(workspace, "created")); !os.IsNotExist(err) {
		t.Fatalf("remove result err = %v", err)
	}
	missing := runProtocolMetadataOperation(t, stream, binding, "missing", guestv1.FileOperation_FILE_OPERATION_EXISTS, "created")
	if missing.Exists {
		t.Fatalf("missing exists metadata = %#v", missing)
	}
}

func protocolTestBinding() *guestv1.ConnectionBinding {
	return &guestv1.ConnectionBinding{
		InstanceId:        "instance-1",
		SandboxId:         "sandbox-1",
		SandboxGeneration: 7,
		ConnectionNonce:   []byte("0123456789abcdef0123456789abcdef"),
	}
}

func openNegotiatedProtocolTestStream(t *testing.T, workspace string) (guestv1.GuestAgent_ConnectClient, *guestv1.ConnectionBinding, func()) {
	return openNegotiatedProtocolTestStreamWithFeatures(t, workspace, []guestv1.GuestFeature{
		guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
	})
}

func openNegotiatedProtocolTestStreamWithFeatures(
	t *testing.T,
	workspace string,
	features []guestv1.GuestFeature,
) (guestv1.GuestAgent_ConnectClient, *guestv1.ConnectionBinding, func()) {
	t.Helper()
	stream, cleanup := openProtocolTestStream(t, ProtocolIdentity{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
		HeartbeatInterval:       time.Second,
	}, workspace)
	binding := protocolTestBinding()
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
		Binding:                         binding,
		SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
		RequestedFeatures:               append([]guestv1.GuestFeature(nil), features...),
		MandatoryFeatures:               append([]guestv1.GuestFeature(nil), features...),
		ExpectedImageManifestDigest:     "sha256:image",
		ExpectedToolchainManifestDigest: "sha256:toolchain",
	}}}); err != nil {
		cleanup()
		t.Fatalf("send hello: %v", err)
	}
	if welcome, err := stream.Recv(); err != nil || welcome.GetWelcome() == nil {
		cleanup()
		t.Fatalf("receive welcome: frame=%#v err=%v", welcome, err)
	}
	return stream, binding, cleanup
}

func protocolTestOperationBinding(connection *guestv1.ConnectionBinding, operationID string, sequence uint64) *guestv1.OperationBinding {
	return &guestv1.OperationBinding{
		Connection:   cloneConnectionBinding(connection),
		AssignmentId: "assignment-1",
		OperationId:  operationID,
		StreamId:     operationID + "-stream",
		Sequence:     sequence,
	}
}

func sendProtocolExecRequest(t *testing.T, stream guestv1.GuestAgent_ConnectClient, binding *guestv1.OperationBinding, request *guestv1.ExecRequest) {
	t.Helper()
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: binding,
		Payload: &guestv1.ExecFrame_Request{Request: request},
	}}}); err != nil {
		t.Fatalf("send exec request: %v", err)
	}
}

func receiveProtocolExec(t *testing.T, stream guestv1.GuestAgent_ConnectClient) *guestv1.ExecFrame {
	t.Helper()
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive exec frame: %v", err)
	}
	if frame.GetExec() == nil {
		t.Fatalf("frame = %#v, want exec", frame)
	}
	return frame.GetExec()
}

func receiveProtocolFile(t *testing.T, stream guestv1.GuestAgent_ConnectClient) *guestv1.FileFrame {
	t.Helper()
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive file frame: %v", err)
	}
	if frame.GetFile() == nil {
		t.Fatalf("frame = %#v, want file", frame)
	}
	return frame.GetFile()
}

func runProtocolMetadataOperation(
	t *testing.T,
	stream guestv1.GuestAgent_ConnectClient,
	connection *guestv1.ConnectionBinding,
	operationID string,
	operation guestv1.FileOperation,
	path string,
) *guestv1.FileMetadata {
	t.Helper()
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: protocolTestOperationBinding(connection, operationID, 1),
		Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
			Operation:             operation,
			WorkspaceRelativePath: path,
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
	metadata := receiveProtocolFile(t, stream).GetMetadata()
	if metadata == nil {
		t.Fatal("metadata response is missing")
	}
	if terminal := receiveProtocolFile(t, stream).GetTerminal(); terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("terminal = %#v", terminal)
	}
	return metadata
}

func runProtocolTerminalOperation(
	t *testing.T,
	stream guestv1.GuestAgent_ConnectClient,
	connection *guestv1.ConnectionBinding,
	operationID string,
	operation guestv1.FileOperation,
	path string,
	recursive bool,
	force bool,
) {
	t.Helper()
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
		Binding: protocolTestOperationBinding(connection, operationID, 1),
		Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
			Operation:             operation,
			WorkspaceRelativePath: path,
			Recursive:             recursive,
			Force:                 force,
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if terminal := receiveProtocolFile(t, stream).GetTerminal(); terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func openProtocolTestStream(
	t *testing.T,
	identity ProtocolIdentity,
	workspace string,
) (guestv1.GuestAgent_ConnectClient, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	service, err := NewProtocolService(Server{
		WorkspaceDir:      workspace,
		RuntimePrivateDir: t.TempDir(),
		InstanceID:        identity.InstanceID,
		SandboxID:         identity.SandboxID,
	}, identity)
	if err != nil {
		t.Fatalf("create guest protocol service: %v", err)
	}
	guestv1.RegisterGuestAgentServer(server, service)
	go func() {
		_ = server.Serve(listener)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	connection, err := grpc.NewClient(
		"passthrough:///guest-protocol",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		server.Stop()
		t.Fatalf("dial guest protocol: %v", err)
	}
	stream, err := guestv1.NewGuestAgentClient(connection).Connect(ctx)
	if err != nil {
		connection.Close()
		cancel()
		server.Stop()
		t.Fatalf("connect guest protocol: %v", err)
	}
	return stream, func() {
		connection.Close()
		cancel()
		server.Stop()
		listener.Close()
	}
}

func protocolFeatureEnabled(features []guestv1.GuestFeature, want guestv1.GuestFeature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func protocolTestFeature(t *testing.T, name string) guestv1.GuestFeature {
	t.Helper()
	switch name {
	case "streaming_exec":
		return guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC
	case "descriptor_pinned_filesystem":
		return guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM
	case "activity_events":
		return guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS
	case "port_proxy":
		return guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY
	case "unsupported_reserved_5":
		return guestv1.GuestFeature(5)
	default:
		t.Fatalf("unknown fixture feature %q", name)
		return guestv1.GuestFeature_GUEST_FEATURE_UNSPECIFIED
	}
}
