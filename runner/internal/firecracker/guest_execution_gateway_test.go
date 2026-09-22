package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
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

// testRunnerGateways is the sorted projection two released logical gateways
// produce when one egress context maps both to the same Runner-host address.
var testRunnerGateways = []networkpolicy.LogicalGatewayEndpoint{
	{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
	{LogicalName: "platform-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
}

const testRunnerGatewaysValue = "agent-gateway.secondbox.internal=10.210.2.10:443 platform-gateway.secondbox.internal=10.210.2.10:443"

func TestReservedGuestEnvironmentInjectedAtGuestDispatch(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		for _, withGateways := range []bool{false, true} {
			name := map[bool]string{false: "ordinary", true: "attributed"}[attributed] +
				map[bool]string{false: "-without-gateways", true: "-with-gateways"}[withGateways]
			t.Run(name, func(t *testing.T) {
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
				if withGateways {
					session.runnerGateways = testRunnerGateways
				}
				request := &guestv1.ExecRequest{DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024, Environment: []*guestv1.EnvironmentEntry{{Name: "HTTP_PROXY", Value: []byte("application-owned")}}}
				for _, reserved := range []string{
					"SECONDBOX_EXECUTION_GATEWAY", " SECONDBOX_EXECUTION_GATEWAY ",
					"SECONDBOX_RUNNER_GATEWAYS", " SECONDBOX_RUNNER_GATEWAYS ",
				} {
					collision := proto.CloneOf(request)
					collision.Environment = append(collision.Environment, &guestv1.EnvironmentEntry{Name: reserved, Value: []byte("forged:123")})
					if _, err := session.ExecuteBuffered(t.Context(), "assignment", collision); err == nil || !strings.Contains(err.Error(), "reserved") || stream.request != nil {
						t.Fatalf("caller value for %s was not refused before dispatch: %v", reserved, err)
					}
				}
				if _, err := session.ExecuteBuffered(t.Context(), "assignment", request); err == nil || !strings.Contains(err.Error(), "captured exec send") {
					t.Fatalf("dispatch: %v", err)
				}
				if len(request.Environment) != 1 || string(stream.request.Environment[0].Value) != "application-owned" {
					t.Fatal("caller request or proxy configuration was modified")
				}
				var want []*guestv1.EnvironmentEntry
				if attributed {
					want = append(want, &guestv1.EnvironmentEntry{Name: executionGatewayEnvironment, Value: []byte("169.254.104.1:41000")})
				}
				if withGateways {
					want = append(want, &guestv1.EnvironmentEntry{Name: runnerGatewaysEnvironment, Value: []byte(testRunnerGatewaysValue)})
				}
				injected := stream.request.Environment[1:]
				if len(injected) != len(want) {
					t.Fatalf("injected guest environment = %+v", injected)
				}
				for index, entry := range want {
					if injected[index].Name != entry.Name || string(injected[index].Value) != string(entry.Value) {
						t.Fatalf("injected guest environment %d = %+v, want %+v", index, injected[index], entry)
					}
				}
			})
		}
	}
}

func TestRunnerGatewaysPublishCompiledPolicyProjection(t *testing.T) {
	gatewayAddress := netip.MustParseAddr("10.210.2.10")
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{
			{Protocol: networkpolicy.ProtocolHTTPS, Domain: "platform-gateway.secondbox.internal", Port: 443},
			{Protocol: networkpolicy.ProtocolHTTPS, Domain: "agent-gateway.secondbox.internal", Port: 443},
			{Protocol: networkpolicy.ProtocolHTTPS, Domain: "example.com", Port: 443},
		},
	}, networkpolicy.CompileOptions{
		MaximumPins:        64,
		MaximumTTL:         time.Minute,
		ManagementPrefixes: []netip.Prefix{netip.MustParsePrefix("10.210.2.0/24")},
		RunnerGateways: map[string]netip.Addr{
			"agent-gateway.secondbox.internal":    gatewayAddress,
			"platform-gateway.secondbox.internal": gatewayAddress,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := compiled.LogicalGatewayEndpoints()
	if err := validateRunnerGateways(endpoints); err != nil {
		t.Fatal(err)
	}
	if got := formatRunnerGateways(endpoints); got != testRunnerGatewaysValue {
		t.Fatalf("SECONDBOX_RUNNER_GATEWAYS = %q, want %q", got, testRunnerGatewaysValue)
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

func TestRunnerGatewaysRequireCanonicalNameAndUnicastEndpoint(t *testing.T) {
	for _, gateway := range []networkpolicy.LogicalGatewayEndpoint{
		{LogicalName: "", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: " agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "Agent-Gateway.SecondBox.Internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "agent_gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.AddrPort{}},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:0")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("127.0.0.1:443")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("[2001:db8::1]:443")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("224.0.0.1:443")},
	} {
		request := GuestProtocolNegotiation{RunnerGateways: []networkpolicy.LogicalGatewayEndpoint{gateway}}
		if _, err := NegotiateGuestProtocol(context.Background(), request); err == nil || !strings.Contains(err.Error(), "Runner gateway") {
			t.Fatalf("invalid Runner gateway %+v accepted: %v", gateway, err)
		}
	}
}

func TestReservedGuestEnvironmentReachesRealGuestCommand(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "attributed"}[attributed], func(t *testing.T) {
			socket, _, _ := startDirectUnixSocketGuest(t)
			expiry := time.Now().Add(10 * time.Second)
			negotiation := GuestProtocolNegotiation{
				UDSPath: socket, DirectUnixSocket: true,
				RunnerGateways: testRunnerGateways,
				InstanceID:     "instance-1", SandboxID: "sandbox-1", SandboxGeneration: 7,
				ExpectedGuestBuildID: "guest-build-1", ExpectedImageManifestDigest: "sha256:image", ExpectedToolchainManifestDigest: "sha256:toolchain",
				MandatoryFeatures: []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC},
			}
			want := "absent|" + testRunnerGatewaysValue
			if attributed {
				guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
				if err != nil {
					t.Fatal(err)
				}
				negotiation.AttributedExecution = guard
				negotiation.ExecutionGateway = netip.MustParseAddrPort("169.254.104.1:41000")
				want = negotiation.ExecutionGateway.String() + "|" + testRunnerGatewaysValue
			}
			ctx, cancel := context.WithDeadline(t.Context(), expiry)
			defer cancel()
			session, err := NegotiateGuestProtocol(ctx, negotiation)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.ExecuteBuffered(ctx, "assignment", &guestv1.ExecRequest{
				Command:        &guestv1.ExecRequest_Shell{Shell: `sh -c 'printf "%s|%s" "${SECONDBOX_EXECUTION_GATEWAY-absent}" "${SECONDBOX_RUNNER_GATEWAYS-absent}"'`},
				DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
			})
			if err != nil || result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || result.Terminal.GetExitCode() != 0 || string(result.Stdout) != want {
				t.Fatalf("reserved guest environment: err=%v terminal=%+v stdout=%q stderr=%q", err, result.Terminal, result.Stdout, result.Stderr)
			}
		})
	}
}
