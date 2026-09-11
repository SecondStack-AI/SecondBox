package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"google.golang.org/protobuf/proto"
)

type captureExecutionGatewayStream struct {
	guestv1.GuestAgent_ConnectClient
	request *guestv1.ExecRequest
}

func (stream *captureExecutionGatewayStream) Send(frame *guestv1.RunnerToGuest) error {
	stream.request = proto.CloneOf(frame.GetExec().GetRequest())
	return errors.New("captured exec send")
}

func TestExecutionGatewayInjectedAtGuestDispatch(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "attributed"}[attributed], func(t *testing.T) {
			expiry := time.Now().Add(time.Minute)
			stream := &captureExecutionGatewayStream{}
			session := &GuestProtocolSession{Stream: stream, Binding: &guestv1.ConnectionBinding{}, EnabledFeatures: map[guestv1.GuestFeature]bool{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC: true}}
			if attributed {
				guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
				if err != nil {
					t.Fatal(err)
				}
				session.attributedExecution = guard
				session.executionGateway = netip.MustParseAddrPort("169.254.104.1:41000")
			}
			request := &guestv1.ExecRequest{DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024, Environment: []*guestv1.EnvironmentEntry{{Name: "HTTP_PROXY", Value: []byte("application-owned")}}}
			for _, name := range []string{"SECONDBOX_EXECUTION_GATEWAY", " SECONDBOX_EXECUTION_GATEWAY "} {
				collision := proto.CloneOf(request)
				collision.Environment = append(collision.Environment, &guestv1.EnvironmentEntry{Name: name, Value: []byte("forged:123")})
				if _, err := session.ExecuteBuffered(t.Context(), "assignment", collision); err == nil || !strings.Contains(err.Error(), "reserved") || stream.request != nil {
					t.Fatalf("caller gateway was not refused before dispatch: %v", err)
				}
			}
			if _, err := session.ExecuteBuffered(t.Context(), "assignment", request); err == nil || !strings.Contains(err.Error(), "captured exec send") {
				t.Fatalf("dispatch: %v", err)
			}
			if len(request.Environment) != 1 || string(stream.request.Environment[0].Value) != "application-owned" {
				t.Fatal("caller request or proxy configuration was modified")
			}
			if attributed {
				if len(stream.request.Environment) != 2 || stream.request.Environment[1].Name != executionGatewayEnvironment || string(stream.request.Environment[1].Value) != "169.254.104.1:41000" {
					t.Fatalf("wrong guest gateway environment: %+v", stream.request.Environment)
				}
			} else if len(stream.request.Environment) != 1 {
				t.Fatal("ordinary exec received gateway environment")
			}
		})
	}
}

func TestExecutionGatewayRequiresMatchingHostMode(t *testing.T) {
	for _, test := range []struct {
		attributed bool
		endpoint   netip.AddrPort
	}{
		{true, netip.AddrPort{}}, {true, netip.MustParseAddrPort("127.0.0.1:123")},
		{true, netip.MustParseAddrPort("[::1]:123")}, {true, netip.MustParseAddrPort("169.254.104.1:0")},
		{false, netip.MustParseAddrPort("169.254.104.1:41000")},
	} {
		request := GuestProtocolNegotiation{ExecutionGateway: test.endpoint}
		if test.attributed {
			request.AttributedExecution = &runtimemanager.AttributedExecutionGuard{}
		}
		if _, err := NegotiateGuestProtocol(context.Background(), request); err == nil || !strings.Contains(err.Error(), "execution") {
			t.Fatalf("invalid execution gateway accepted: %v", err)
		}
	}
}

func TestExecutionGatewayReachesRealGuestCommand(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "attributed"}[attributed], func(t *testing.T) {
			socket, _, _ := startDirectUnixSocketGuest(t)
			expiry := time.Now().Add(10 * time.Second)
			negotiation := GuestProtocolNegotiation{
				UDSPath: socket, DirectUnixSocket: true,
				InstanceID: "instance-1", SandboxID: "sandbox-1", SandboxGeneration: 7,
				ExpectedGuestBuildID: "guest-build-1", ExpectedImageManifestDigest: "sha256:image", ExpectedToolchainManifestDigest: "sha256:toolchain",
				MandatoryFeatures: []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC},
			}
			want := "absent"
			if attributed {
				guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
				if err != nil {
					t.Fatal(err)
				}
				negotiation.AttributedExecution = guard
				negotiation.ExecutionGateway = netip.MustParseAddrPort("169.254.104.1:41000")
				want = negotiation.ExecutionGateway.String()
			}
			ctx, cancel := context.WithDeadline(t.Context(), expiry)
			defer cancel()
			session, err := NegotiateGuestProtocol(ctx, negotiation)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.ExecuteBuffered(ctx, "assignment", &guestv1.ExecRequest{
				Command:        &guestv1.ExecRequest_Shell{Shell: `sh -c 'printf "%s" "${SECONDBOX_EXECUTION_GATEWAY-absent}"'`},
				DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
			})
			if err != nil || result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || result.Terminal.GetExitCode() != 0 || string(result.Stdout) != want {
				t.Fatalf("guest execution gateway: err=%v terminal=%+v stdout=%q stderr=%q", err, result.Terminal, result.Stdout, result.Stderr)
			}
		})
	}
}
