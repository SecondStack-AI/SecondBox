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
			return nil, runnercontrol.ErrLiveDataPlaneUnavailable
		}
		stream, err := service.liveDataPlane.Open(
			session.RunnerID, session.Kind, session.ID, session.StreamID,
		)
		if err != nil {
			return nil, err
		}
		if session.State == "pending" {
			if _, err := service.dataPlaneStore.StartDataPlaneSession(
				ctx, session.TenantRef, session.SubjectRef, session.ID, service.now().UTC(),
			); err != nil {
				stream.Close()
				return nil, err
			}
		}
		return &proxiedDataPlaneStream{stream: stream}, nil
	case contracts.DataPlaneTransportDirect:
		return service.openDirectDataPlaneStream(ctx, session)
	default:
		return nil, errors.New("SecondBox data-plane session transport is invalid")
	}
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
	defer stream.Close()
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: 1, Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.ExecFrame_Open{Open: proto.Clone(open).(*runnerv1.ExecOpen)},
		}},
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
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
		return nil, fmt.Errorf("SecondBox streaming Exec request decoding: %w", err)
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
		_ = stream.Close()
		return nil, err
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
	service         *ControlPlaneService
	session         runnercontrol.DataPlaneSession
	attachmentID    string
	stream          dataPlaneStream
	mu              sync.Mutex
	nextSend        int64
	nextRecv        int64
	request         int64
	credit          int64
	emitted         int64
	recordedThrough int64
	terminal        bool
	closed          bool
	detached        bool
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
		return nil, fmt.Errorf("SecondBox Terminal request decoding: %w", err)
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
			_ = stream.Close()
			return nil, err
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
		_ = stream.Close()
		return nil, err
	}
	message, err := stream.Receive(ctx)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	frame := message.GetPty()
	if frame == nil || frame.OperationId != session.ID || frame.StreamId != session.StreamID ||
		!proto.Equal(frame.Fence, dataPlaneFence(session)) ||
		!proto.Equal(frame.Correlation, dataPlaneCorrelation(session)) || frame.GetAttachResult() == nil {
		_ = stream.Close()
		return nil, errors.New("SecondBox Terminal attachment result is invalid")
	}
	switch frame.GetAttachResult().Kind {
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED:
		return result, nil
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_REPLAY_EVICTED:
		_ = stream.Close()
		return nil, runnercontrol.ErrTerminalReplayEvicted
	case runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ALREADY_ATTACHED:
		_ = stream.Close()
		return nil, runnercontrol.ErrTerminalAttached
	default:
		_ = stream.Close()
		return nil, errors.New("SecondBox Terminal attachment was rejected")
	}
}

func (stream *SandboxTerminalStream) Send(
	ctx context.Context,
	frame runnercontrol.TerminalClientFrame,
) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
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
	if frame.Credit > 0 && stream.credit-stream.emitted+frame.Credit > stream.session.StreamWindowBytes {
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
	store, err := stream.service.terminalStore()
	if err != nil {
		return err
	}
	if _, err := store.RecordTerminalClientFrame(
		context.WithoutCancel(ctx), stream.session.TenantRef, stream.session.SubjectRef,
		stream.session.ID, stream.attachmentID, frame, stream.service.now().UTC(),
	); err != nil {
		return err
	}
	stream.nextSend++
	stream.request += int64(len(frame.Input))
	stream.credit += frame.Credit
	if frame.Cancel {
		stream.closed = true
	}
	return nil
}

func (stream *SandboxTerminalStream) Receive(
	ctx context.Context,
) (runnercontrol.TerminalServerFrame, runnercontrol.DataPlaneSession, error) {
	message, err := stream.stream.Receive(ctx)
	if err != nil {
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, err
	}
	frame := message.GetPty()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if frame == nil || frame.OperationId != stream.session.ID ||
		frame.StreamId != stream.session.StreamID ||
		!proto.Equal(frame.Fence, dataPlaneFence(stream.session)) ||
		!proto.Equal(frame.Correlation, dataPlaneCorrelation(stream.session)) || frame.Sequence == 0 ||
		int64(frame.Sequence)-1 != stream.nextRecv || stream.terminal {
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
			!replayed && (stream.emitted+int64(len(frame.GetOutput().Data)) > stream.credit ||
				stream.emitted+int64(len(frame.GetOutput().Data)) > stream.session.MaximumResponseBytes) {
			return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, fmt.Errorf(
				"SecondBox live Terminal response credit: %w", runnercontrol.ErrDataPlaneSequence,
			)
		}
		result.Output = bytes.Clone(frame.GetOutput().Data)
	case frame.GetTerminal() != nil:
		result.Terminal = proto.Clone(frame.GetTerminal()).(*runnerv1.ExecTerminal)
		stream.terminal = true
	default:
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, errors.New("SecondBox Terminal response payload is invalid")
	}
	store, err := stream.service.terminalStore()
	if err != nil {
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, err
	}
	session, err := store.RecordTerminalServerFrame(
		context.WithoutCancel(ctx), stream.session.TenantRef, stream.session.SubjectRef,
		stream.session.ID, result, stream.service.now().UTC(),
	)
	if err != nil {
		return runnercontrol.TerminalServerFrame{}, runnercontrol.DataPlaneSession{}, fmt.Errorf(
			"SecondBox live Terminal response recording: %w", err,
		)
	}
	stream.nextRecv++
	stream.emitted = session.InboundBytes
	stream.recordedThrough = session.NextInboundSequence - 2
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
	return errors.Join(detachErr, stream.stream.Close())
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
	defer stream.Close()
	sequence := uint64(1)
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
			Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
			Sequence: sequence, Correlation: dataPlaneCorrelation(session),
			Payload: &runnerv1.FileFrame_Open{Open: proto.Clone(open).(*runnerv1.FileOpen)},
		}},
	}); err != nil {
		return runnercontrol.DataPlaneSession{}, err
	}
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
	if open.Operation == runnerv1.FileOperation_FILE_OPERATION_READ {
		sequence++
		if err := stream.Send(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
				Fence: dataPlaneFence(session), OperationId: session.ID, StreamId: session.StreamID,
				Sequence: sequence, Correlation: dataPlaneCorrelation(session),
				Payload: &runnerv1.FileFrame_Credit{Credit: &runnerv1.StreamCredit{
					ByteCount: uint64(session.MaximumResponseBytes),
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
