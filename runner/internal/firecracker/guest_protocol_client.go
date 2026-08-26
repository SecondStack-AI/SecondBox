package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	currentGuestProtocolGeneration uint32 = 1
	guestConnectionNonceByteCount         = 32
	maxGuestHeartbeatInterval             = 60 * time.Second
	// guestUnixSocketPathBound keeps direct socket paths inside
	// sockaddr_un's sun_path limit.
	guestUnixSocketPathBound = 107
)

type GuestProtocolNegotiation struct {
	UDSPath string
	Port    uint32
	// DirectUnixSocket dials UDSPath as a plain filesystem Unix socket with
	// no Firecracker vsock CONNECT framing and no port. This is the gVisor
	// transport, where the agent listens on a gofer-passed host socket.
	DirectUnixSocket                bool
	InstanceID                      string
	SandboxID                       string
	SandboxGeneration               uint64
	ExpectedGuestBuildID            string
	ExpectedImageManifestDigest     string
	ExpectedToolchainManifestDigest string
	RequestedFeatures               []guestv1.GuestFeature
	MandatoryFeatures               []guestv1.GuestFeature
}

// GuestProtocolSession is one negotiated, assignment-bound guest connection.
type GuestProtocolSession struct {
	Connection              *grpc.ClientConn
	Stream                  guestv1.GuestAgent_ConnectClient
	Binding                 *guestv1.ConnectionBinding
	Generation              uint32
	EnabledFeatures         map[guestv1.GuestFeature]bool
	GuestBuildID            string
	ImageManifestDigest     string
	ToolchainManifestDigest string
	cancel                  context.CancelFunc
	operationMu             sync.Mutex
	sendMu                  sync.Mutex
}

// guestProtocolConnectParams retries the guest vsock dial on a millisecond
// cadence instead of the gRPC default.
//
// The guest agent is never listening when the first dial is attempted, because
// the microVM has only just been launched. gRPC's default reconnect backoff
// starts at one second and multiplies by 1.6, so negotiation completed at the
// backoff boundary rather than when the guest became ready: measured startup
// spent 1,014 ms waiting for the first retry and 2,567 ms when two retries were
// needed. Retrying quickly makes negotiation track actual guest readiness.
func guestProtocolConnectParams() grpc.ConnectParams {
	return grpc.ConnectParams{
		Backoff: backoff.Config{
			BaseDelay:  10 * time.Millisecond,
			Multiplier: 1.5,
			Jitter:     0.2,
			MaxDelay:   250 * time.Millisecond,
		},
		MinConnectTimeout: 20 * time.Second,
	}
}

func NegotiateGuestProtocol(ctx context.Context, request GuestProtocolNegotiation) (*GuestProtocolSession, error) {
	if strings.TrimSpace(request.UDSPath) == "" {
		return nil, fmt.Errorf("guest protocol UDS path is required")
	}
	if request.DirectUnixSocket {
		if request.Port != 0 {
			return nil, fmt.Errorf("guest protocol direct Unix socket takes no port")
		}
		if len(request.UDSPath) > guestUnixSocketPathBound {
			return nil, fmt.Errorf("guest protocol Unix socket path exceeds %d bytes", guestUnixSocketPathBound)
		}
	} else if request.Port == 0 {
		return nil, fmt.Errorf("guest protocol vsock port is required")
	}
	if strings.TrimSpace(request.InstanceID) == "" ||
		strings.TrimSpace(request.SandboxID) == "" ||
		request.SandboxGeneration == 0 ||
		strings.TrimSpace(request.ExpectedGuestBuildID) == "" ||
		strings.TrimSpace(request.ExpectedImageManifestDigest) == "" ||
		strings.TrimSpace(request.ExpectedToolchainManifestDigest) == "" {
		return nil, fmt.Errorf("guest protocol assignment binding is incomplete")
	}
	nonce := make([]byte, guestConnectionNonceByteCount)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("create guest protocol connection nonce: %w", err)
	}
	binding := &guestv1.ConnectionBinding{
		InstanceId:        request.InstanceID,
		SandboxId:         request.SandboxID,
		SandboxGeneration: request.SandboxGeneration,
		ConnectionNonce:   nonce,
	}
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	stopCallerCancellation := context.AfterFunc(ctx, sessionCancel)
	connection, err := grpc.NewClient(
		"passthrough:///secondbox-guest",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			if request.DirectUnixSocket {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", request.UDSPath)
			}
			return dialFirecrackerVsock(ctx, request.UDSPath, request.Port)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(guestProtocolConnectParams()),
	)
	if err != nil {
		sessionCancel()
		return nil, fmt.Errorf("create guest protocol client: %w", err)
	}
	closeWithError := func(err error) (*GuestProtocolSession, error) {
		sessionCancel()
		return nil, errors.Join(err, connection.Close())
	}
	stream, err := guestv1.NewGuestAgentClient(connection).Connect(
		sessionCtx,
		grpc.WaitForReady(true),
	)
	if err != nil {
		return closeWithError(fmt.Errorf("open guest protocol stream: %w", err))
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:                         binding,
			SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: currentGuestProtocolGeneration, Maximum: currentGuestProtocolGeneration},
			RequestedFeatures:               append([]guestv1.GuestFeature(nil), request.RequestedFeatures...),
			MandatoryFeatures:               append([]guestv1.GuestFeature(nil), request.MandatoryFeatures...),
			ExpectedImageManifestDigest:     request.ExpectedImageManifestDigest,
			ExpectedToolchainManifestDigest: request.ExpectedToolchainManifestDigest,
		}},
	}); err != nil {
		return closeWithError(fmt.Errorf("send guest protocol hello: %w", err))
	}
	first, err := stream.Recv()
	if err != nil {
		return closeWithError(fmt.Errorf("receive guest protocol welcome: %w", err))
	}
	if rejection := first.GetRejection(); rejection != nil {
		return closeWithError(fmt.Errorf("guest protocol negotiation rejected (%s): %s", rejection.Kind.String(), rejection.SafeDetail))
	}
	welcome := first.GetWelcome()
	if welcome == nil {
		return closeWithError(fmt.Errorf("guest protocol first response was not welcome"))
	}
	if welcome.SelectedGeneration != currentGuestProtocolGeneration {
		return closeWithError(fmt.Errorf("guest selected unsupported protocol generation %d", welcome.SelectedGeneration))
	}
	if welcome.GuestBuildId != request.ExpectedGuestBuildID {
		return closeWithError(fmt.Errorf("guest welcome build ID mismatch"))
	}
	if !sameConnectionBinding(welcome.Binding, binding) {
		return closeWithError(fmt.Errorf("guest welcome binding mismatch"))
	}
	if welcome.ImageManifestDigest != request.ExpectedImageManifestDigest {
		return closeWithError(fmt.Errorf("guest welcome image manifest digest mismatch"))
	}
	if welcome.ToolchainManifestDigest != request.ExpectedToolchainManifestDigest {
		return closeWithError(fmt.Errorf("guest welcome toolchain manifest digest mismatch"))
	}
	enabled := make(map[guestv1.GuestFeature]bool, len(welcome.EnabledFeatures))
	allowed := make(map[guestv1.GuestFeature]bool, len(request.RequestedFeatures)+len(request.MandatoryFeatures))
	for _, feature := range request.RequestedFeatures {
		allowed[feature] = true
	}
	for _, feature := range request.MandatoryFeatures {
		allowed[feature] = true
	}
	for _, feature := range welcome.EnabledFeatures {
		if feature == guestv1.GuestFeature_GUEST_FEATURE_UNSPECIFIED || !allowed[feature] {
			return closeWithError(fmt.Errorf("guest welcome enabled unrequested feature %s", feature.String()))
		}
		enabled[feature] = true
	}
	for _, feature := range request.MandatoryFeatures {
		if !enabled[feature] {
			return closeWithError(fmt.Errorf("guest welcome omitted mandatory feature %s", feature.String()))
		}
	}
	if welcome.HeartbeatIntervalMs == 0 || welcome.HeartbeatIntervalMs > uint64(maxGuestHeartbeatInterval.Milliseconds()) {
		return closeWithError(fmt.Errorf("guest welcome heartbeat interval is invalid"))
	}
	if !stopCallerCancellation() {
		return closeWithError(ctx.Err())
	}
	return &GuestProtocolSession{
		Connection:              connection,
		Stream:                  stream,
		Binding:                 binding,
		Generation:              welcome.SelectedGeneration,
		EnabledFeatures:         enabled,
		GuestBuildID:            welcome.GuestBuildId,
		ImageManifestDigest:     welcome.ImageManifestDigest,
		ToolchainManifestDigest: welcome.ToolchainManifestDigest,
		cancel:                  sessionCancel,
	}, nil
}

func (s *GuestProtocolSession) Close() error {
	if s == nil || s.Connection == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	return s.Connection.Close()
}

func sameConnectionBinding(left, right *guestv1.ConnectionBinding) bool {
	return left != nil &&
		right != nil &&
		left.InstanceId == right.InstanceId &&
		left.SandboxId == right.SandboxId &&
		left.SandboxGeneration == right.SandboxGeneration &&
		bytes.Equal(left.ConnectionNonce, right.ConnectionNonce)
}

func guestFeatureFromContractName(name string) (guestv1.GuestFeature, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "streaming_exec":
		return guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC, nil
	case "pty_resize":
		return guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE, nil
	case "descriptor_pinned_filesystem":
		return guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM, nil
	case "activity_events":
		return guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS, nil
	case "port_proxy":
		return guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY, nil
	default:
		return guestv1.GuestFeature_GUEST_FEATURE_UNSPECIFIED, fmt.Errorf("unknown mandatory guest feature %q", name)
	}
}
