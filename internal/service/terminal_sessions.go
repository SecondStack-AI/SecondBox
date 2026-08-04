package service

import (
	"context"
	"errors"
	"net/url"
	"path"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (service *ControlPlaneService) terminalStore() (terminalDataPlaneStore, error) {
	store, ok := service.dataPlaneStore.(terminalDataPlaneStore)
	if !ok {
		return nil, errors.New("SecondBox Terminal store is unavailable")
	}
	return store, nil
}

// CreateSandboxTerminal admits one leased PTY against the pinned Profile.
func (service *ControlPlaneService) CreateSandboxTerminal(
	ctx context.Context,
	principal contracts.Principal,
	requestID string,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
	request contracts.CreateTerminalRequest,
) (runnercontrol.DataPlaneSession, bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if leaseID == "" ||
		request.Rows < 1 || request.Rows > 1000 ||
		request.Columns < 1 || request.Columns > 1000 {
		return runnercontrol.DataPlaneSession{}, false, invalidRequest(errors.New("SecondBox Terminal Lease or dimensions are invalid"))
	}
	if _, err := validateBufferedExecRequest(contracts.BufferedExecRequest{
		Command: request.Command, Cwd: request.Cwd, Environment: request.Environment,
		DeadlineMilliseconds: request.DeadlineMilliseconds, MaximumOutputBytes: 1,
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	now := service.now().UTC()
	deadline := now.Add(time.Duration(request.DeadlineMilliseconds) * time.Millisecond)
	open := publicExecOpen(request.Command, request.Cwd, request.Environment, deadline, 0, nil)
	open.AllocatePty = true
	open.PtyRows = uint32(request.Rows)
	open.PtyColumns = uint32(request.Columns)
	open.Streaming = true
	sessionID := service.newID("term")
	return service.dataPlaneStore.AdmitDataPlane(ctx, runnercontrol.DataPlaneAdmission{
		ID: sessionID, StreamID: service.newID("stream"),
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		LeaseID:    leaseID,
		RequestID:  requestID, Generation: generation,
		Kind: "terminal", Operation: "terminal", IdempotencyKey: idempotencyKey,
		RequestHash: requestHash, DeadlineAt: deadline,
		UseProfileResponseLimit: true, UseProfileRequestLimit: true,
		UseProfileStreamWindow: true, DeferResponseCredit: true,
		Detachable: request.Detachable, ExecOpen: open, Request: request, Now: now,
		CredentialDigest: service.dataPlaneCredentialDigest(sessionID),
	})
}

// GetSandboxTerminal resolves one project-owned, generation-bound Terminal.
func (service *ControlPlaneService) GetSandboxTerminal(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	generation int64,
) (runnercontrol.DataPlaneSession, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	session, err := service.dataPlaneStore.GetDataPlaneSession(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID,
	)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	if session.Kind != "terminal" || session.Operation != "terminal" ||
		session.SandboxID != sandboxID {
		return runnercontrol.DataPlaneSession{}, runnercontrol.ErrDataPlaneNotFound
	}
	if session.Generation != generation {
		return runnercontrol.DataPlaneSession{}, ports.ErrGenerationFenced
	}
	return session, nil
}

// AcquireSandboxTerminalAttachment grants the single WebSocket attachment.
func (service *ControlPlaneService) AcquireSandboxTerminalAttachment(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	generation int64,
) (runnercontrol.DataPlaneSession, string, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, "", err
	}
	store, err := service.terminalStore()
	if err != nil {
		return runnercontrol.DataPlaneSession{}, "", err
	}
	attachmentID := service.newID("attach")
	session, err := store.AcquireTerminalAttachment(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
		sessionID, generation, attachmentID, service.now().UTC(),
	)
	return session, attachmentID, err
}

func (service *ControlPlaneService) DetachSandboxTerminalAttachment(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
	attachmentID string,
) (bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return false, err
	}
	store, err := service.terminalStore()
	if err != nil {
		return false, err
	}
	return store.DetachTerminalAttachment(
		ctx, principal.TenantRef, principal.SubjectRef,
		sessionID, attachmentID, service.now().UTC(),
	)
}

// CancelSandboxTerminal durably replays one key-scoped public cancellation response.
func (service *ControlPlaneService) CancelSandboxTerminal(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	generation int64,
	idempotencyKey string,
) (runnercontrol.DataPlaneSession, bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	requestHash, err := hashPublicSessionCancellation(sandboxID, sessionID, generation)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	now := service.now().UTC()
	return service.dataPlaneStore.CancelPublicDataPlaneSession(
		ctx,
		runnercontrol.PublicDataPlaneCancellation{
			TenantRef: principal.TenantRef, SandboxID: sandboxID, SessionID: sessionID,
			SubjectRef:  principal.SubjectRef,
			SessionKind: "terminal", SessionOperation: "terminal",
			IdempotencyKey: idempotencyKey,
			RequestHash:    requestHash, Reason: "public Terminal cancellation",
			Generation: generation, Now: now, IdempotencyEnds: now.Add(idempotencyRetention),
		},
	)
}

func (service *ControlPlaneService) SandboxTerminalEndpoint(
	sandboxID string,
	sessionID string,
) (string, error) {
	base, err := validatedPublicBaseURL(service.publicBaseURL)
	if err != nil {
		return "", err
	}
	if base.Scheme == "https" {
		base.Scheme = "wss"
	} else {
		base.Scheme = "ws"
	}
	base.Path = path.Join(
		base.Path, "/v1/sandboxes", url.PathEscape(sandboxID),
		"terminals", url.PathEscape(sessionID),
	)
	return base.String(), nil
}

func (service *ControlPlaneService) SandboxTerminalOutcome(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
) (any, error) {
	session, err := service.dataPlaneStore.GetDataPlaneSession(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID,
	)
	if err != nil {
		return nil, err
	}
	if session.Operation != "terminal" {
		return nil, runnercontrol.ErrDataPlaneNotFound
	}
	return execOutcome(session)
}
