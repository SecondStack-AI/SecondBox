package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
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
	sends   int
}

func (stream *captureExecutionGatewayStream) Send(frame *guestv1.RunnerToGuest) error {
	stream.sends++
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

var testExecutionListener = netip.MustParseAddrPort("169.254.104.1:41000")

// attributedListenerGateways is what both backends hand an attributed
// generation: the projection of the forwarder-compiled listener policy, built
// from compile options that do map ordinary logical gateways.
func attributedListenerGateways(t *testing.T) []networkpolicy.LogicalGatewayEndpoint {
	t.Helper()
	listener, err := networkpolicy.CompileExecutionListener(testExecutionListener, networkpolicy.CompileOptions{
		MaximumPins: 64, MaximumTTL: time.Minute,
		ManagementPrefixes: []netip.Prefix{netip.MustParsePrefix("10.210.2.0/24")},
		RunnerGateways:     map[string]netip.Addr{"agent-gateway.secondbox.internal": netip.MustParseAddr("10.210.2.10")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return listener.LogicalGatewayEndpoints()
}

func TestReservedGuestEnvironmentInjectedAtGuestDispatch(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributed bool
		gateways   func(*testing.T) []networkpolicy.LogicalGatewayEndpoint
		want       []*guestv1.EnvironmentEntry
	}{
		{name: "ordinary-without-gateways", gateways: func(*testing.T) []networkpolicy.LogicalGatewayEndpoint { return nil }},
		{
			name:     "ordinary-with-gateways",
			gateways: func(*testing.T) []networkpolicy.LogicalGatewayEndpoint { return testRunnerGateways },
			want:     []*guestv1.EnvironmentEntry{{Name: runnerGatewaysEnvironment, Value: []byte(testRunnerGatewaysValue)}},
		},
		{
			name: "attributed", attributed: true, gateways: attributedListenerGateways,
			want: []*guestv1.EnvironmentEntry{{Name: executionGatewayEnvironment, Value: []byte(testExecutionListener.String())}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			expiry := time.Now().Add(time.Minute)
			stream := &captureExecutionGatewayStream{}
			session := &GuestProtocolSession{
				Stream: stream, Binding: &guestv1.ConnectionBinding{},
				EnabledFeatures: map[guestv1.GuestFeature]bool{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC: true},
				runnerGateways:  test.gateways(t),
			}
			if test.attributed {
				guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
				if err != nil {
					t.Fatal(err)
				}
				session.attributedExecution = guard
				session.executionGateway = testExecutionListener
			}
			request := &guestv1.ExecRequest{DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024, Environment: []*guestv1.EnvironmentEntry{{Name: "HTTP_PROXY", Value: []byte("application-owned")}}}
			for _, reserved := range []string{
				"SECONDBOX_EXECUTION_GATEWAY", " SECONDBOX_EXECUTION_GATEWAY ",
				"SECONDBOX_RUNNER_GATEWAYS", " SECONDBOX_RUNNER_GATEWAYS ",
			} {
				collision := proto.CloneOf(request)
				collision.Environment = append(collision.Environment, &guestv1.EnvironmentEntry{Name: reserved, Value: []byte("forged:123")})
				if _, err := session.ExecuteBuffered(t.Context(), "assignment", collision); err == nil || !strings.Contains(err.Error(), "reserved") || stream.sends != 0 {
					t.Fatalf("caller value for %s was not refused before dispatch: %v", reserved, err)
				}
			}
			if _, err := session.ExecuteBuffered(t.Context(), "assignment", request); err == nil || !strings.Contains(err.Error(), "captured exec send") {
				t.Fatalf("dispatch: %v", err)
			}
			if len(request.Environment) != 1 || string(stream.request.Environment[0].Value) != "application-owned" {
				t.Fatal("caller request or proxy configuration was modified")
			}
			injected := stream.request.Environment[1:]
			if len(injected) != len(test.want) {
				t.Fatalf("injected guest environment = %+v", injected)
			}
			for index, entry := range test.want {
				if injected[index].Name != entry.Name || string(injected[index].Value) != string(entry.Value) {
					t.Fatalf("injected guest environment %d = %+v, want %+v", index, injected[index], entry)
				}
			}
		})
	}
}

func TestReservedGuestEnvironmentRefusedOnPTY(t *testing.T) {
	for _, reserved := range []string{
		"SECONDBOX_RUNNER_GATEWAYS", " SECONDBOX_RUNNER_GATEWAYS ",
		"SECONDBOX_EXECUTION_GATEWAY", " SECONDBOX_EXECUTION_GATEWAY ",
	} {
		stream := &captureExecutionGatewayStream{}
		session := &GuestProtocolSession{
			Stream: stream, Binding: &guestv1.ConnectionBinding{},
			EnabledFeatures: map[guestv1.GuestFeature]bool{
				guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC: true,
				guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE:     true,
			},
			runnerGateways: testRunnerGateways,
		}
		request := &guestv1.ExecRequest{
			Pty: &guestv1.PtyDimensions{Rows: 24, Columns: 80}, Streaming: true, OutputLimitBytes: 1024,
			DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
			Environment:    []*guestv1.EnvironmentEntry{{Name: reserved, Value: []byte("forged=10.0.0.1:443")}},
		}
		_, err := session.ExecutePTY(t.Context(), "assignment", request, nil, func([]byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "reserved") || stream.sends != 0 {
			t.Fatalf("PTY value for %q was not refused before dispatch: %v", reserved, err)
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

func TestRunnerGatewaysRequireCanonicalNameAndValidEndpoint(t *testing.T) {
	for _, gateway := range []networkpolicy.LogicalGatewayEndpoint{
		{LogicalName: "", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: " agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "Agent-Gateway.SecondBox.Internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "agent_gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:443")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.AddrPort{}},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("10.210.2.10:0")},
		{LogicalName: "agent-gateway.secondbox.internal", Endpoint: netip.MustParseAddrPort("[::ffff:10.210.2.10]:443")},
	} {
		request := GuestProtocolNegotiation{RunnerGateways: []networkpolicy.LogicalGatewayEndpoint{gateway}}
		if _, err := NegotiateGuestProtocol(context.Background(), request); err == nil || !strings.Contains(err.Error(), "Runner gateway") {
			t.Fatalf("invalid Runner gateway %+v accepted: %v", gateway, err)
		}
	}
}

// Every address the egress-context loader and the policy compiler admit must
// start and be published; loopback, unspecified, multicast and IPv6 mappings
// started before guest publication existed.
func TestRunnerGatewaysPublishEveryLoaderAdmittedMapping(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "egress-contexts.json")
	document := `{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"tenant-a","gateways":[` +
		`{"logicalName":"agent-gateway.secondbox.internal","address":"10.210.2.10"},` +
		`{"logicalName":"loopback-gateway.secondbox.internal","address":"127.0.0.1"},` +
		`{"logicalName":"unspecified-gateway.secondbox.internal","address":"0.0.0.0"},` +
		`{"logicalName":"multicast-gateway.secondbox.internal","address":"224.0.0.1"},` +
		`{"logicalName":"ipv6-gateway.secondbox.internal","address":"fd00:2026:9::10"}]}]}`
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	contexts, err := networkpolicy.LoadEgressContextConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	options, err := contexts.CompileOptionsForContext("tenant-a", networkpolicy.CompileOptions{MaximumPins: 64, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var destinations []networkpolicy.Destination
	for _, name := range []string{"agent", "loopback", "unspecified", "multicast", "ipv6"} {
		destinations = append(destinations, networkpolicy.Destination{Protocol: networkpolicy.ProtocolHTTPS, Domain: name + "-gateway.secondbox.internal", Port: 443})
	}
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{Mode: networkpolicy.ModeAllowList, Destinations: destinations}, options)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := compiled.LogicalGatewayEndpoints()
	want := "agent-gateway.secondbox.internal=10.210.2.10:443" +
		" ipv6-gateway.secondbox.internal=[fd00:2026:9::10]:443" +
		" loopback-gateway.secondbox.internal=127.0.0.1:443" +
		" multicast-gateway.secondbox.internal=224.0.0.1:443" +
		" unspecified-gateway.secondbox.internal=0.0.0.0:443"

	socket, _, _ := startDirectUnixSocketGuest(t)
	expiry := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), expiry)
	defer cancel()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath: socket, DirectUnixSocket: true, RunnerGateways: endpoints,
		InstanceID: "instance-1", SandboxID: "sandbox-1", SandboxGeneration: 7,
		ExpectedGuestBuildID: "guest-build-1", ExpectedImageManifestDigest: "sha256:image", ExpectedToolchainManifestDigest: "sha256:toolchain",
		MandatoryFeatures: []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC},
	})
	if err != nil {
		t.Fatalf("loader-admitted gateways refused at negotiation: %v", err)
	}
	defer session.Close()
	result, err := session.ExecuteBuffered(ctx, "assignment", &guestv1.ExecRequest{
		Command:        &guestv1.ExecRequest_Shell{Shell: `sh -c 'printf "%s" "${SECONDBOX_RUNNER_GATEWAYS-absent}"'`},
		DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
	})
	if err != nil || result.Terminal.GetExitCode() != 0 || string(result.Stdout) != want {
		t.Fatalf("published gateways: err=%v terminal=%+v stdout=%q, want %q", err, result.Terminal, result.Stdout, want)
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
				negotiation.ExecutionGateway = testExecutionListener
				negotiation.RunnerGateways = attributedListenerGateways(t)
				want = testExecutionListener.String() + "|absent"
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
