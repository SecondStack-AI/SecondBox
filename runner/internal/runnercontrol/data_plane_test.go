package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestRunnerDataPlaneExecCreditFenceOrderingAndCancellation(t *testing.T) {
	backend := &relayAssignmentBackend{
		exec: func(
			ctx context.Context,
			_ *runnerprotocol.AssignmentFence,
			open *runnerprotocol.ExecOpen,
		) (BufferedExecResult, error) {
			if open.GetShell() == "wait-for-cancel" {
				<-ctx.Done()
				return BufferedExecResult{}, ctx.Err()
			}
			if open.GetShell() == "output-exhausted" {
				return BufferedExecResult{
					Stdout: []byte("bounded-partial"),
					Terminal: &runnerprotocol.ExecTerminal{
						Kind:       runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED,
						ExitCode:   -1,
						SafeDetail: "command output limit exhausted",
					},
				}, nil
			}
			return BufferedExecResult{
				Stdout: []byte("stdout"),
				Stderr: []byte("err"),
				Terminal: &runnerprotocol.ExecTerminal{
					Kind:     runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
					ExitCode: 0,
				},
			}, nil
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
	}
	asyncErrors := make(chan error, 1)

	open := relayExecOpen(fence, "exec-1", "exec-stream-1", "printf ignored")
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if got := stream.messages(); len(got) != 0 {
		t.Fatalf("Exec emitted without byte credit: %#v", got)
	}
	if err := service.handleExecFrame(t.Context(), stream, relayExecCredit(fence, "exec-1", "exec-stream-1", 2, 2), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 1)
	first := stream.messages()[0].GetExec()
	if first.GetOutput() == nil || string(first.GetOutput().Data) != "st" {
		t.Fatalf("first credit-gated output = %#v", first)
	}
	if err := service.handleExecFrame(t.Context(), stream, relayExecCredit(fence, "exec-1", "exec-stream-1", 3, 64), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 4)
	messages := stream.messages()
	completion := messages[len(messages)-1].GetExec().GetBufferedResult()
	if completion.GetTerminal().GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(completion.Stdout) != "stdout" || string(completion.Stderr) != "err" {
		t.Fatalf("Exec completion = %#v", completion)
	}

	if err := service.handleExecFrame(t.Context(), stream, relayExecCredit(fence, "exec-1", "exec-stream-1", 3, 64), enabled, asyncErrors); err != nil {
		t.Fatalf("exact duplicate credit: %v", err)
	}
	if err := service.handleExecFrame(t.Context(), stream, relayExecCredit(fence, "exec-1", "exec-stream-1", 5, 1), enabled, asyncErrors); err == nil {
		t.Fatal("reordered Exec credit was accepted")
	}

	stale := cloneRunnerFence(fence)
	stale.SandboxGeneration++
	beforeStale := len(stream.messages())
	if err := service.handleExecFrame(t.Context(), stream, relayExecOpen(stale, "exec-stale", "exec-stale-stream", "true"), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, beforeStale+1)
	if got := stream.messages()[beforeStale].GetExec().GetBufferedResult().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_FENCED {
		t.Fatalf("stale Exec terminal = %v", got)
	}

	beforeCancel := len(stream.messages())
	cancelOpen := relayExecOpen(fence, "exec-cancel", "exec-cancel-stream", "wait-for-cancel")
	if err := service.handleExecFrame(t.Context(), stream, cancelOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	cancel := &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: "exec-cancel", StreamId: "exec-cancel-stream", Sequence: 2,
		Payload: &runnerprotocol.ExecFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{Reason: "test cancellation"}},
	}
	if err := service.handleExecFrame(t.Context(), stream, cancel, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, beforeCancel+1)
	if got := stream.messages()[beforeCancel].GetExec().GetBufferedResult().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("cancelled Exec terminal = %v", got)
	}

	beforeExhausted := len(stream.messages())
	exhaustedOpen := relayExecOpen(fence, "exec-exhausted", "exec-exhausted-stream", "output-exhausted")
	if err := service.handleExecFrame(t.Context(), stream, exhaustedOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if err := service.handleExecFrame(
		t.Context(), stream,
		relayExecCredit(fence, "exec-exhausted", "exec-exhausted-stream", 2, 1024),
		enabled, asyncErrors,
	); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, beforeExhausted+2)
	exhausted := stream.messages()[beforeExhausted:]
	if string(exhausted[0].GetExec().GetOutput().GetData()) != "bounded-partial" {
		t.Fatalf("output-exhausted partial output = %#v", exhausted[0].GetExec())
	}
	exhaustedCompletion := exhausted[1].GetExec().GetBufferedResult()
	if exhaustedCompletion.GetTerminal().GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED ||
		string(exhaustedCompletion.Stdout) != "bounded-partial" {
		t.Fatalf("output-exhausted completion = %#v", exhaustedCompletion)
	}

	slowOpen := relayExecOpen(fence, "exec-slow", "exec-slow-stream", "wait-for-cancel")
	if err := service.handleExecFrame(t.Context(), stream, slowOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	creditDone := make(chan error, 1)
	go func() {
		for sequence := uint64(2); sequence < 102; sequence++ {
			if err := service.handleExecFrame(
				t.Context(), stream,
				relayExecCredit(fence, "exec-slow", "exec-slow-stream", sequence, 1),
				enabled, asyncErrors,
			); err != nil {
				creditDone <- err
				return
			}
		}
		creditDone <- nil
	}()
	select {
	case err := <-creditDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("excess slow-client credit blocked the control receive path")
	}
	drain := &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Drain{Drain: &runnerprotocol.DrainCommand{
			MessageId: "drain-1", Sequence: 1,
			Mode: runnerprotocol.DrainMode_DRAIN_MODE_GRACEFUL,
		}},
	}
	if err := service.handleCommand(t.Context(), stream, drain, enabled, asyncErrors); err != nil {
		t.Fatalf("control command after excess credit: %v", err)
	}
	cancelSlow := &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: "exec-slow", StreamId: "exec-slow-stream", Sequence: 102,
		Payload: &runnerprotocol.ExecFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{Reason: "done"}},
	}
	if err := service.handleExecFrame(t.Context(), stream, cancelSlow, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDataPlanePTYRoutesBinaryControlOutputAndTypedTerminal(t *testing.T) {
	observedControls := make(chan PTYControl, 4)
	output := append(bytes.Repeat([]byte{0x00, 0x01, 0xfe, 0xff}, runnerDataPlaneChunkBytes/4), 0xaa, 0xbb)
	backend := &relayAssignmentBackend{
		pty: func(
			ctx context.Context,
			_ *runnerprotocol.AssignmentFence,
			open *runnerprotocol.ExecOpen,
			controls <-chan PTYControl,
			emit func([]byte) error,
		) (*runnerprotocol.ExecTerminal, error) {
			if open.GetShell() == "wait-for-cancel" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if !open.AllocatePty || open.PtyRows != 24 || open.PtyColumns != 80 {
				return nil, fmt.Errorf("PTY allocation = %#v", open)
			}
			for range 3 {
				select {
				case control := <-controls:
					observedControls <- control
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if err := emit(output); err != nil {
				return nil, err
			}
			return &runnerprotocol.ExecTerminal{
				Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED, ExitCode: 0,
			}, nil
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	asyncErrors := make(chan error, 1)

	open := relayExecOpen(fence, "terminal-1", "terminal-stream-1", "interactive")
	open.GetOpen().AllocatePty = true
	open.GetOpen().PtyRows = 24
	open.GetOpen().PtyColumns = 80
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(fence), OperationId: "terminal-1", StreamId: "terminal-stream-1", Sequence: 2,
		Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
			ReconnectId: "attachment-1", AfterSequence: -1,
			StreamWindowBytes: uint64(len(output)),
		}},
	}, enabled); err != nil {
		t.Fatal(err)
	}
	frames := []*runnerprotocol.PtyFrame{
		{
			Fence: cloneRunnerFence(fence), OperationId: "terminal-1", StreamId: "terminal-stream-1", Sequence: 2,
			Payload: &runnerprotocol.PtyFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: uint64(len(output))}},
		},
		{
			Fence: cloneRunnerFence(fence), OperationId: "terminal-1", StreamId: "terminal-stream-1", Sequence: 3,
			Payload: &runnerprotocol.PtyFrame_Resize{Resize: &runnerprotocol.PtyResize{Rows: 48, Columns: 160}},
		},
		{
			Fence: cloneRunnerFence(fence), OperationId: "terminal-1", StreamId: "terminal-stream-1", Sequence: 4,
			Payload: &runnerprotocol.PtyFrame_Input{Input: &runnerprotocol.PtyInput{Data: []byte{0x00, 0x01, 0xfe, 0xff}}},
		},
	}
	for _, frame := range frames {
		if err := service.handlePTYFrame(t.Context(), stream, frame, enabled); err != nil {
			t.Fatal(err)
		}
	}
	gotControls := []PTYControl{<-observedControls, <-observedControls, <-observedControls}
	if gotControls[0].Credit != uint64(len(output)) {
		t.Fatalf("PTY credit = %#v", gotControls[0])
	}
	if gotControls[1].Rows != 48 || gotControls[1].Columns != 160 {
		t.Fatalf("PTY resize = %#v", gotControls[1])
	}
	if !bytes.Equal(gotControls[2].Input, []byte{0x00, 0x01, 0xfe, 0xff}) {
		t.Fatalf("PTY input = %x", gotControls[2].Input)
	}

	waitRunnerMessages(t, stream, 4)
	messages := stream.messages()
	if messages[0].GetPty().GetAttachResult().GetKind() != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED ||
		len(messages[1].GetPty().GetOutput().GetData()) != runnerDataPlaneChunkBytes ||
		!bytes.Equal(messages[2].GetPty().GetOutput().GetData(), []byte{0xaa, 0xbb}) {
		t.Fatalf("PTY attachment and chunked output = %#v", messages[:3])
	}
	if got := messages[3].GetPty().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("PTY terminal = %v", got)
	}
	if err := service.handlePTYFrame(t.Context(), stream, frames[2], enabled); err != nil {
		t.Fatalf("exact duplicate PTY input: %v", err)
	}
	waitRunnerMessages(t, stream, 5)
	if stream.messages()[4].GetPty().GetTerminal() == nil {
		t.Fatal("terminal PTY duplicate did not replay its typed terminal")
	}

	stale := cloneRunnerFence(fence)
	stale.SandboxGeneration++
	staleOpen := relayExecOpen(stale, "terminal-stale", "terminal-stale-stream", "interactive")
	staleOpen.GetOpen().AllocatePty = true
	staleOpen.GetOpen().PtyRows = 24
	staleOpen.GetOpen().PtyColumns = 80
	if err := service.handleExecFrame(t.Context(), stream, staleOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(stale), OperationId: "terminal-stale", StreamId: "terminal-stale-stream", Sequence: 2,
		Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
			ReconnectId: "attachment-stale", AfterSequence: -1, StreamWindowBytes: 4096,
		}},
	}, enabled); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 7)
	if got := stream.messages()[6].GetPty().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_FENCED {
		t.Fatalf("stale PTY terminal = %v", got)
	}

	cancelOpen := relayExecOpen(fence, "terminal-cancel", "terminal-cancel-stream", "wait-for-cancel")
	cancelOpen.GetOpen().AllocatePty = true
	cancelOpen.GetOpen().PtyRows = 24
	cancelOpen.GetOpen().PtyColumns = 80
	if err := service.handleExecFrame(t.Context(), stream, cancelOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(fence), OperationId: "terminal-cancel", StreamId: "terminal-cancel-stream", Sequence: 2,
		Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
			ReconnectId: "attachment-cancel", AfterSequence: -1, StreamWindowBytes: 4096,
		}},
	}, enabled); err != nil {
		t.Fatal(err)
	}
	cancel := &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: "terminal-cancel", StreamId: "terminal-cancel-stream", Sequence: 2,
		Payload: &runnerprotocol.ExecFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{Reason: "test cancellation"}},
	}
	if err := service.handleExecFrame(t.Context(), stream, cancel, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 9)
	if got := stream.messages()[8].GetPty().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("cancelled PTY terminal = %v", got)
	}
}

func TestRunnerDataPlanePTYReplayRingDetachEvictionAndExclusiveAttachment(t *testing.T) {
	emitters := make(chan func([]byte) error, 1)
	backend := &relayAssignmentBackend{
		pty: func(
			ctx context.Context,
			_ *runnerprotocol.AssignmentFence,
			_ *runnerprotocol.ExecOpen,
			_ <-chan PTYControl,
			emit func([]byte) error,
		) (*runnerprotocol.ExecTerminal, error) {
			emitters <- emit
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	service := newRelayRunnerService(t, backend)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	asyncErrors := make(chan error, 1)
	first := &threadSafeRunnerStream{}
	open := relayExecOpen(fence, "terminal-replay", "terminal-replay-stream", "interactive")
	open.GetOpen().AllocatePty = true
	open.GetOpen().PtyRows = 24
	open.GetOpen().PtyColumns = 80
	if err := service.handleExecFrame(t.Context(), first, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	emit := <-emitters
	attach := func(stream RunnerProtocolStream, reconnectID string, afterSequence int64) error {
		return service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "terminal-replay",
			StreamId: "terminal-replay-stream", Sequence: 2,
			Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
				ReconnectId: reconnectID, AfterSequence: afterSequence, StreamWindowBytes: 6,
			}},
		}, enabled)
	}
	detach := func(stream RunnerProtocolStream, reconnectID string) error {
		return service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "terminal-replay",
			StreamId: "terminal-replay-stream", Sequence: 2,
			Payload: &runnerprotocol.PtyFrame_Detach{Detach: &runnerprotocol.PtyDetach{
				ReconnectId: reconnectID,
			}},
		}, enabled)
	}
	if err := attach(first, "attachment-first", -1); err != nil {
		t.Fatal(err)
	}
	if err := emit([]byte("aa")); err != nil {
		t.Fatal(err)
	}
	service.detachPTYAttachmentsForStream(first)
	if err := emit([]byte("bb")); err != nil {
		t.Fatal(err)
	}
	if err := emit([]byte("cc")); err != nil {
		t.Fatal(err)
	}

	second := &threadSafeRunnerStream{}
	if err := attach(second, "attachment-second", 0); err != nil {
		t.Fatal(err)
	}
	secondMessages := second.messages()
	if len(secondMessages) != 3 ||
		secondMessages[0].GetPty().GetAttachResult().GetKind() != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED ||
		secondMessages[1].GetPty().GetSequence() != 2 || string(secondMessages[1].GetPty().GetOutput().GetData()) != "bb" ||
		secondMessages[2].GetPty().GetSequence() != 3 || string(secondMessages[2].GetPty().GetOutput().GetData()) != "cc" {
		t.Fatalf("reattached PTY replay = %#v", secondMessages)
	}
	parallel := &threadSafeRunnerStream{}
	if err := attach(parallel, "attachment-parallel", 2); err != nil {
		t.Fatal(err)
	}
	if got := parallel.messages()[0].GetPty().GetAttachResult().GetKind(); got != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ALREADY_ATTACHED {
		t.Fatalf("parallel PTY attachment result = %s", got)
	}
	if err := detach(second, "attachment-second"); err != nil {
		t.Fatal(err)
	}
	if err := emit([]byte("dddd")); err != nil {
		t.Fatal(err)
	}
	evicted := &threadSafeRunnerStream{}
	if err := attach(evicted, "attachment-evicted", 0); err != nil {
		t.Fatal(err)
	}
	if got := evicted.messages()[0].GetPty().GetAttachResult().GetKind(); got != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_REPLAY_EVICTED {
		t.Fatalf("evicted PTY attachment result = %s", got)
	}
	service.operationMu.Lock()
	service.execOperations[runnerDataPlaneOperationKey(
		fence, "terminal-replay", "terminal-replay-stream",
	)].cancel(context.Canceled)
	service.operationMu.Unlock()
}

func TestRunnerDataPlanePTYDeadlineProducesTypedTerminal(t *testing.T) {
	backend := &relayAssignmentBackend{
		pty: func(
			ctx context.Context,
			_ *runnerprotocol.AssignmentFence,
			_ *runnerprotocol.ExecOpen,
			_ <-chan PTYControl,
			_ func([]byte) error,
		) (*runnerprotocol.ExecTerminal, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	service := newRelayRunnerService(t, backend)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	stream := &threadSafeRunnerStream{}
	open := relayExecOpen(fence, "terminal-deadline", "terminal-deadline-stream", "sleep")
	open.GetOpen().AllocatePty = true
	open.GetOpen().PtyRows = 24
	open.GetOpen().PtyColumns = 80
	open.GetOpen().DeadlineUnixMs = uint64(time.Now().Add(20 * time.Millisecond).UnixMilli())
	asyncErrors := make(chan error, 1)
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(fence), OperationId: "terminal-deadline",
		StreamId: "terminal-deadline-stream", Sequence: 2,
		Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
			ReconnectId: "attachment-deadline", AfterSequence: -1, StreamWindowBytes: 64,
		}},
	}, enabled); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 2)
	if got := stream.messages()[1].GetPty().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED {
		t.Fatalf("deadline PTY terminal = %v", got)
	}
}

func TestRunnerDataPlaneFileBinaryWriteReadAndTypedOutcomes(t *testing.T) {
	var written []byte
	backend := &relayAssignmentBackend{
		file: func(
			_ context.Context,
			_ *runnerprotocol.AssignmentFence,
			open *runnerprotocol.FileOpen,
			content []byte,
		) (FileOperationResult, error) {
			switch open.Operation {
			case runnerprotocol.FileOperation_FILE_OPERATION_WRITE:
				written = bytes.Clone(content)
				return FileOperationResult{Terminal: &runnerprotocol.FileTerminal{
					Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
				}}, nil
			case runnerprotocol.FileOperation_FILE_OPERATION_READ:
				return FileOperationResult{
					Metadata: &runnerprotocol.FileMetadata{
						Exists: true,
						Size:   uint64(len(written)),
						Mode:   0o600,
					},
					Content: bytes.Clone(written),
					Terminal: &runnerprotocol.FileTerminal{
						Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
					},
				}, nil
			default:
				return FileOperationResult{Terminal: &runnerprotocol.FileTerminal{
					Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND,
				}}, nil
			}
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true,
	}
	asyncErrors := make(chan error, 1)
	content := []byte{0, 1, 0xff, 'x'}
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	writeOpen := &runnerprotocol.FileFrame{
		Fence: cloneRunnerFence(fence), OperationId: "write-1", StreamId: "write-stream-1", Sequence: 1,
		Correlation: relayOperationCorrelation(fence, "write-1", "request-write", "lease-write"),
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
			Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "binary.dat",
			ExpectedSize: uint64(len(content)), ExpectedChecksum: checksum,
		}},
	}
	if err := service.handleFileFrame(t.Context(), stream, writeOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	writeChunk := &runnerprotocol.FileFrame{
		Fence: cloneRunnerFence(fence), OperationId: "write-1", StreamId: "write-stream-1", Sequence: 2,
		Payload: &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Offset: 0, Data: content}},
	}
	if err := service.handleFileFrame(t.Context(), stream, writeChunk, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 1)
	if !bytes.Equal(written, content) {
		t.Fatalf("written binary content = %v, want %v", written, content)
	}
	if got := stream.messages()[0].GetFile().GetTerminal().GetKind(); got != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("write terminal = %v", got)
	}

	readOpen := &runnerprotocol.FileFrame{
		Fence: cloneRunnerFence(fence), OperationId: "read-1", StreamId: "read-stream-1", Sequence: 1,
		Correlation: relayOperationCorrelation(fence, "read-1", "request-read", "lease-read"),
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
			Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "binary.dat",
			ExpectedSize: 1024,
		}},
	}
	if err := service.handleFileFrame(t.Context(), stream, readOpen, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 2)
	if stream.messages()[1].GetFile().GetMetadata() == nil {
		t.Fatal("read metadata was not emitted before content")
	}
	credit := &runnerprotocol.FileFrame{
		Fence: cloneRunnerFence(fence), OperationId: "read-1", StreamId: "read-stream-1", Sequence: 2,
		Payload: &runnerprotocol.FileFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 1024}},
	}
	if err := service.handleFileFrame(t.Context(), stream, credit, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 4)
	messages := stream.messages()
	if !bytes.Equal(messages[2].GetFile().GetChunk().GetData(), content) {
		t.Fatalf("read binary chunk = %v, want %v", messages[2].GetFile().GetChunk().GetData(), content)
	}
	if got := messages[3].GetFile().GetTerminal().GetKind(); got != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("read terminal = %v", got)
	}
}

func TestRunnerDataPlaneTerminalTombstonesAreBoundedAndReplayDuplicates(t *testing.T) {
	backend := &relayAssignmentBackend{
		exec: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
			t.Fatal("stale fenced operation reached backend")
			return BufferedExecResult{}, nil
		},
		file: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte) (FileOperationResult, error) {
			return FileOperationResult{}, nil
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
	}
	asyncErrors := make(chan error, 1)
	stale := relayRunnerFence()
	var last *runnerprotocol.ExecFrame
	for index := 0; index < maxRunnerDataPlaneTerminalTombstones+32; index++ {
		last = relayExecOpen(
			stale,
			fmt.Sprintf("stale-%d", index),
			fmt.Sprintf("stale-stream-%d", index),
			"true",
		)
		if err := service.handleExecFrame(t.Context(), stream, last, enabled, asyncErrors); err != nil {
			t.Fatal(err)
		}
	}
	service.operationMu.Lock()
	retained := len(service.execOperations)
	service.operationMu.Unlock()
	if retained > maxRunnerDataPlaneTerminalTombstones {
		t.Fatalf("retained Exec tombstones = %d, maximum %d", retained, maxRunnerDataPlaneTerminalTombstones)
	}
	beforeReplay := len(stream.messages())
	if err := service.handleExecFrame(t.Context(), stream, last, enabled, asyncErrors); err != nil {
		t.Fatalf("terminal duplicate replay: %v", err)
	}
	waitRunnerMessages(t, stream, beforeReplay+1)
	if got := stream.messages()[beforeReplay].GetExec().GetBufferedResult().GetTerminal().GetKind(); got != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_FENCED {
		t.Fatalf("replayed terminal = %v", got)
	}
}

func TestRunnerDataPlaneRejectsMissingOrInconsistentOperationCorrelation(t *testing.T) {
	service := newRelayRunnerService(t, &relayAssignmentBackend{})
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	asyncErrors := make(chan error, 1)

	execOpen := relayExecOpen(fence, "exec-correlation", "exec-correlation-stream", "true")
	execOpen.Correlation = nil
	if err := service.handleExecFrame(
		t.Context(),
		stream,
		execOpen,
		map[runnerprotocol.RunnerFeature]bool{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		},
		asyncErrors,
	); err == nil || !strings.Contains(err.Error(), "operation correlation") {
		t.Fatalf("missing Exec correlation error = %v", err)
	}

	fileOpen := &runnerprotocol.FileFrame{
		Fence:       cloneRunnerFence(fence),
		OperationId: "file-correlation",
		StreamId:    "file-correlation-stream",
		Sequence:    1,
		Correlation: relayOperationCorrelation(fence, "different-operation", "request-file", "lease-file"),
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
			Operation:             runnerprotocol.FileOperation_FILE_OPERATION_EXISTS,
			WorkspaceRelativePath: "item",
		}},
	}
	if err := service.handleFileFrame(
		t.Context(),
		stream,
		fileOpen,
		map[runnerprotocol.RunnerFeature]bool{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true,
		},
		asyncErrors,
	); err == nil || !strings.Contains(err.Error(), "operation correlation") {
		t.Fatalf("inconsistent File correlation error = %v", err)
	}
}

func TestRunnerDataPlaneForwardsLiveExecInputAndBackpressuresOutput(t *testing.T) {
	inputObserved := make(chan []byte, 1)
	backend := &relayAssignmentBackend{
		streaming: func(
			ctx context.Context,
			_ *runnerprotocol.AssignmentFence,
			open *runnerprotocol.ExecOpen,
			controls <-chan ExecControl,
			emit func(runnerprotocol.ExecOutputChannel, []byte) error,
		) (*runnerprotocol.ExecTerminal, error) {
			if !open.Streaming {
				return nil, errors.New("streaming Open was not retained")
			}
			var input []byte
			for {
				select {
				case control := <-controls:
					if control.Input == nil {
						continue
					}
					input = append(input, control.Input.Data...)
					if !control.Input.EndOfInput {
						continue
					}
					inputObserved <- bytes.Clone(input)
					if err := emit(
						runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
						append([]byte("echo:"), input...),
					); err != nil {
						return nil, err
					}
					return &runnerprotocol.ExecTerminal{
						Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
					}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
	}
	asyncErrors := make(chan error, 1)
	open := relayExecOpen(fence, "exec-live", "stream-live", "read value")
	open.GetOpen().Streaming = true
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	input := &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: "exec-live", StreamId: "stream-live", Sequence: 2,
		Payload: &runnerprotocol.ExecFrame_Input{Input: &runnerprotocol.ExecInput{Data: []byte("hello")}},
	}
	if err := service.handleExecFrame(t.Context(), stream, input, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-inputObserved:
		t.Fatalf("Runner completed EOF-dependent input before EOF: %q", observed)
	case <-time.After(10 * time.Millisecond):
	}
	endInput := &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: "exec-live", StreamId: "stream-live", Sequence: 3,
		Payload: &runnerprotocol.ExecFrame_Input{Input: &runnerprotocol.ExecInput{EndOfInput: true}},
	}
	if err := service.handleExecFrame(t.Context(), stream, endInput, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-inputObserved:
		if string(observed) != "hello" {
			t.Fatalf("Runner forwarded input = %q", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("Runner did not forward live Exec input")
	}
	time.Sleep(10 * time.Millisecond)
	if got := stream.messages(); len(got) != 0 {
		t.Fatalf("Runner emitted live output without credit: %#v", got)
	}
	if err := service.handleExecFrame(
		t.Context(), stream,
		relayExecCredit(fence, "exec-live", "stream-live", 4, 10),
		enabled, asyncErrors,
	); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 2)
	messages := stream.messages()
	if string(messages[0].GetExec().GetOutput().Data) != "echo:hello" ||
		messages[1].GetExec().GetBufferedResult().GetTerminal().GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(messages[1].GetExec().GetBufferedResult().Stdout) != "echo:hello" {
		t.Fatalf("Runner live output = %#v", messages)
	}
}

type relayAssignmentBackend struct {
	exec      func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen) (BufferedExecResult, error)
	streaming func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen, <-chan ExecControl, func(runnerprotocol.ExecOutputChannel, []byte) error) (*runnerprotocol.ExecTerminal, error)
	pty       func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen, <-chan PTYControl, func([]byte) error) (*runnerprotocol.ExecTerminal, error)
	file      func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte) (FileOperationResult, error)
}

func (*relayAssignmentBackend) Readiness(context.Context) (BackendReadiness, error) {
	return BackendReadiness{}, nil
}

func (*relayAssignmentBackend) ValidateAssignment(context.Context, *runnerprotocol.AssignmentCommand) error {
	return nil
}

func (*relayAssignmentBackend) StartAssignment(
	context.Context,
	*runnerprotocol.AssignmentCommand,
	func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	return BackendInstance{}, nil
}

func (*relayAssignmentBackend) FenceAssignment(context.Context, *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	return FenceEvidence{}, nil
}

func (backend *relayAssignmentBackend) ExecuteBuffered(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
) (BufferedExecResult, error) {
	return backend.exec(ctx, fence, open)
}

func (backend *relayAssignmentBackend) ExecuteStreaming(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan ExecControl,
	emit func(runnerprotocol.ExecOutputChannel, []byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	if backend.streaming != nil {
		return backend.streaming(ctx, fence, open, controls, emit)
	}
	result, err := backend.ExecuteBuffered(ctx, fence, open)
	if err != nil {
		return nil, err
	}
	if len(result.Stdout) > 0 {
		if err := emit(runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, result.Stdout); err != nil {
			return nil, err
		}
	}
	if len(result.Stderr) > 0 {
		if err := emit(runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR, result.Stderr); err != nil {
			return nil, err
		}
	}
	return result.Terminal, nil
}

func (backend *relayAssignmentBackend) ExecuteFile(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.FileOpen,
	content []byte,
) (FileOperationResult, error) {
	return backend.file(ctx, fence, open, content)
}

func (backend *relayAssignmentBackend) ExecutePTY(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan PTYControl,
	emit func([]byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	return backend.pty(ctx, fence, open, controls, emit)
}

func newRelayRunnerService(t *testing.T, backend *relayAssignmentBackend) *RunnerProtocolService {
	t.Helper()
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{
		stream: &threadSafeRunnerStream{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type threadSafeRunnerStream struct {
	mu       sync.Mutex
	outbound []*runnerprotocol.RunnerToControlPlane
}

func (stream *threadSafeRunnerStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	stream.mu.Lock()
	stream.outbound = append(stream.outbound, message)
	stream.mu.Unlock()
	return nil
}

func (*threadSafeRunnerStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	return nil, io.EOF
}

func (stream *threadSafeRunnerStream) messages() []*runnerprotocol.RunnerToControlPlane {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return append([]*runnerprotocol.RunnerToControlPlane(nil), stream.outbound...)
}

func waitRunnerMessages(t *testing.T, stream *threadSafeRunnerStream, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(stream.messages()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runner outbound message count = %d, want at least %d", len(stream.messages()), count)
}

func relayRunnerFence() *runnerprotocol.AssignmentFence {
	return &runnerprotocol.AssignmentFence{
		AssignmentId:      "assignment-1",
		SandboxId:         "sandbox-1",
		InstanceId:        "instance-1",
		SandboxGeneration: 3,
		FencingToken:      []byte("opaque-fence-token"),
	}
}

func relayExecOpen(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
	shell string,
) *runnerprotocol.ExecFrame {
	return &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: operationID, StreamId: streamID, Sequence: 1,
		Correlation: relayOperationCorrelation(fence, operationID, "request-"+operationID, "lease-"+operationID),
		Payload: &runnerprotocol.ExecFrame_Open{Open: &runnerprotocol.ExecOpen{
			Command:          &runnerprotocol.ExecOpen_Shell{Shell: shell},
			OutputLimitBytes: 1024,
			Streaming:        true,
		}},
	}
}

func relayOperationCorrelation(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	requestID string,
	leaseID string,
) *runnerprotocol.Correlation {
	return &runnerprotocol.Correlation{
		RequestId:         requestID,
		OperationId:       operationID,
		SandboxId:         fence.SandboxId,
		InstanceId:        fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		AssignmentId:      fence.AssignmentId,
		LeaseId:           leaseID,
		RunnerId:          "runner-1",
	}
}

func relayExecCredit(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
	credit uint64,
) *runnerprotocol.ExecFrame {
	return &runnerprotocol.ExecFrame{
		Fence: cloneRunnerFence(fence), OperationId: operationID, StreamId: streamID, Sequence: sequence,
		Payload: &runnerprotocol.ExecFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: credit}},
	}
}
