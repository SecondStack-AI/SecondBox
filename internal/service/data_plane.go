package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// DataPlaneRelay is the durable runner relay used by synchronous public operations.
type DataPlaneRelay interface {
	AdmitDataPlane(context.Context, runnercontrol.DataPlaneAdmission) (runnercontrol.DataPlaneSession, bool, error)
	GetDataPlaneSession(context.Context, string, string, string) (runnercontrol.DataPlaneSession, error)
	ExpireDataPlaneSession(context.Context, string, string, string, time.Time) (runnercontrol.DataPlaneSession, error)
	AppendExecClientFrame(context.Context, string, string, string, runnercontrol.ExecClientFrame, time.Time) (bool, error)
	ListExecServerFrames(context.Context, string, string, string, int64, int) ([]runnercontrol.ExecServerFrame, error)
	CancelDataPlaneSession(context.Context, string, string, string, string, time.Time) (bool, error)
	CancelPublicDataPlaneSession(context.Context, runnercontrol.PublicDataPlaneCancellation) (runnercontrol.DataPlaneSession, bool, error)
}

type terminalDataPlaneRelay interface {
	DataPlaneRelay
	AcquireTerminalAttachment(context.Context, string, string, string, string, int64, string, time.Time) (runnercontrol.DataPlaneSession, error)
	DetachTerminalAttachment(context.Context, string, string, string, string, time.Time) (bool, error)
	AppendTerminalClientFrame(context.Context, string, string, string, string, runnercontrol.TerminalClientFrame, time.Time) (bool, error)
	ListTerminalServerFrames(context.Context, string, string, string, int64, int) ([]runnercontrol.TerminalServerFrame, error)
}

func (service *ControlPlaneService) CreateSandboxExecStream(
	ctx context.Context,
	principal contracts.Principal,
	requestID string,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
	request contracts.StreamingExecRequest,
) (runnercontrol.DataPlaneSession, bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if request.WindowBytes < 4096 {
		return runnercontrol.DataPlaneSession{}, false, errors.New("SecondBox streaming Exec window is invalid")
	}
	if _, err := validateBufferedExecRequest(contracts.BufferedExecRequest{
		Command: request.Command, Cwd: request.Cwd, Environment: request.Environment,
		DeadlineMilliseconds: request.DeadlineMilliseconds,
		MaximumOutputBytes:   request.MaximumOutputBytes,
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	now := service.now().UTC()
	open := publicExecOpen(
		request.Command, request.Cwd, request.Environment,
		now.Add(time.Duration(request.DeadlineMilliseconds)*time.Millisecond),
		request.MaximumOutputBytes, nil,
	)
	open.Streaming = true
	return service.dataPlaneRelay.AdmitDataPlane(ctx, runnercontrol.DataPlaneAdmission{
		ID: service.newID("dps"), StreamID: service.newID("stream"),
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		LeaseID:    leaseID,
		RequestID:  requestID, Generation: generation,
		Kind: "exec", Operation: "exec-stream", IdempotencyKey: idempotencyKey,
		RequestHash:          requestHash,
		DeadlineAt:           now.Add(time.Duration(request.DeadlineMilliseconds) * time.Millisecond),
		MaximumResponseBytes: request.MaximumOutputBytes,
		StreamWindowBytes:    request.WindowBytes, UseProfileRequestLimit: true,
		DeferResponseCredit: true, ExecOpen: open, Request: request, Now: now,
	})
}

func (service *ControlPlaneService) GetSandboxExecStream(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	sessionID string,
	generation int64,
) (runnercontrol.DataPlaneSession, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	session, err := service.dataPlaneRelay.GetDataPlaneSession(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID,
	)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	if session.Kind != "exec" || session.Operation != "exec-stream" ||
		session.SandboxID != sandboxID {
		return runnercontrol.DataPlaneSession{}, runnercontrol.ErrDataPlaneNotFound
	}
	if session.Generation != generation {
		return runnercontrol.DataPlaneSession{}, ports.ErrGenerationFenced
	}
	return session, nil
}

func (service *ControlPlaneService) AppendSandboxExecStreamFrame(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
	frame runnercontrol.ExecClientFrame,
) (bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return false, err
	}
	return service.dataPlaneRelay.AppendExecClientFrame(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID, frame, service.now().UTC(),
	)
}

func (service *ControlPlaneService) ListSandboxExecStreamFrames(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
	afterSequence int64,
) ([]runnercontrol.ExecServerFrame, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return nil, err
	}
	return service.dataPlaneRelay.ListExecServerFrames(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID, afterSequence, 64,
	)
}

func (service *ControlPlaneService) CancelSandboxExecStream(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
	reason string,
) (bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return false, err
	}
	return service.dataPlaneRelay.CancelDataPlaneSession(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID, reason, service.now().UTC(),
	)
}

// CancelSandboxExecStreamAtGeneration durably replays one key-scoped public cancellation response.
func (service *ControlPlaneService) CancelSandboxExecStreamAtGeneration(
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
	return service.dataPlaneRelay.CancelPublicDataPlaneSession(
		ctx,
		runnercontrol.PublicDataPlaneCancellation{
			TenantRef: principal.TenantRef, SandboxID: sandboxID, SessionID: sessionID,
			SubjectRef:  principal.SubjectRef,
			SessionKind: "exec", SessionOperation: "exec-stream",
			IdempotencyKey: idempotencyKey,
			RequestHash:    requestHash, Reason: "public streaming client cancelled",
			Generation: generation, Now: now, IdempotencyEnds: now.Add(idempotencyRetention),
		},
	)
}

func hashPublicSessionCancellation(
	sandboxID string,
	sessionID string,
	generation int64,
) (string, error) {
	return hashCanonicalRequest(struct {
		SandboxID  string `json:"sandboxId"`
		SessionID  string `json:"sessionId"`
		Generation int64  `json:"generation"`
	}{
		SandboxID: sandboxID, SessionID: sessionID, Generation: generation,
	})
}

func (service *ControlPlaneService) SandboxExecStreamEndpoint(
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
	base.Path = path.Join(base.Path, "/v1/sandboxes", url.PathEscape(sandboxID), "exec-streams", url.PathEscape(sessionID))
	return base.String(), nil
}

func (service *ControlPlaneService) DataPlanePollInterval() time.Duration {
	return service.dataPlanePollInterval
}

func (service *ControlPlaneService) SandboxExecStreamOutcome(
	ctx context.Context,
	principal contracts.Principal,
	sessionID string,
) (any, error) {
	session, err := service.dataPlaneRelay.GetDataPlaneSession(
		ctx, principal.TenantRef, principal.SubjectRef, sessionID,
	)
	if err != nil {
		return nil, err
	}
	if session.Operation != "exec-stream" {
		return nil, runnercontrol.ErrDataPlaneNotFound
	}
	return execOutcome(session)
}

func (service *ControlPlaneService) ExecuteSandboxCommand(
	ctx context.Context,
	principal contracts.Principal,
	requestID string,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
	request contracts.BufferedExecRequest,
) (any, bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return nil, false, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return nil, false, err
	}
	stdin, err := validateBufferedExecRequest(request)
	if err != nil {
		return nil, false, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return nil, false, err
	}
	now := service.now().UTC()
	deadline := now.Add(time.Duration(request.DeadlineMilliseconds) * time.Millisecond)
	open := publicExecOpen(
		request.Command, request.Cwd, request.Environment, deadline,
		request.MaximumOutputBytes, stdin,
	)
	session, replayed, err := service.dataPlaneRelay.AdmitDataPlane(ctx, runnercontrol.DataPlaneAdmission{
		ID: service.newID("dps"), StreamID: service.newID("stream"),
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		LeaseID:    leaseID,
		RequestID:  requestID,
		Generation: generation, Kind: "exec", Operation: "exec",
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		DeadlineAt:           deadline,
		MaximumResponseBytes: request.MaximumOutputBytes, MaximumRequestBytes: int64(len(stdin)),
		ExecOpen: open, Request: request, Now: now,
	})
	if err != nil {
		return nil, false, err
	}
	session, err = service.waitForDataPlane(
		ctx, principal.TenantRef, principal.SubjectRef, session, request.DeadlineMilliseconds,
	)
	if err != nil {
		return nil, replayed, err
	}
	outcome, err := execOutcome(session)
	return outcome, replayed, err
}

func publicExecOpen(
	command contracts.ExecCommand,
	cwd *string,
	environment map[string]string,
	deadline time.Time,
	maximumOutputBytes int64,
	stdin []byte,
) *runnerv1.ExecOpen {
	open := &runnerv1.ExecOpen{
		DeadlineUnixMs:   uint64(deadline.UnixMilli()),
		OutputLimitBytes: uint64(maximumOutputBytes),
		Stdin:            append([]byte(nil), stdin...),
	}
	if cwd != nil {
		open.Cwd = *cwd
	}
	if command.Mode == "shell" {
		open.Command = &runnerv1.ExecOpen_Shell{Shell: command.Command}
	} else {
		open.Command = &runnerv1.ExecOpen_Argv{Argv: &runnerv1.ArgvCommand{
			Argument: append([]string{command.Executable}, command.Arguments...),
		}}
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		open.Environment = append(open.Environment, &runnerv1.EnvironmentEntry{
			Name: name, Value: []byte(environment[name]),
		})
	}
	return open
}

func (service *ControlPlaneService) ReadSandboxFile(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, path string,
) ([]byte, string, error) {
	session, _, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, "", "read", path,
		false, false, nil, "", 0,
	)
	if err != nil {
		return nil, "", err
	}
	if err := fileTerminalError(session); err != nil {
		return nil, "", err
	}
	if session.Metadata == nil || session.Metadata.Checksum == "" {
		return nil, "", errors.New("SecondBox File read returned no checksum evidence")
	}
	return session.Content, session.Metadata.Checksum, nil
}

func (service *ControlPlaneService) WriteSandboxFile(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, idempotencyKey string,
	path string, content []byte, sha256Hex string,
) (contracts.FileWriteResult, bool, error) {
	session, replayed, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, idempotencyKey, "write", path,
		false, false, content, "sha256:"+sha256Hex, int64(len(content)),
	)
	if err != nil {
		return contracts.FileWriteResult{}, replayed, err
	}
	if err := fileTerminalError(session); err != nil {
		return contracts.FileWriteResult{}, replayed, err
	}
	return contracts.FileWriteResult{Path: path, SizeBytes: int64(len(content)), SHA256: sha256Hex}, replayed, nil
}

func (service *ControlPlaneService) StatSandboxFile(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, path string,
) (contracts.FileStat, error) {
	session, _, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, "", "stat", path,
		false, false, nil, "", 0,
	)
	if err != nil {
		return contracts.FileStat{}, err
	}
	if err := fileTerminalError(session); err != nil {
		return contracts.FileStat{}, err
	}
	if session.Metadata == nil {
		return contracts.FileStat{}, errors.New("SecondBox File stat returned no metadata")
	}
	return publicFileStat(path, session.Metadata.Kind, session.Metadata.Size, session.Metadata.ModifiedAtUnixMs)
}

func (service *ControlPlaneService) SandboxFileExists(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, path string,
) (contracts.FileExistsResult, error) {
	session, _, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, "", "exists", path,
		false, false, nil, "", 0,
	)
	if err != nil {
		return contracts.FileExistsResult{}, err
	}
	if err := fileTerminalError(session); err != nil {
		return contracts.FileExistsResult{}, err
	}
	if session.Metadata == nil {
		return contracts.FileExistsResult{}, errors.New("SecondBox File exists returned no metadata")
	}
	return contracts.FileExistsResult{Path: path, Exists: session.Metadata.Exists}, nil
}

func (service *ControlPlaneService) ListSandboxDirectory(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, path string,
) (contracts.DirectoryListing, error) {
	session, _, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, "", "list", path,
		false, false, nil, "", 0,
	)
	if err != nil {
		return contracts.DirectoryListing{}, err
	}
	if err := fileTerminalError(session); err != nil {
		return contracts.DirectoryListing{}, err
	}
	if session.Metadata == nil || len(session.Metadata.DirectChildEntries) > 10_000 {
		return contracts.DirectoryListing{}, runnercontrol.ErrRelaySessionLimit
	}
	result := contracts.DirectoryListing{Path: path, Entries: make([]contracts.FileStat, 0, len(session.Metadata.DirectChildEntries))}
	for _, entry := range session.Metadata.DirectChildEntries {
		if entry == nil {
			return contracts.DirectoryListing{}, errors.New("SecondBox directory list contains empty metadata")
		}
		stat, err := publicFileStat(entry.Path, entry.Kind, entry.Size, entry.ModifiedAtUnixMs)
		if err != nil {
			return contracts.DirectoryListing{}, err
		}
		result.Entries = append(result.Entries, stat)
	}
	return result, nil
}

func (service *ControlPlaneService) CreateSandboxDirectory(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, idempotencyKey string,
	request contracts.CreateDirectoryRequest,
) (bool, error) {
	if request.Recursive == nil {
		return false, errors.New("SecondBox recursive directory option is required")
	}
	session, replayed, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, idempotencyKey, "mkdir",
		request.Path, *request.Recursive, false, nil, "", 0,
	)
	if err != nil {
		return replayed, err
	}
	return replayed, fileTerminalError(session)
}

func (service *ControlPlaneService) RemoveSandboxPath(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, idempotencyKey string,
	request contracts.RemovePathRequest,
) (bool, error) {
	if request.Recursive == nil || request.Force == nil {
		return false, errors.New("SecondBox recursive and force remove options are required")
	}
	session, replayed, err := service.runFileOperation(
		ctx, principal, requestID, sandboxID, generation, leaseID, idempotencyKey, "remove",
		request.Path, *request.Recursive, *request.Force, nil, "", 0,
	)
	if err != nil {
		return replayed, err
	}
	return replayed, fileTerminalError(session)
}

func (service *ControlPlaneService) runFileOperation(
	ctx context.Context, principal contracts.Principal, requestID string, sandboxID string,
	generation int64, leaseID string, idempotencyKey string, operation string,
	path string, recursive bool, force bool, content []byte, checksum string, expectedSize int64,
) (runnercontrol.DataPlaneSession, bool, error) {
	if err := service.requireDataPlane(principal); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if err := validateWorkspacePath(path); err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	if idempotencyKey != "" {
		if err := validateIdempotencyKey(idempotencyKey); err != nil {
			return runnercontrol.DataPlaneSession{}, false, err
		}
	}
	request := struct {
		Path      string `json:"path"`
		Operation string `json:"operation"`
		Recursive bool   `json:"recursive"`
		Force     bool   `json:"force"`
		Checksum  string `json:"checksum"`
		Size      int64  `json:"size"`
	}{path, operation, recursive, force, checksum, expectedSize}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	fileOperation, err := runnerFileOperation(operation)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	now := service.now().UTC()
	const fileDeadlineMilliseconds int64 = 30_000
	session, replayed, err := service.dataPlaneRelay.AdmitDataPlane(ctx, runnercontrol.DataPlaneAdmission{
		ID: service.newID("dps"), StreamID: service.newID("stream"),
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		LeaseID:    leaseID,
		RequestID:  requestID,
		Generation: generation, Kind: "file", Operation: operation,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		DeadlineAt:           now.Add(time.Duration(fileDeadlineMilliseconds) * time.Millisecond),
		MaximumResponseBytes: maximumFileResponseBytes(operation),
		MaximumRequestBytes:  int64(len(content)),
		FileOpen: &runnerv1.FileOpen{
			Operation: fileOperation, WorkspaceRelativePath: path,
			ExpectedSize: uint64(expectedSize), ExpectedChecksum: checksum,
			Recursive: recursive, Force: force,
		},
		FileContent: content, Request: request, Now: now,
	})
	if err != nil {
		return runnercontrol.DataPlaneSession{}, false, err
	}
	session, err = service.waitForDataPlane(
		ctx, principal.TenantRef, principal.SubjectRef, session, fileDeadlineMilliseconds,
	)
	return session, replayed, err
}

func (service *ControlPlaneService) waitForDataPlane(
	ctx context.Context, tenantRef string, subjectRef string,
	session runnercontrol.DataPlaneSession,
	maximumWaitMilliseconds int64,
) (runnercontrol.DataPlaneSession, error) {
	if session.State == "completed" || session.State == "failed" {
		return session, nil
	}
	ticker := time.NewTicker(service.dataPlanePollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Duration(maximumWaitMilliseconds) * time.Millisecond)
	defer timer.Stop()
	timerChannel := timer.C
	for {
		select {
		case <-ctx.Done():
			return runnercontrol.DataPlaneSession{}, ctx.Err()
		case <-timerChannel:
			expired, err := service.dataPlaneRelay.ExpireDataPlaneSession(
				ctx, tenantRef, subjectRef, session.ID, service.now().UTC(),
			)
			if err != nil {
				return runnercontrol.DataPlaneSession{}, err
			}
			if session.Kind == "exec" {
				return expired, nil
			}
			timerChannel = nil
		case <-ticker.C:
			current, err := service.dataPlaneRelay.GetDataPlaneSession(
				ctx, tenantRef, subjectRef, session.ID,
			)
			if err != nil {
				return runnercontrol.DataPlaneSession{}, err
			}
			if current.State == "completed" || current.State == "failed" {
				return current, nil
			}
		}
	}
}

func (service *ControlPlaneService) requireDataPlane(principal contracts.Principal) error {
	if service.dataPlaneRelay == nil {
		return ports.ErrLifecycleUnavailable
	}
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func validateBufferedExecRequest(request contracts.BufferedExecRequest) ([]byte, error) {
	if request.Environment == nil || len(request.Environment) > 128 ||
		request.DeadlineMilliseconds < 1 || request.MaximumOutputBytes < 1 {
		return nil, errors.New("SecondBox buffered Exec bounds are invalid")
	}
	if request.Cwd != nil {
		if err := validateWorkspacePath(*request.Cwd); err != nil {
			return nil, err
		}
	}
	switch request.Command.Mode {
	case "shell":
		if request.Command.Command == "" || len(request.Command.Command) > 1<<20 {
			return nil, errors.New("SecondBox shell command is invalid")
		}
	case "argv":
		if request.Command.Executable == "" || len(request.Command.Executable) > 4096 ||
			request.Command.Arguments == nil || len(request.Command.Arguments) > 4096 {
			return nil, errors.New("SecondBox argv command is invalid")
		}
		for _, argument := range request.Command.Arguments {
			if len(argument) > 131072 {
				return nil, errors.New("SecondBox argv argument exceeds its bound")
			}
		}
	default:
		return nil, errors.New("SecondBox Exec command mode is invalid")
	}
	for name, value := range request.Environment {
		if name == "" || len(name) > 256 || len(value) > 8192 {
			return nil, errors.New("SecondBox Exec environment exceeds its bound")
		}
	}
	if request.StdinBase64 != nil && len(*request.StdinBase64) > 1_398_104 {
		return nil, errors.New("SecondBox buffered stdin exceeds its encoded bound")
	}
	if request.StdinBase64 == nil {
		return nil, nil
	}
	stdin, err := base64.StdEncoding.Strict().DecodeString(*request.StdinBase64)
	if err != nil {
		return nil, errors.New("SecondBox stdinBase64 is not canonical base64")
	}
	return stdin, nil
}

func validateWorkspacePath(path string) error {
	if len(path) < 1 || len(path) > 4096 || strings.ContainsRune(path, 0) ||
		strings.HasPrefix(path, "/") {
		return errors.New("SecondBox workspace path is invalid")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return errors.New("SecondBox workspace path contains a parent segment")
		}
	}
	return nil
}

func runnerFileOperation(operation string) (runnerv1.FileOperation, error) {
	switch operation {
	case "read":
		return runnerv1.FileOperation_FILE_OPERATION_READ, nil
	case "write":
		return runnerv1.FileOperation_FILE_OPERATION_WRITE, nil
	case "stat":
		return runnerv1.FileOperation_FILE_OPERATION_STAT, nil
	case "list":
		return runnerv1.FileOperation_FILE_OPERATION_LIST, nil
	case "exists":
		return runnerv1.FileOperation_FILE_OPERATION_EXISTS, nil
	case "mkdir":
		return runnerv1.FileOperation_FILE_OPERATION_MKDIR, nil
	case "remove":
		return runnerv1.FileOperation_FILE_OPERATION_REMOVE, nil
	default:
		return 0, errors.New("SecondBox File operation is invalid")
	}
}

func maximumFileResponseBytes(operation string) int64 {
	return 0
}

func execOutcome(session runnercontrol.DataPlaneSession) (any, error) {
	output := contracts.ExecOutput{
		StdoutBase64: base64.StdEncoding.EncodeToString(session.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(session.Stderr),
	}
	switch session.TerminalKind {
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED.String():
		if session.ExitCode < 0 || session.ExitCode > 255 || session.Signal < 0 || session.Signal > 64 {
			return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
		}
		var signal *int32
		if session.Signal > 0 {
			value := session.Signal
			signal = &value
		}
		return contracts.ExecExited{Kind: "exited", ExitCode: session.ExitCode, Signal: signal, Output: output}, nil
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED.String():
		reason, err := publicSpawnFailureReason(session.SpawnFailureReason)
		if err != nil || len(session.TerminalMessage) < 1 || len(session.TerminalMessage) > 2048 {
			return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
		}
		return contracts.ExecSpawnFailed{
			Kind: "spawn_failed", Reason: reason,
			Message: session.TerminalMessage,
		}, nil
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED.String():
		if session.ElapsedMilliseconds < 0 {
			return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
		}
		return contracts.ExecDeadlineExceeded{
			Kind: "deadline_exceeded", ElapsedMilliseconds: session.ElapsedMilliseconds, Output: output,
		}, nil
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String():
		return contracts.ExecCancelled{Kind: "cancelled", Output: output}, nil
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String():
		if session.LimitBytes < 1 {
			return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
		}
		return contracts.ExecOutputExhausted{Kind: "output_exhausted", LimitBytes: session.LimitBytes, Output: output}, nil
	case runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_GUEST_AGENT_FAILED.String(),
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED.String(),
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED.String(),
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_FENCED.String():
		reason, err := publicInfrastructureReason(session.InfrastructureReason)
		if err != nil || len(session.TerminalMessage) < 1 || len(session.TerminalMessage) > 2048 {
			return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
		}
		return contracts.ExecInfrastructureFailed{
			Kind: "infrastructure_failed", Reason: reason,
			Retryable: session.Retryable, Message: session.TerminalMessage,
		}, nil
	default:
		return nil, errors.New("SecondBox Exec terminal outcome is incomplete")
	}
}

func publicSpawnFailureReason(reason string) (string, error) {
	switch reason {
	case runnerv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND.String():
		return "not_found", nil
	case runnerv1.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED.String():
		return "permission_denied", nil
	case runnerv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD.String():
		return "invalid_cwd", nil
	case runnerv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE.String():
		return "malformed_executable", nil
	default:
		return "", errors.New("SecondBox Exec spawn failure reason is incomplete")
	}
}

func publicInfrastructureReason(reason string) (string, error) {
	switch reason {
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_TRANSPORT.String():
		return "transport", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_ADMISSION.String():
		return "admission", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GENERATION_FENCED.String():
		return "generation_fenced", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_LEASE_FENCED.String():
		return "lease_fenced", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT.String():
		return "guest_agent", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE.String():
		return "execution_node", nil
	case runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_SERVICE.String():
		return "service", nil
	default:
		return "", errors.New("SecondBox Exec infrastructure failure reason is incomplete")
	}
}

func fileTerminalError(session runnercontrol.DataPlaneSession) error {
	switch session.TerminalKind {
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED.String():
		return nil
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND.String():
		return ports.ErrSandboxNotFound
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_FENCED.String():
		return ports.ErrGenerationFenced
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED.String():
		return runnercontrol.ErrRelaySessionLimit
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED.String():
		return runnercontrol.ErrFilePermission
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH.String():
		return runnercontrol.ErrFileChecksum
	case runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String():
		return runnercontrol.ErrDataPlaneDeadline
	default:
		return fmt.Errorf("SecondBox File operation failed: %s", session.TerminalDetail)
	}
}

func publicFileStat(
	path string, kind runnerv1.FileKind, size uint64, modifiedAtUnixMilliseconds uint64,
) (contracts.FileStat, error) {
	publicKind := strings.ToLower(strings.TrimPrefix(kind.String(), "FILE_KIND_"))
	if publicKind != "file" && publicKind != "directory" && publicKind != "symbolic_link" {
		return contracts.FileStat{}, errors.New("SecondBox File metadata kind is invalid")
	}
	if modifiedAtUnixMilliseconds == 0 {
		return contracts.FileStat{}, errors.New("SecondBox File metadata timestamp is missing")
	}
	return contracts.FileStat{
		Path: path, Kind: publicKind, SizeBytes: int64(size),
		ModifiedAt: time.UnixMilli(int64(modifiedAtUnixMilliseconds)).UTC(),
	}, nil
}
