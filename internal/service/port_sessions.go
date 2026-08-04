package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

const portTunnelTokenDomain = "secondbox/port-tunnel/v1\x00"

type portTunnelClaims struct {
	SessionID  string `json:"sid"`
	TenantRef  string `json:"ten"`
	SubjectRef string `json:"sub"`
	SandboxID  string `json:"sbx"`
	Generation int64  `json:"gen"`
	ExpiresAt  int64  `json:"exp"`
}

// CreateSandboxPortSession admits one PortSession on the transport the caller's
// authority grants. Admission itself is identical for both transports: the same
// transactional binding of tenant, subject, pinned ProfileRevision, Lease,
// assignment fence, generation, named port, protocol, duration, and limits.
func (service *ControlPlaneService) CreateSandboxPortSession(
	ctx context.Context,
	principal contracts.Principal,
	requestID string,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
	transport string,
	request contracts.CreatePortSessionRequest,
) (contracts.PortSession, bool, error) {
	if err := service.requirePortAuthority(principal); err != nil {
		return contracts.PortSession{}, false, err
	}
	if transport != contracts.PortTransportRelay && transport != contracts.PortTransportDirect {
		return contracts.PortSession{}, false, errors.New("SecondBox PortSession transport is invalid")
	}
	if requestID == "" || sandboxID == "" || generation < 1 || leaseID == "" {
		return contracts.PortSession{}, false, invalidRequest(errors.New("SecondBox PortSession authority is incomplete"))
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.PortSession{}, false, err
	}
	if !utf8.ValidString(request.Name) || strings.TrimSpace(request.Name) != request.Name ||
		request.Name == "" || utf8.RuneCountInString(request.Name) > 80 ||
		request.DurationSeconds < 1 || request.DurationSeconds > 86400 {
		return contracts.PortSession{}, false, invalidRequest(errors.New("SecondBox PortSession request is invalid"))
	}
	requestHash, err := hashCanonicalRequest(struct {
		Request    contracts.CreatePortSessionRequest `json:"request"`
		Generation int64                              `json:"generation"`
		LeaseID    string                             `json:"leaseId"`
	}{request, generation, leaseID})
	if err != nil {
		return contracts.PortSession{}, false, err
	}
	now := service.now().UTC()
	session := contracts.PortSession{
		ID: service.newID("port"), SandboxID: sandboxID, Generation: generation,
		Name: request.Name, Transport: transport, State: contracts.PortSessionStateOpen,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(request.DurationSeconds) * time.Second),
	}
	// The credential is derived before admission so the home Runner can be given
	// its digest in the same transaction that binds the session. The Runner never
	// holds the credential itself.
	credential, err := service.portTunnelCredential(session, principal.TenantRef, principal.SubjectRef)
	if err != nil {
		return contracts.PortSession{}, false, err
	}
	digest := sha256.Sum256([]byte(credential))
	tunnel, replayed, err := service.portSessionStore.AdmitPortSession(ctx, runnercontrol.PortSessionAdmission{
		Session:  session,
		StreamID: service.newID("stream"), TenantRef: principal.TenantRef,
		SubjectRef: principal.SubjectRef,
		RequestID:  requestID,
		LeaseID:    leaseID, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		CredentialDigest: digest[:], Now: now,
	})
	if err != nil {
		return contracts.PortSession{}, false, err
	}
	if tunnel.Session.Transport == contracts.PortTransportDirect {
		tunnel.Session.CertificateSPKISHA256 = tunnel.DataPlaneCertificateSPKISHA256
	}
	tunnel.Session.Endpoint, err = service.portTunnelEndpoint(tunnel, transport)
	return tunnel.Session, replayed, err
}

func (service *ControlPlaneService) GetSandboxPortSession(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	transport string,
) (contracts.PortSession, error) {
	if err := service.requirePortAuthority(principal); err != nil {
		return contracts.PortSession{}, err
	}
	tunnel, err := service.portSessionStore.GetPortTunnel(
		ctx, principal.TenantRef, principal.SubjectRef,
		sandboxID, sessionID, service.now().UTC(),
	)
	if err != nil {
		return contracts.PortSession{}, err
	}
	if tunnel.Session.Transport == contracts.PortTransportDirect {
		tunnel.Session.CertificateSPKISHA256 = tunnel.DataPlaneCertificateSPKISHA256
	}
	tunnel.Session.Endpoint, err = service.portTunnelEndpoint(tunnel, transport)
	if err != nil {
		return contracts.PortSession{}, err
	}
	return tunnel.Session, nil
}

func (service *ControlPlaneService) CloseSandboxPortSession(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	idempotencyKey string,
) error {
	if err := service.requirePortAuthority(principal); err != nil {
		return err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	requestHash, err := hashCanonicalRequest(struct {
		SandboxID string `json:"sandboxId"`
		SessionID string `json:"portSessionId"`
	}{sandboxID, sessionID})
	if err != nil {
		return err
	}
	_, err = service.portSessionStore.ClosePortSession(ctx, runnercontrol.PortTunnelClose{
		TenantRef: principal.TenantRef, SandboxID: sandboxID, SessionID: sessionID,
		SubjectRef:     principal.SubjectRef,
		IdempotencyKey: idempotencyKey,
		RequestHash:    requestHash, Reason: "application requested close", Now: service.now().UTC(),
	})
	return err
}

// ConsumePortTunnel atomically spends the signed URL before proxying any bytes.
func (service *ControlPlaneService) ConsumePortTunnel(
	ctx context.Context,
	endpoint string,
) (runnercontrol.PortTunnel, error) {
	token, err := service.portTunnelTokenFromEndpoint(endpoint)
	if err != nil {
		return runnercontrol.PortTunnel{}, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return runnercontrol.PortTunnel{}, ports.ErrPortTokenInvalid
	}
	return service.ConsumePortTunnelToken(ctx, path.Base(parsed.Path), token)
}

// ConsumePortTunnelToken spends a fragment-delivered credential for one exact session.
func (service *ControlPlaneService) ConsumePortTunnelToken(
	ctx context.Context,
	sessionID string,
	token string,
) (runnercontrol.PortTunnel, error) {
	claims, err := service.verifyPortTunnelToken(token)
	if err != nil {
		return runnercontrol.PortTunnel{}, err
	}
	if claims.SessionID != sessionID {
		return runnercontrol.PortTunnel{}, ports.ErrPortTokenInvalid
	}
	now := service.now().UTC()
	if now.Unix() >= claims.ExpiresAt {
		return runnercontrol.PortTunnel{}, ports.ErrPortTokenInvalid
	}
	return service.portSessionStore.ConsumePortSession(
		ctx, claims.TenantRef, claims.SubjectRef, claims.SessionID, now,
	)
}

// ClosePortTunnel terminates a consumed proxy and its useful-activity record.
func (service *ControlPlaneService) ClosePortTunnel(
	ctx context.Context,
	tunnel runnercontrol.PortTunnel,
	reason string,
) error {
	if reason == "" {
		return errors.New("SecondBox PortSession close reason is required")
	}
	_, err := service.portSessionStore.ClosePortSession(ctx, runnercontrol.PortTunnelClose{
		TenantRef: tunnel.TenantRef, SandboxID: tunnel.Session.SandboxID,
		SubjectRef: tunnel.SubjectRef,
		SessionID:  tunnel.Session.ID, Generation: tunnel.Session.Generation,
		Reason: reason, Now: service.now().UTC(),
	})
	return err
}

// SandboxPortStream forwards one proxied PortSession over the authenticated
// Runner connection without retaining payload bytes in PostgreSQL.
type SandboxPortStream struct {
	service        *ControlPlaneService
	tunnel         runnercontrol.PortTunnel
	stream         *runnercontrol.LiveDataPlaneStream
	mu             sync.Mutex
	nextSend       uint64
	nextReceive    uint64
	clientCredit   int64
	clientBytes    int64
	responseCredit int64
	runnerBytes    int64
	acknowledged   int64
	terminal       bool
}

func (service *ControlPlaneService) OpenPortTunnel(
	ctx context.Context,
	tunnel runnercontrol.PortTunnel,
) (*SandboxPortStream, error) {
	if tunnel.Session.Transport != contracts.PortTransportRelay ||
		tunnel.Session.State != contracts.PortSessionStateOpen ||
		tunnel.StreamWindowBytes < 1 || tunnel.MaximumRequestBytes < 1 ||
		tunnel.MaximumResponseBytes < 1 || service.liveDataPlane == nil {
		return nil, runnercontrol.ErrLiveDataPlaneUnavailable
	}
	stream, err := service.liveDataPlane.Open(
		tunnel.RunnerID, "port", tunnel.Session.ID, tunnel.StreamID,
	)
	if err != nil {
		return nil, err
	}
	result := &SandboxPortStream{
		service: service, tunnel: tunnel, stream: stream,
		nextSend: 1, nextReceive: 1, responseCredit: tunnel.StreamWindowBytes,
		acknowledged: tunnel.AcknowledgedInboundSequence,
	}
	idleTimeout := tunnel.Session.ExpiresAt.Sub(service.now().UTC()).Milliseconds()
	if idleTimeout < 1 {
		stream.Close()
		return nil, ports.ErrPortTokenInvalid
	}
	if err := result.send(&runnerv1.PortFrame_Open{Open: &runnerv1.PortOpen{
		GuestPort: uint32(tunnel.GuestPort), Protocol: tunnel.Session.Protocol,
		IdleTimeoutMs: uint64(idleTimeout),
	}}); err != nil {
		stream.Close()
		return nil, err
	}
	if err := result.send(&runnerv1.PortFrame_Credit{Credit: &runnerv1.StreamCredit{
		ByteCount: uint64(tunnel.StreamWindowBytes),
	}}); err != nil {
		stream.Close()
		return nil, err
	}
	return result, nil
}

func (stream *SandboxPortStream) send(payload any) error {
	frame := &runnerv1.PortFrame{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId:      stream.tunnel.AssignmentID,
			SandboxId:         stream.tunnel.Session.SandboxID,
			InstanceId:        stream.tunnel.InstanceID,
			SandboxGeneration: uint64(stream.tunnel.Session.Generation),
			FencingToken:      bytes.Clone(stream.tunnel.FencingToken),
		},
		OperationId: stream.tunnel.Session.ID, StreamId: stream.tunnel.StreamID,
		Sequence: stream.nextSend,
		Correlation: &runnerv1.Correlation{
			RequestId: stream.tunnel.RequestID, OperationId: stream.tunnel.Session.ID,
			SandboxId: stream.tunnel.Session.SandboxID, InstanceId: stream.tunnel.InstanceID,
			SandboxGeneration: uint64(stream.tunnel.Session.Generation),
			AssignmentId:      stream.tunnel.AssignmentID, LeaseId: stream.tunnel.LeaseID,
			RunnerId: stream.tunnel.RunnerID,
		},
	}
	switch value := payload.(type) {
	case *runnerv1.PortFrame_Open:
		frame.Payload = value
	case *runnerv1.PortFrame_Bytes:
		frame.Payload = value
	case *runnerv1.PortFrame_Credit:
		frame.Payload = value
	default:
		return errors.New("SecondBox live Port outbound payload is invalid")
	}
	if err := stream.stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Port{Port: frame},
	}); err != nil {
		return err
	}
	stream.nextSend++
	return nil
}

func (stream *SandboxPortStream) Send(ctx context.Context, payload []byte) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.terminal || len(payload) == 0 || int64(len(payload)) > stream.clientCredit ||
		stream.clientBytes+int64(len(payload)) > stream.tunnel.MaximumRequestBytes {
		if len(payload) > 0 && int64(len(payload)) > stream.clientCredit {
			return ports.ErrPortBackpressure
		}
		return runnercontrol.ErrDataPlaneSessionLimit
	}
	if err := stream.service.portSessionStore.RecordPortClientBytes(
		ctx, stream.tunnel.TenantRef, stream.tunnel.SubjectRef,
		stream.tunnel.Session.ID, payload, stream.service.now().UTC(),
	); err != nil {
		return err
	}
	stream.clientCredit -= int64(len(payload))
	stream.clientBytes += int64(len(payload))
	return stream.send(&runnerv1.PortFrame_Bytes{Bytes: &runnerv1.PortBytes{
		Data: bytes.Clone(payload),
	}})
}

func (stream *SandboxPortStream) Receive(
	ctx context.Context,
) (runnercontrol.PortTunnelEvent, error) {
	for {
		message, err := stream.stream.Receive(ctx)
		if err != nil {
			return runnercontrol.PortTunnelEvent{}, err
		}
		frame := message.GetPort()
		stream.mu.Lock()
		if frame == nil || frame.OperationId != stream.tunnel.Session.ID ||
			frame.StreamId != stream.tunnel.StreamID || frame.Sequence != stream.nextReceive ||
			!proto.Equal(frame.Fence, portTunnelFence(stream.tunnel)) ||
			!proto.Equal(frame.Correlation, portTunnelCorrelation(stream.tunnel)) || stream.terminal {
			stream.mu.Unlock()
			return runnercontrol.PortTunnelEvent{}, runnercontrol.ErrDataPlaneSequence
		}
		stream.nextReceive++
		switch {
		case frame.GetCredit() != nil:
			credit := int64(frame.GetCredit().ByteCount)
			if credit < 1 || stream.clientCredit+credit > stream.tunnel.StreamWindowBytes {
				stream.mu.Unlock()
				return runnercontrol.PortTunnelEvent{}, runnercontrol.ErrDataPlaneFrameLimit
			}
			stream.clientCredit += credit
			stream.mu.Unlock()
			continue
		case frame.GetBytes() != nil:
			payload := frame.GetBytes().Data
			if len(payload) == 0 || stream.runnerBytes+int64(len(payload)) > stream.responseCredit ||
				stream.runnerBytes+int64(len(payload)) > stream.tunnel.MaximumResponseBytes {
				stream.mu.Unlock()
				return runnercontrol.PortTunnelEvent{}, runnercontrol.ErrDataPlaneSessionLimit
			}
			stream.runnerBytes += int64(len(payload))
			event := runnercontrol.PortTunnelEvent{
				Sequence: int64(frame.Sequence), Bytes: bytes.Clone(payload),
			}
			stream.mu.Unlock()
			return event, nil
		case frame.GetTerminal() != nil:
			stream.terminal = true
			event := runnercontrol.PortTunnelEvent{
				Sequence:       int64(frame.Sequence),
				TerminalKind:   frame.GetTerminal().Kind.String(),
				TerminalDetail: frame.GetTerminal().SafeDetail,
			}
			stream.mu.Unlock()
			return event, nil
		default:
			stream.mu.Unlock()
			return runnercontrol.PortTunnelEvent{}, errors.New("SecondBox live Port response payload is invalid")
		}
	}
}

func (stream *SandboxPortStream) Acknowledge(
	ctx context.Context,
	event runnercontrol.PortTunnelEvent,
) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if event.Sequence <= stream.acknowledged ||
		(event.Bytes == nil) == (event.TerminalKind == "") {
		return runnercontrol.ErrDataPlaneSequence
	}
	if err := stream.service.portSessionStore.RecordPortTunnelAcknowledgement(
		ctx, stream.tunnel.TenantRef, stream.tunnel.SubjectRef,
		stream.tunnel.Session.ID, event.Sequence, stream.service.now().UTC(),
	); err != nil {
		return err
	}
	stream.acknowledged = event.Sequence
	if event.Bytes == nil {
		return nil
	}
	if err := stream.send(&runnerv1.PortFrame_Credit{Credit: &runnerv1.StreamCredit{
		ByteCount: uint64(len(event.Bytes)),
	}}); err != nil {
		return err
	}
	stream.responseCredit += int64(len(event.Bytes))
	return nil
}

func (stream *SandboxPortStream) Close() error {
	if stream == nil || stream.stream == nil {
		return nil
	}
	stream.stream.Close()
	return nil
}

func portTunnelFence(tunnel runnercontrol.PortTunnel) *runnerv1.AssignmentFence {
	return &runnerv1.AssignmentFence{
		AssignmentId: tunnel.AssignmentID, SandboxId: tunnel.Session.SandboxID,
		InstanceId: tunnel.InstanceID, SandboxGeneration: uint64(tunnel.Session.Generation),
		FencingToken: bytes.Clone(tunnel.FencingToken),
	}
}

func portTunnelCorrelation(tunnel runnercontrol.PortTunnel) *runnerv1.Correlation {
	return &runnerv1.Correlation{
		RequestId: tunnel.RequestID, OperationId: tunnel.Session.ID,
		SandboxId: tunnel.Session.SandboxID, InstanceId: tunnel.InstanceID,
		SandboxGeneration: uint64(tunnel.Session.Generation),
		AssignmentId:      tunnel.AssignmentID, LeaseId: tunnel.LeaseID,
		RunnerId: tunnel.RunnerID,
	}
}

func (service *ControlPlaneService) requirePortAuthority(principal contracts.Principal) error {
	if service.portSessionStore == nil {
		return ports.ErrLifecycleUnavailable
	}
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func validatedPublicBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("SecondBox public base URL must be an absolute HTTP or HTTPS URL")
	}
	return parsed, nil
}

// portTunnelCredential seals the single-use credential for one exact session.
// The same credential authenticates either transport; only the endpoint it is
// delivered in differs.
func (service *ControlPlaneService) portTunnelCredential(
	session contracts.PortSession,
	tenantRef string,
	subjectRef string,
) (string, error) {
	payload, err := json.Marshal(portTunnelClaims{
		SessionID: session.ID, TenantRef: tenantRef, SubjectRef: subjectRef,
		SandboxID: session.SandboxID, Generation: session.Generation,
		ExpiresAt: session.ExpiresAt.UTC().Unix(),
	})
	if err != nil {
		return "", err
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, service.credentialSealSecret)
	_, _ = mac.Write([]byte(portTunnelTokenDomain))
	_, _ = mac.Write([]byte(payloadPart))
	return payloadPart + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// portTunnelEndpoint returns the endpoint for the session's admitted transport.
//
// A caller whose authority does not grant the direct transport never receives a
// Runner address, so a direct session is unaddressable to it rather than
// downgraded to a proxied endpoint that its home Runner would refuse.
func (service *ControlPlaneService) portTunnelEndpoint(
	tunnel runnercontrol.PortTunnel,
	grantedTransport string,
) (string, error) {
	credential, err := service.portTunnelCredential(
		tunnel.Session, tunnel.TenantRef, tunnel.SubjectRef,
	)
	if err != nil {
		return "", err
	}
	if tunnel.Session.Transport == contracts.PortTransportDirect {
		if grantedTransport != contracts.PortTransportDirect {
			return "", ports.ErrAuthorizationDenied
		}
		return service.directPortEndpoint(tunnel, credential)
	}
	base, err := validatedPublicBaseURL(service.publicBaseURL)
	if err != nil {
		return "", err
	}
	if base.Scheme == "https" {
		base.Scheme = "wss"
	} else {
		base.Scheme = "ws"
	}
	base.Path = path.Join(base.Path, "/v1/port-tunnels", tunnel.Session.ID)
	base.Fragment = credential
	return base.String(), nil
}

// directPortEndpoint addresses the home Runner's advertised caller-facing
// listener. The scheme names the framed credential handshake that precedes any
// payload byte so a caller cannot mistake it for a plain TCP forward.
func (service *ControlPlaneService) directPortEndpoint(
	tunnel runnercontrol.PortTunnel,
	credential string,
) (string, error) {
	if tunnel.DataPlaneAddress == "" {
		return "", ports.ErrLifecycleUnavailable
	}
	certificatePin, err := hex.DecodeString(tunnel.DataPlaneCertificateSPKISHA256)
	if err != nil || len(certificatePin) != sha256.Size ||
		tunnel.DataPlaneCertificateSPKISHA256 != hex.EncodeToString(certificatePin) {
		return "", ports.ErrLifecycleUnavailable
	}
	host, port, err := net.SplitHostPort(tunnel.DataPlaneAddress)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return "", errors.New("SecondBox Runner data-plane address is invalid")
	}
	endpoint := url.URL{
		Scheme:   "secondbox+tcp",
		Host:     net.JoinHostPort(host, port),
		Path:     path.Join("/v1/port-sessions", tunnel.Session.ID),
		Fragment: credential,
	}
	return endpoint.String(), nil
}

func (service *ControlPlaneService) portTunnelTokenFromEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment == "" {
		return "", ports.ErrPortTokenInvalid
	}
	token := parsed.Fragment
	if path.Base(parsed.Path) == "." || path.Base(parsed.Path) == "/" ||
		path.Base(parsed.Path) == "" {
		return "", ports.ErrPortTokenInvalid
	}
	return token, nil
}

func (service *ControlPlaneService) verifyPortTunnelToken(token string) (portTunnelClaims, error) {
	payloadPart, signaturePart, found := strings.Cut(token, ".")
	if !found || payloadPart == "" || signaturePart == "" {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signaturePart)
	if err != nil || len(signature) != sha256.Size {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	mac := hmac.New(sha256.New, service.credentialSealSecret)
	_, _ = mac.Write([]byte(portTunnelTokenDomain))
	_, _ = mac.Write([]byte(payloadPart))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil || len(payload) > 1024 {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	var claims portTunnelClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil ||
		claims.SessionID == "" || claims.TenantRef == "" || claims.SubjectRef == "" ||
		claims.SandboxID == "" ||
		claims.Generation < 1 || claims.ExpiresAt < 1 {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return portTunnelClaims{}, ports.ErrPortTokenInvalid
	}
	return claims, nil
}
