package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/portdirect"
	"google.golang.org/protobuf/proto"
)

const liveDataPlaneChunkBytes = 64 << 10

const dataPlaneCredentialDomain = "secondbox/data-plane/v1\x00"

type dataPlaneStream interface {
	Send(*runnerv1.ControlPlaneToRunner) error
	Receive(context.Context) (*runnerv1.RunnerToControlPlane, error)
	Close() error
}

func (service *ControlPlaneService) openDataPlaneStream(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
) (dataPlaneStream, error) {
	switch session.Transport {
	case contracts.DataPlaneTransportProxied:
		if service.liveDataPlane == nil {
			return nil, service.abortDataPlaneSetup(
				ctx, session, nil, runnercontrol.ErrLiveDataPlaneUnavailable,
			)
		}
		responseCreditBytes := int64(0)
		replayedThrough := uint64(0)
		if session.Kind == "terminal" {
			responseCreditBytes = session.ResponseCreditBytes - session.InboundBytes
			if session.State != "pending" {
				// PostgreSQL may trail an attached Terminal by one checkpoint gap.
				// One window admits bounded replay; the Runner still enforces the
				// actual credit it received before the control-plane interruption.
				responseCreditBytes = session.StreamWindowBytes
			}
			if session.NextInboundSequence > 1 {
				replayedThrough = uint64(session.NextInboundSequence - 1)
			}
		}
		stream, err := service.liveDataPlane.Open(
			session.RunnerID, session.Kind, session.ID, session.StreamID,
			session.StreamWindowBytes, responseCreditBytes, replayedThrough,
		)
		if err != nil {
			return nil, service.abortDataPlaneSetup(ctx, session, nil, err)
		}
		proxied := &proxiedDataPlaneStream{stream: stream}
		if session.State == "pending" {
			if _, err := service.dataPlaneStore.StartDataPlaneSession(
				ctx, session.TenantRef, session.SubjectRef, session.ID, service.now().UTC(),
			); err != nil {
				return nil, service.abortDataPlaneSetup(ctx, session, proxied, err)
			}
		}
		return proxied, nil
	case contracts.DataPlaneTransportDirect:
		stream, err := service.openDirectDataPlaneStream(ctx, session)
		if err != nil {
			return nil, service.abortDataPlaneSetup(ctx, session, nil, err)
		}
		return stream, nil
	default:
		return nil, service.abortDataPlaneSetup(
			ctx, session, nil,
			errors.New("SecondBox data-plane session transport is invalid"),
		)
	}
}

func (service *ControlPlaneService) abortDataPlaneSetup(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
	stream dataPlaneStream,
	setupErr error,
) error {
	var closeErr error
	if stream != nil {
		closeErr = stream.Close()
	}
	if session.State != "pending" {
		// A running session has already reached the Runner, so an attachment
		// failure is not proof that the operation failed.
		return errors.Join(setupErr, closeErr)
	}
	_, cancelErr := service.dataPlaneStore.CancelDataPlaneSession(
		context.WithoutCancel(ctx), session.TenantRef, session.SubjectRef,
		session.ID, "data-plane setup failed", service.now().UTC(),
	)
	if cancelErr != nil {
		cancelErr = fmt.Errorf("SecondBox data-plane setup cancellation: %w", cancelErr)
	}
	return errors.Join(setupErr, closeErr, cancelErr)
}

type proxiedDataPlaneStream struct {
	stream *runnercontrol.LiveDataPlaneStream
}

func (stream *proxiedDataPlaneStream) Send(message *runnerv1.ControlPlaneToRunner) error {
	return stream.stream.Send(message)
}

func (stream *proxiedDataPlaneStream) Receive(ctx context.Context) (*runnerv1.RunnerToControlPlane, error) {
	return stream.stream.Receive(ctx)
}

func (stream *proxiedDataPlaneStream) Close() error {
	stream.stream.Close()
	return nil
}

type directDataPlaneStream struct {
	connection net.Conn
	writeMu    sync.Mutex
}

func (service *ControlPlaneService) openDirectDataPlaneStream(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
) (dataPlaneStream, error) {
	tlsConfig, err := portdirect.TLSConfigForSPKIPin(session.DataPlaneCertificateSPKI)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", session.DataPlaneAddress)
	if err != nil {
		return nil, fmt.Errorf("SecondBox direct data-plane dial: %w", err)
	}
	connection := tls.Client(raw, tlsConfig)
	if err := connection.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("SecondBox direct data-plane TLS handshake: %w", err)
	}
	if err := connection.SetDeadline(session.DeadlineAt); err != nil {
		_ = connection.Close()
		return nil, err
	}
	kind := portdirect.SessionKindExec
	switch session.Kind {
	case "file":
		kind = portdirect.SessionKindFile
	case "terminal":
		kind = portdirect.SessionKindPTY
	}
	if err := portdirect.WriteCredential(connection, kind, service.dataPlaneCredential(session.ID)); err != nil {
		_ = connection.Close()
		return nil, err
	}
	verdict, detail, err := portdirect.ReadVerdict(connection)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if verdict != portdirect.VerdictAdmitted {
		_ = connection.Close()
		return nil, fmt.Errorf("SecondBox direct data-plane admission denied: %s", detail)
	}
	return &directDataPlaneStream{connection: connection}, nil
}

func (stream *directDataPlaneStream) Send(message *runnerv1.ControlPlaneToRunner) error {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return fmt.Errorf("SecondBox direct data-plane message encoding: %w", err)
	}
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	return portdirect.WriteTypedMessage(stream.connection, payload)
}

func (stream *directDataPlaneStream) Receive(ctx context.Context) (*runnerv1.RunnerToControlPlane, error) {
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.connection.SetReadDeadline(time.Now())
		case <-finished:
		}
	}()
	defer close(finished)
	payload, err := portdirect.ReadTypedMessage(stream.connection)
	if err != nil {
		return nil, err
	}
	message := &runnerv1.RunnerToControlPlane{}
	if err := proto.Unmarshal(payload, message); err != nil {
		return nil, fmt.Errorf("SecondBox direct data-plane message decoding: %w", err)
	}
	return message, nil
}

func (stream *directDataPlaneStream) Close() error {
	return stream.connection.Close()
}

func (service *ControlPlaneService) dataPlaneCredential(sessionID string) string {
	mac := hmac.New(sha256.New, service.credentialSealSecret)
	_, _ = mac.Write([]byte(dataPlaneCredentialDomain))
	_, _ = mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (service *ControlPlaneService) dataPlaneCredentialDigest(sessionID string) []byte {
	digest := sha256.Sum256([]byte(service.dataPlaneCredential(sessionID)))
	return digest[:]
}

func (service *ControlPlaneService) ConsumeDirectDataPlane(
	ctx context.Context,
	input runnercontrol.DirectDataPlaneConsumption,
) error {
	expected := service.dataPlaneCredentialDigest(input.SessionID)
	if subtle.ConstantTimeCompare(expected, input.CredentialDigest) != 1 {
		return errors.New("SecondBox direct data-plane credential is invalid")
	}
	return service.dataPlaneStore.ConsumeDirectDataPlaneSession(ctx, input)
}

func dataPlaneFence(session runnercontrol.DataPlaneSession) *runnerv1.AssignmentFence {
	return &runnerv1.AssignmentFence{
		AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
		InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
		FencingToken: append([]byte(nil), session.FencingToken...),
	}
}

func dataPlaneCorrelation(session runnercontrol.DataPlaneSession) *runnerv1.Correlation {
	return &runnerv1.Correlation{
		RequestId: session.RequestID, OperationId: session.ID,
		SandboxId: session.SandboxID, InstanceId: session.InstanceID,
		SandboxGeneration: uint64(session.Generation), AssignmentId: session.AssignmentID,
		LeaseId: session.LeaseID, RunnerId: session.RunnerID,
	}
}

func dataPlaneDeadlineContext(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, session.DeadlineAt)
}

func (service *ControlPlaneService) executeBufferedDataPlane(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
	open *runnerv1.ExecOpen,
) (runnercontrol.DataPlaneSession, error) {
	operationCtx, cancel := dataPlaneDeadlineContext(ctx, session)
	defer cancel()
	stream, err := service.openDataPlaneStream(operationCtx, session)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: 1, Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.ExecFrame_Open{Open: proto.Clone(open).(*runnerv1.ExecOpen)},
		}},
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, service.abortDataPlaneSetup(
			ctx, session, stream, err,
		)
	}
	defer stream.Close()
	message, err := stream.Receive(operationCtx)
	if err != nil {
		if errors.Is(operationCtx.Err(), context.DeadlineExceeded) {
			return service.dataPlaneStore.ExpireDataPlaneSession(
				context.WithoutCancel(ctx), session.TenantRef, session.SubjectRef,
				session.ID, service.now().UTC(),
			)
		}
		return runnercontrol.DataPlaneSession{}, err
	}
	frame := message.GetExec()
	if frame == nil || frame.Sequence != 1 || frame.OperationId != session.ID ||
		frame.StreamId != session.StreamID || frame.GetBufferedResult() == nil {
		return runnercontrol.DataPlaneSession{}, errors.New("SecondBox buffered Exec completion message is invalid")
	}
	return service.dataPlaneStore.CompleteDataPlaneSession(context.WithoutCancel(ctx), runnercontrol.DataPlaneCompletion{
		TenantRef: session.TenantRef, SubjectRef: session.SubjectRef,
		SessionID: session.ID, Exec: frame.GetBufferedResult(), Now: service.now().UTC(),
	})
}

// SandboxExecStream forwards one public streaming Exec attachment without
// retaining payload bytes outside the bounded live transport.
type SandboxExecStream struct {
	service  *ControlPlaneService
	session  runnercontrol.DataPlaneSession
	stream   dataPlaneStream
	mu       sync.Mutex
	nextSend int64
	nextRecv uint64
	request  int64
	credit   int64
	emitted  int64
	closed   bool
	terminal bool
}

func (service *ControlPlaneService) OpenSandboxExecStream(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
) (*SandboxExecStream, error) {
	if session.Kind != "exec" || session.Operation != "exec-stream" ||
		(session.State != "pending" && session.State != "running") {
		return nil, runnercontrol.ErrDataPlaneNotFound
	}
	var request contracts.StreamingExecRequest
	if err := json.Unmarshal(session.RequestJSON, &request); err != nil {
		return nil, service.abortDataPlaneSetup(
			ctx, session, nil,
			fmt.Errorf("SecondBox streaming Exec request decoding: %w", err),
		)
	}
	open := publicExecOpen(
		request.Command, request.Cwd, request.Environment, session.DeadlineAt,
		session.MaximumResponseBytes, nil,
	)
	open.Streaming = true
	operationCtx, cancel := dataPlaneDeadlineContext(ctx, session)
	defer cancel()
	stream, err := service.openDataPlaneStream(operationCtx, session)
	if err != nil {
		return nil, err
	}
	result := &SandboxExecStream{
		service: service, session: session, stream: stream, nextRecv: 1,
	}
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: 1, Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.ExecFrame_Open{Open: open},
		}},
	}); err != nil {
		return nil, service.abortDataPlaneSetup(ctx, session, stream, err)
	}
	return result, nil
}

func (stream *SandboxExecStream) Send(
	ctx context.Context,
	frame runnercontrol.ExecClientFrame,
) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if frame.Sequence != stream.nextSend || stream.terminal {
		return runnercontrol.ErrDataPlaneSequence
	}
	isInput := frame.Input != nil || frame.EndInput
	selected := 0
	if isInput {
		selected++
	}
	if frame.Credit > 0 {
		selected++
	}
	if frame.Cancel {
		selected++
	}
	if selected != 1 {
		return errors.New("SecondBox public Exec frame requires exactly one payload")
	}
	if isInput {
		if stream.closed || len(frame.Input) == 0 && !frame.EndInput {
			return runnercontrol.ErrDataPlaneSequence
		}
		if stream.request+int64(len(frame.Input)) > stream.session.MaximumRequestBytes {
			return runnercontrol.ErrDataPlaneSessionLimit
		}
	}
	if frame.Credit > 0 && stream.credit-stream.emitted+frame.Credit > stream.session.StreamWindowBytes {
		return runnercontrol.ErrDataPlaneFrameLimit
	}
	runnerFrame := &runnerv1.ExecFrame{
		Fence: dataPlaneFence(stream.session), OperationId: stream.session.ID,
		StreamId: stream.session.StreamID, Sequence: uint64(frame.Sequence + 2),
		Correlation: dataPlaneCorrelation(stream.session),
	}
	switch {
	case isInput:
		runnerFrame.Payload = &runnerv1.ExecFrame_Input{Input: &runnerv1.ExecInput{
			Data: bytes.Clone(frame.Input), EndOfInput: frame.EndInput,
		}}
	case frame.Credit > 0:
		runnerFrame.Payload = &runnerv1.ExecFrame_Credit{Credit: &runnerv1.StreamCredit{
			ByteCount: uint64(frame.Credit),
		}}
	case frame.Cancel:
		runnerFrame.Payload = &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{
			Reason: "public streaming client cancellation",
		}}
	}
	sendErr := stream.stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: runnerFrame},
	})
	if sendErr == nil {
		stream.nextSend++
	}
	if isInput {
		if sendErr != nil {
			return sendErr
		}
		stream.request += int64(len(frame.Input))
		stream.closed = frame.EndInput
	}
	if frame.Credit > 0 {
		if sendErr != nil {
			return sendErr
		}
		stream.credit += frame.Credit
	}
	if frame.Cancel {
		return errors.Join(
			sendErr,
			stream.recordCancellation(ctx, "public streaming client cancellation"),
		)
	}
	return sendErr
}

func (stream *SandboxExecStream) Cancel(ctx context.Context, reason string) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.terminal {
		return nil
	}
	frame := &runnerv1.ExecFrame{
		Fence: dataPlaneFence(stream.session), OperationId: stream.session.ID,
		StreamId: stream.session.StreamID, Sequence: uint64(stream.nextSend + 2),
		Correlation: dataPlaneCorrelation(stream.session),
		Payload:     &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{Reason: reason}},
	}
	sendErr := stream.stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: frame},
	})
	if sendErr == nil {
		stream.nextSend++
	}
	return errors.Join(sendErr, stream.recordCancellation(ctx, reason))
}

func (stream *SandboxExecStream) recordCancellation(ctx context.Context, reason string) error {
	_, err := stream.service.dataPlaneStore.CancelDataPlaneSession(
		context.WithoutCancel(ctx), stream.session.TenantRef, stream.session.SubjectRef,
		stream.session.ID, reason, stream.service.now().UTC(),
	)
	return err
}

func (stream *SandboxExecStream) Receive(
	ctx context.Context,
) (runnercontrol.ExecServerFrame, runnercontrol.DataPlaneSession, error) {
	operationCtx, cancel := dataPlaneDeadlineContext(ctx, stream.session)
	defer cancel()
	message, err := stream.stream.Receive(operationCtx)
	if err != nil {
		if errors.Is(operationCtx.Err(), context.DeadlineExceeded) {
			session, expireErr := stream.service.dataPlaneStore.ExpireDataPlaneSession(
				context.WithoutCancel(ctx), stream.session.TenantRef, stream.session.SubjectRef,
				stream.session.ID, stream.service.now().UTC(),
			)
			if expireErr != nil {
				return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, expireErr
			}
			stream.mu.Lock()
			sequence := int64(stream.nextRecv - 1)
			stream.terminal = true
			stream.mu.Unlock()
			return runnercontrol.ExecServerFrame{Sequence: sequence, Terminal: &runnerv1.ExecTerminal{}}, session, nil
		}
		return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, err
	}
	frame := message.GetExec()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if frame == nil || frame.OperationId != stream.session.ID ||
		frame.StreamId != stream.session.StreamID || frame.Sequence != stream.nextRecv || stream.terminal {
		return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, runnercontrol.ErrDataPlaneSequence
	}
	result := runnercontrol.ExecServerFrame{Sequence: int64(frame.Sequence - 1)}
	switch {
	case frame.GetOutput() != nil:
		outputBytes := int64(len(frame.GetOutput().Data))
		if outputBytes == 0 || stream.emitted+outputBytes > stream.credit ||
			stream.emitted+outputBytes > stream.session.MaximumResponseBytes {
			return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, runnercontrol.ErrDataPlaneSessionLimit
		}
		stream.emitted += outputBytes
		result.Output = proto.Clone(frame.GetOutput()).(*runnerv1.ExecOutput)
	case frame.GetBufferedResult() != nil && frame.GetBufferedResult().Terminal != nil:
		stream.terminal = true
		completion := proto.Clone(frame.GetBufferedResult()).(*runnerv1.ExecBufferedResult)
		result.Terminal = proto.Clone(completion.Terminal).(*runnerv1.ExecTerminal)
		session, err := stream.service.dataPlaneStore.CompleteDataPlaneSession(
			context.WithoutCancel(ctx),
			runnercontrol.DataPlaneCompletion{
				TenantRef: stream.session.TenantRef, SubjectRef: stream.session.SubjectRef,
				SessionID: stream.session.ID,
				Exec:      completion,
				Now:       stream.service.now().UTC(),
			},
		)
		if err != nil {
			return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, err
		}
		stream.nextRecv++
		return result, session, nil
	default:
		return runnercontrol.ExecServerFrame{}, runnercontrol.DataPlaneSession{}, errors.New("SecondBox streaming Exec response payload is invalid")
	}
	stream.nextRecv++
	return result, runnercontrol.DataPlaneSession{}, nil
}

func (stream *SandboxExecStream) Close() error {
	return stream.stream.Close()
}

// SandboxTerminalStream forwards one exclusive Terminal attachment while the
// Runner owns bounded replay across attachment transports.
type SandboxTerminalStream struct {
	service          *ControlPlaneService
	session          runnercontrol.DataPlaneSession
	attachmentID     string
	stream           dataPlaneStream
	mu               sync.Mutex
	checkpointMu     sync.Mutex
	nextSend         int64
	nextRecv         int64
	request          int64
	credit           int64
	emitted          int64
	recordedThrough  int64
	replayAllowance  int64
	version          uint64
	persistedVersion uint64
	checkpointCancel context.CancelFunc
	checkpointDone   chan struct{}
	checkpointErr    error
	terminal         bool
	cancelled        bool
	closed           bool
	detached         bool
}

func (service *ControlPlaneService) OpenSandboxTerminalStream(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
	attachmentID string,
	afterSequence int64,
) (*SandboxTerminalStream, error) {
	if session.Kind != "terminal" || session.Operation != "terminal" ||
		(session.State != "pending" && session.State != "running") ||
		attachmentID == "" || afterSequence < -1 {
		return nil, runnercontrol.ErrDataPlaneNotFound
	}
	var request contracts.CreateTerminalRequest
	if err := json.Unmarshal(session.RequestJSON, &request); err != nil {
		return nil, service.abortDataPlaneSetup(
			ctx, session, nil,
			fmt.Errorf("SecondBox Terminal request decoding: %w", err),
		)
	}
	stream, err := service.openDataPlaneStream(ctx, session)
	if err != nil {
		return nil, err
	}
	result := &SandboxTerminalStream{
		service: service, session: session, attachmentID: attachmentID, stream: stream,
		nextSend: session.NextClientSequence, nextRecv: afterSequence + 1,
		request: session.RequestStreamBytes, credit: session.ResponseCreditBytes,
		emitted:         session.InboundBytes,
		recordedThrough: session.NextInboundSequence - 2,
	}
	if session.State != "pending" {
		// The durable outstanding value may be stale, so reattach treats fresh
		// client credit independently and reserves one window for credit the
		// Runner may already hold. This permits progress while bounding any
		// over-grant caused by the checkpoint gap to one stream window.
		result.replayAllowance = session.StreamWindowBytes
	}
	if session.State == "pending" {
		open := publicExecOpen(
			request.Command, request.Cwd, request.Environment, session.DeadlineAt,
			session.MaximumResponseBytes, nil,
		)
		open.AllocatePty = true
		open.PtyRows = uint32(request.Rows)
		open.PtyColumns = uint32(request.Columns)
		open.Streaming = true
		if err := stream.Send(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
				Sequence: 1, Correlation: dataPlaneCorrelation(session),
				Payload: &runnerv1.ExecFrame_Open{Open: open},
			}},
		}); err != nil {
			return nil, service.abortDataPlaneSetup(ctx, session, stream, err)
		}
	}
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: &runnerv1.PtyFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: uint64(result.nextSend + 2), Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.PtyFrame_Attach{Attach: &runnerv1.PtyAttach{
				ReconnectId: attachmentID, AfterSequence: afterSequence,
				StreamWindowBytes: uint64(session.StreamWindowBytes),
			}},
		}},
	}); err != nil {
		return nil, service.abortDataPlaneSetup(ctx, session, stream, err)
	}
	message, err := stream.Receive(ctx)
	if err != nil {
		return nil, service.abortDataPlaneSetup(ctx, session, stream, err)
	}
	frame := message.GetPty()
	if frame == nil || frame.OperationId != session.ID || frame.StreamId != session.StreamID ||
		!proto.Equal(frame.Fence, dataPlaneFence(session)) ||
		!proto.Equal(frame.Correlation, dataPlaneCorrelation(session)) || frame.GetAttachResult() == nil {
		return nil, service.abortDataPlaneSetup(
			ctx, session, stream,
			errors.New("SecondBox Terminal attachment result is invalid"),
		)
	}
	switch frame.GetAttachResult().Kind {
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED:
		nextSend, err := terminalAttachNextClientSequence(session, frame.GetAttachResult())
		if err != nil {
			return nil, service.abortDataPlaneSetup(ctx, session, stream, err)
		}
		result.nextSend = nextSend
		result.startCheckpointing()
		return result, nil
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_REPLAY_EVICTED:
		return nil, service.abortDataPlaneSetup(
			ctx, session, stream, runnercontrol.ErrTerminalReplayEvicted,
		)
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ALREADY_ATTACHED:
		return nil, service.abortDataPlaneSetup(
			ctx, session, stream, runnercontrol.ErrTerminalAttached,
		)
	default:
		return nil, service.abortDataPlaneSetup(
			ctx, session, stream,
			errors.New("SecondBox Terminal attachment was rejected"),
		)
	}
}

func terminalAttachNextClientSequence(
	session runnercontrol.DataPlaneSession,
	result *runnerv1.PtyAttachResult,
) (int64, error) {
	if result.NextInputSequence != nil {
		nextInputSequence := result.GetNextInputSequence()
		if nextInputSequence < 2 || nextInputSequence > uint64(^uint64(0)>>1) {
			return 0, errors.New("SecondBox Terminal attachment input sequence is invalid")
		}
		// A reconnect adopts the Runner's authoritative next input sequence.
		// Already-forwarded keystrokes are therefore never silently replayed;
		// the Runner's existing duplicate and gap checks cover later frames.
		return int64(nextInputSequence) - 2, nil
	}
	if session.State != "pending" {
		// Older Runners cannot prove the expected input sequence after a
		// checkpoint gap, so a running Terminal is conservatively non-resumable.
		return 0, runnercontrol.ErrTerminalReplayEvicted
	}
	return session.NextClientSequence, nil
}

// NextClientSequence returns the Runner-authoritative public input sequence
// selected by the attachment handshake.
func (stream *SandboxTerminalStream) NextClientSequence() int64 {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.nextSend
}

func (stream *SandboxTerminalStream) Send(
	ctx context.Context,
	frame runnercontrol.TerminalClientFrame,
) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.checkpointErr != nil {
		return stream.checkpointErr
	}
	if stream.closed || stream.terminal || frame.Sequence != stream.nextSend {
		return runnercontrol.ErrDataPlaneSequence
	}
	kinds := 0
	if frame.Input != nil {
		kinds++
	}
	if frame.ResizeRows != 0 || frame.ResizeColumns != 0 {
		kinds++
	}
	if frame.Credit != 0 {
		kinds++
	}
	if frame.Cancel {
		kinds++
	}
	if kinds != 1 || frame.Credit < 0 || frame.Input != nil && len(frame.Input) == 0 ||
		(frame.ResizeRows == 0) != (frame.ResizeColumns == 0) ||
		frame.ResizeRows > 1000 || frame.ResizeColumns > 1000 {
		return errors.New("SecondBox live Terminal frame requires exactly one valid payload")
	}
	if stream.request+int64(len(frame.Input)) > stream.session.MaximumRequestBytes {
		return runnercontrol.ErrDataPlaneSessionLimit
	}
	if frame.Credit > 0 && stream.credit-stream.emitted+frame.Credit >
		stream.session.StreamWindowBytes+stream.replayAllowance {
		return runnercontrol.ErrDataPlaneFrameLimit
	}
	sequence := uint64(frame.Sequence + 2)
	var message *runnerv1.ControlPlaneToRunner
	if frame.Cancel {
		message = &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: dataPlaneFence(stream.session), OperationId: stream.session.ID,
				StreamId: stream.session.StreamID, Sequence: sequence,
				Correlation: dataPlaneCorrelation(stream.session),
				Payload: &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{
					Reason: "public Terminal client cancellation",
				}},
			}},
		}
	} else {
		pty := &runnerv1.PtyFrame{
			Fence: dataPlaneFence(stream.session), OperationId: stream.session.ID,
			StreamId: stream.session.StreamID, Sequence: sequence,
			Correlation: dataPlaneCorrelation(stream.session),
		}
		switch {
		case frame.Input != nil:
			pty.Payload = &runnerv1.PtyFrame_Input{Input: &runnerv1.PtyInput{Data: bytes.Clone(frame.Input)}}
		case frame.Credit > 0:
			pty.Payload = &runnerv1.PtyFrame_Credit{Credit: &runnerv1.StreamCredit{ByteCount: uint64(frame.Credit)}}
		default:
			pty.Payload = &runnerv1.PtyFrame_Resize{Resize: &runnerv1.PtyResize{
				Rows: frame.ResizeRows, Columns: frame.ResizeColumns,
			}}
		}
		message = &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: pty},
		}
	}
	if err := stream.stream.Send(message); err != nil {
		return err
	}
	stream.nextSend++
	stream.request += int64(len(frame.Input))
	stream.credit += frame.Credit
	stream.version++
	if frame.Cancel {
		stream.cancelled = true
		stream.closed = true
	}
	return nil
}

func (stream *SandboxTerminalStream) Receive(
	ctx context.Context,
) (runnercontrol.TerminalServerFrame, runnercontrol.DataPlaneSession, error) {
	stream.mu.Lock()
	if stream.checkpointErr != nil {
		err := stream.checkpointErr
		stream.mu.Unlock()
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, err
	}
	stream.mu.Unlock()
	message, err := stream.stream.Receive(ctx)
	if err != nil {
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, err
	}
	frame := message.GetPty()
	stream.mu.Lock()
	if frame == nil || frame.OperationId != stream.session.ID ||
		frame.StreamId != stream.session.StreamID ||
		!proto.Equal(frame.Fence, dataPlaneFence(stream.session)) ||
		!proto.Equal(frame.Correlation, dataPlaneCorrelation(stream.session)) || frame.Sequence == 0 ||
		int64(frame.Sequence)-1 != stream.nextRecv || stream.terminal {
		stream.mu.Unlock()
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, fmt.Errorf(
			"SecondBox live Terminal response ordering: %w", runnercontrol.ErrDataPlaneSequence,
		)
	}
	result := runnercontrol.TerminalServerFrame{Sequence: stream.nextRecv}
	switch {
	case frame.GetOutput() != nil:
		replayed := stream.nextRecv <= stream.recordedThrough
		if frame.GetOutput().Channel != runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT ||
			len(frame.GetOutput().Data) == 0 ||
			!replayed && (stream.emitted+int64(len(frame.GetOutput().Data)) > stream.credit+stream.replayAllowance ||
				stream.emitted+int64(len(frame.GetOutput().Data)) > stream.session.MaximumResponseBytes) {
			stream.mu.Unlock()
			return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, fmt.Errorf(
				"SecondBox live Terminal response credit: %w", runnercontrol.ErrDataPlaneSequence,
			)
		}
		result.Output = bytes.Clone(frame.GetOutput().Data)
		if !replayed {
			stream.emitted += int64(len(result.Output))
		}
	case frame.GetTerminal() != nil:
		result.Terminal = proto.Clone(frame.GetTerminal()).(*runnerv1.ExecTerminal)
		stream.terminal = true
	default:
		stream.mu.Unlock()
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, errors.New("SecondBox Terminal response payload is invalid")
	}
	stream.nextRecv++
	stream.version++
	terminal := result.Terminal != nil
	session := stream.session
	stream.mu.Unlock()
	if terminal {
		updated, err := stream.checkpoint(context.WithoutCancel(ctx), result.Terminal)
		if err != nil {
			return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, fmt.Errorf(
				"SecondBox live Terminal outcome checkpoint: %w", err,
			)
		}
		session = updated
	}
	return result, session, nil
}

func (stream *SandboxTerminalStream) Close() error {
	if stream == nil || stream.stream == nil {
		return nil
	}
	stream.mu.Lock()
	if stream.detached {
		stream.mu.Unlock()
		return stream.stream.Close()
	}
	stream.closed = true
	stream.detached = true
	cancel := stream.checkpointCancel
	done := stream.checkpointDone
	stream.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	checkpointContext, stopCheckpoint := context.WithTimeout(context.Background(), 5*time.Second)
	_, checkpointErr := stream.checkpoint(checkpointContext, nil)
	stopCheckpoint()
	stream.mu.Lock()
	periodicErr := stream.checkpointErr
	detachErr := stream.stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: &runnerv1.PtyFrame{
			Fence: dataPlaneFence(stream.session), OperationId: stream.session.ID,
			StreamId: stream.session.StreamID, Sequence: uint64(stream.nextSend + 2),
			Correlation: dataPlaneCorrelation(stream.session),
			Payload: &runnerv1.PtyFrame_Detach{Detach: &runnerv1.PtyDetach{
				ReconnectId: stream.attachmentID,
			}},
		}},
	})
	stream.mu.Unlock()
	return errors.Join(periodicErr, checkpointErr, detachErr, stream.stream.Close())
}

func (stream *SandboxTerminalStream) startCheckpointing() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stream.mu.Lock()
	stream.checkpointCancel = cancel
	stream.checkpointDone = done
	stream.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(stream.service.dataPlanePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := stream.checkpoint(ctx, nil); err != nil {
					stream.mu.Lock()
					if stream.checkpointErr == nil {
						stream.checkpointErr = fmt.Errorf("SecondBox periodic Terminal checkpoint: %w", err)
					}
					stream.mu.Unlock()
					return
				}
			}
		}
	}()
}

func (stream *SandboxTerminalStream) checkpoint(
	ctx context.Context,
	terminal *runnerv1.ExecTerminal,
) (runnercontrol.DataPlaneSession, error) {
	stream.checkpointMu.Lock()
	defer stream.checkpointMu.Unlock()
	stream.mu.Lock()
	if terminal == nil && stream.version == stream.persistedVersion {
		session := stream.session
		stream.mu.Unlock()
		return session, nil
	}
	credit := max(stream.session.ResponseCreditBytes, stream.credit, stream.emitted)
	checkpoint := runnercontrol.TerminalCheckpoint{
		AttachmentID: stream.attachmentID, NextClientSequence: stream.nextSend,
		RequestBytes: stream.request, ResponseCredit: credit,
		InboundBytes: stream.emitted, NextInboundSequence: stream.nextRecv + 1,
		RecoveryAllowance: stream.replayAllowance,
		Cancel:            stream.cancelled, Terminal: terminal,
	}
	version := stream.version
	stream.mu.Unlock()
	store, err := stream.service.terminalStore()
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	updated, err := store.CheckpointTerminal(
		ctx, stream.session.TenantRef, stream.session.SubjectRef, stream.session.ID,
		checkpoint, stream.service.now().UTC(),
	)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	stream.mu.Lock()
	if version > stream.persistedVersion {
		stream.persistedVersion = version
	}
	stream.recordedThrough = max(stream.recordedThrough, checkpoint.NextInboundSequence-2)
	stream.session = updated
	stream.mu.Unlock()
	return updated, nil
}

func inputFileOpen(
	session runnercontrol.DataPlaneSession,
	operation runnerv1.FileOperation,
	path string,
	recursive bool,
	force bool,
	expectedSize int64,
	checksum string,
) *runnerv1.FileOpen {
	if operation == runnerv1.FileOperation_FILE_OPERATION_READ {
		expectedSize = session.MaximumResponseBytes
	}
	return &runnerv1.FileOpen{
		Operation: operation, WorkspaceRelativePath: path,
		ExpectedSize: uint64(expectedSize), ExpectedChecksum: checksum,
		Recursive: recursive, Force: force,
	}
}

func (service *ControlPlaneService) executeFileDataPlane(
	ctx context.Context,
	session runnercontrol.DataPlaneSession,
	open *runnerv1.FileOpen,
	content []byte,
) (runnercontrol.DataPlaneSession, error) {
	operationCtx, cancel := dataPlaneDeadlineContext(ctx, session)
	defer cancel()
	stream, err := service.openDataPlaneStream(operationCtx, session)
	if err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
	sequence := uint64(1)
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: sequence, Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.FileFrame_Open{Open: proto.Clone(open).(*runnerv1.FileOpen)},
		}},
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, service.abortDataPlaneSetup(
			ctx, session, stream, err,
		)
	}
	defer stream.Close()
	for offset := 0; offset < len(content); {
		sequence++
		size := min(liveDataPlaneChunkBytes, len(content)-offset)
		if err := stream.Send(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
				Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
				Sequence: sequence, Correlation: dataPlaneCorrelation(session),
				Payload: &runnerv1.FileFrame_Chunk{Chunk: &runnerv1.FileChunk{
					Offset: uint64(offset), Data: bytes.Clone(content[offset : offset+size]),
				}},
			}},
		}); err != nil {
			return runnercontrol.DataPlaneSession{}, err
		}
		offset += size
	}
	responseCreditGranted := int64(0)
	if open.Operation == runnerv1.FileOperation_FILE_OPERATION_READ {
		responseCreditGranted = min(session.MaximumResponseBytes, session.StreamWindowBytes)
		sequence++
		if err := stream.Send(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
				Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
				Sequence: sequence, Correlation: dataPlaneCorrelation(session),
				Payload: &runnerv1.FileFrame_Credit{Credit: &runnerv1.StreamCredit{
					ByteCount: uint64(responseCreditGranted),
				}},
			}},
		}); err != nil {
			return runnercontrol.DataPlaneSession{}, err
		}
	}
	result := &runnercontrol.FileCompletion{}
	nextSequence := uint64(1)
	for {
		message, err := stream.Receive(operationCtx)
		if err != nil {
			if errors.Is(operationCtx.Err(), context.DeadlineExceeded) {
				return service.dataPlaneStore.ExpireDataPlaneSession(
					context.WithoutCancel(ctx), session.TenantRef, session.SubjectRef,
					session.ID, service.now().UTC(),
				)
			}
			return runnercontrol.DataPlaneSession{}, err
		}
		frame := message.GetFile()
		if frame == nil || frame.OperationId != session.ID || frame.StreamId != session.StreamID ||
			frame.Sequence != nextSequence {
			return runnercontrol.DataPlaneSession{}, errors.New("SecondBox File completion sequence is invalid")
		}
		nextSequence++
		switch {
		case frame.GetMetadata() != nil:
			if result.Metadata != nil {
				return runnercontrol.DataPlaneSession{}, errors.New("SecondBox File completion repeated metadata")
			}
			result.Metadata = proto.Clone(frame.GetMetadata()).(*runnerv1.FileMetadata)
		case frame.GetChunk() != nil:
			if frame.GetChunk().Offset != uint64(len(result.Content)) ||
				int64(len(result.Content)+len(frame.GetChunk().Data)) > session.MaximumResponseBytes {
				return runnercontrol.DataPlaneSession{}, runnercontrol.ErrDataPlaneSessionLimit
			}
			result.Content = append(result.Content, frame.GetChunk().Data...)
			additionalCredit := min(
				int64(len(frame.GetChunk().Data)),
				session.MaximumResponseBytes-responseCreditGranted,
			)
			if additionalCredit > 0 {
				sequence++
				if err := stream.Send(&runnerv1.ControlPlaneToRunner{
					Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
						Fence: dataPlaneFence(session), OperationId: session.ID,
						StreamId: session.StreamID, Sequence: sequence,
						Correlation: dataPlaneCorrelation(session),
						Payload: &runnerv1.FileFrame_Credit{Credit: &runnerv1.StreamCredit{
							ByteCount: uint64(additionalCredit),
						}},
					}},
				}); err != nil {
					return runnercontrol.DataPlaneSession{}, err
				}
				responseCreditGranted += additionalCredit
			}
		case frame.GetTerminal() != nil:
			result.Terminal = proto.Clone(frame.GetTerminal()).(*runnerv1.FileTerminal)
			return service.dataPlaneStore.CompleteDataPlaneSession(context.WithoutCancel(ctx), runnercontrol.DataPlaneCompletion{
				TenantRef: session.TenantRef, SubjectRef: session.SubjectRef,
				SessionID: session.ID, File: result, Now: service.now().UTC(),
			})
		default:
			return runnercontrol.DataPlaneSession{}, errors.New("SecondBox File completion payload is invalid")
		}
	}
}

var _ runnercontrol.DirectDataPlaneAdmitter = (*ControlPlaneService)(nil)
