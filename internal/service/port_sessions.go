package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
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
		return contracts.PortSession{}, false, errors.New("SecondBox PortSession authority is incomplete")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.PortSession{}, false, err
	}
	if !utf8.ValidString(request.Name) || strings.TrimSpace(request.Name) != request.Name ||
		request.Name == "" || utf8.RuneCountInString(request.Name) > 80 ||
		request.DurationSeconds < 1 || request.DurationSeconds > 86400 {
		return contracts.PortSession{}, false, errors.New("SecondBox PortSession request is invalid")
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
	tunnel, replayed, err := service.portSessionRelay.AdmitPortSession(ctx, runnercontrol.PortSessionAdmission{
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
	tunnel, err := service.portSessionRelay.GetPortTunnel(
		ctx, principal.TenantRef, principal.SubjectRef,
		sandboxID, sessionID, service.now().UTC(),
	)
	if err != nil {
		return contracts.PortSession{}, err
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
	_, err = service.portSessionRelay.ClosePortSession(ctx, runnercontrol.PortTunnelClose{
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
	return service.portSessionRelay.ConsumePortSession(
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
	_, err := service.portSessionRelay.ClosePortSession(ctx, runnercontrol.PortTunnelClose{
		TenantRef: tunnel.TenantRef, SandboxID: tunnel.Session.SandboxID,
		SubjectRef: tunnel.SubjectRef,
		SessionID:  tunnel.Session.ID, Generation: tunnel.Session.Generation,
		Reason: reason, Now: service.now().UTC(),
	})
	return err
}

// QueuePortTunnelBytes forwards one bounded client WebSocket message to the runner.
func (service *ControlPlaneService) QueuePortTunnelBytes(
	ctx context.Context,
	tunnel runnercontrol.PortTunnel,
	payload []byte,
) error {
	return service.portSessionRelay.QueuePortClientBytes(
		ctx, tunnel.TenantRef, tunnel.SubjectRef,
		tunnel.Session.ID, payload, service.now().UTC(),
	)
}

// NextPortTunnelEvent returns the next unacknowledged runner payload or terminal event.
func (service *ControlPlaneService) NextPortTunnelEvent(
	ctx context.Context,
	tunnel runnercontrol.PortTunnel,
	afterSequence int64,
) (runnercontrol.PortTunnelEvent, bool, error) {
	return service.portSessionRelay.NextPortTunnelEvent(
		ctx, tunnel.TenantRef, tunnel.SubjectRef,
		tunnel.Session.ID, afterSequence, service.now().UTC(),
	)
}

// AcknowledgePortTunnelEvent releases runner credit only after client delivery.
func (service *ControlPlaneService) AcknowledgePortTunnelEvent(
	ctx context.Context,
	tunnel runnercontrol.PortTunnel,
	sequence int64,
) error {
	return service.portSessionRelay.AcknowledgePortTunnelEvent(
		ctx, tunnel.TenantRef, tunnel.SubjectRef,
		tunnel.Session.ID, sequence, service.now().UTC(),
	)
}

func (service *ControlPlaneService) requirePortAuthority(principal contracts.Principal) error {
	if service.portSessionRelay == nil {
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
// downgraded to a relay endpoint that its home Runner would refuse.
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
