package microvmguest

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

func TestProtocolServiceBufferedExhaustionAcceptsCreditAfterProcessExit(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
	defer cleanup()
	sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "late-credit", 1), &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf 0123456789abcdefghijklmnop; sleep 30"},
		OutputLimitBytes: 16,
	})
	admission := receiveProtocolExec(t, stream).GetAdmission()
	pid, err := strconv.Atoi(admission.GetProcessId())
	if err != nil || pid <= 0 {
		t.Fatalf("guest process admission = %#v: %v", admission, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("output exhaustion did not terminate guest process: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
		Binding: protocolTestOperationBinding(binding, "late-credit", 2),
		Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 16}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if output := receiveProtocolExec(t, stream).GetOutput(); string(output.GetData()) != "0123456789abcdef" {
		t.Fatalf("buffered exhausted output = %#v", output)
	}
	if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		t.Fatalf("buffered exhausted terminal = %#v", terminal)
	}
}

func TestProtocolServiceFullExecCreditQueueUnblocksAfterDeadline(t *testing.T) {
	stream, binding, cleanup := openNegotiatedProtocolTestStream(t, t.TempDir())
	defer cleanup()
	sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "silent-exec", 1), &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 30"},
		OutputLimitBytes: 1024, Streaming: true,
		DeadlineUnixMs: uint64(time.Now().Add(500 * time.Millisecond).UnixMilli()),
	})
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("silent exec admission = %#v", admission)
	}
	// A silent command consumes none of these credits, filling the bounded queue.
	for sequence := uint64(2); sequence < 66; sequence++ {
		if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
			Binding: protocolTestOperationBinding(binding, "silent-exec", sequence),
			Payload: &guestv1.ExecFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 1}},
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "after-credit", 1), &guestv1.ExecRequest{
		Command: &guestv1.ExecRequest_Shell{Shell: "true"}, OutputLimitBytes: 1024,
	})
	if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED {
		t.Fatalf("silent exec terminal = %#v", terminal)
	}
	if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		t.Fatalf("exec after full credit queue admission = %#v", admission)
	}
	if terminal := receiveProtocolExec(t, stream).GetTerminal(); terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || terminal.GetExitCode() != 0 {
		t.Fatalf("exec after full credit queue terminal = %#v", terminal)
	}
}

func TestProtocolServiceExecInputAfterTerminationKeepsConnection(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		command          string
		streamBeforeExit bool
		deadline         bool
		gateExit         bool
		exitCode         int32
	}{
		{"completed", "true", false, false, false, 0},
		{"blocked_reader", "sleep 30", true, true, false, -1},
		{"early_reader_exit", "head -c 65536 > /dev/null", true, false, false, 0},
		{"closed_reader_still_running", "exec 0<&-; while [ ! -f release-exit ]; do sleep 0.01; done; exit 7", true, false, true, 7},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			workspace := t.TempDir()
			stream, binding, cleanup := openNegotiatedProtocolTestStream(t, workspace)
			defer cleanup()
			request := &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: scenario.command},
				OutputLimitBytes: 1024,
				Streaming:        true,
			}
			if scenario.deadline {
				request.DeadlineUnixMs = uint64(time.Now().Add(500 * time.Millisecond).UnixMilli())
			}
			sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "late-stdin", 1), request)
			if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
				t.Fatalf("admission = %#v", admission)
			}
			sent := make(chan error, 1)
			sendInput := func() {
				data := make([]byte, 64<<10)
				for sequence := uint64(2); sequence < 514; sequence++ {
					if err := stream.Send(&guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
						Binding: protocolTestOperationBinding(binding, "late-stdin", sequence),
						Payload: &guestv1.ExecFrame_Input{Input: &guestv1.ExecInput{Data: data, EndOfInput: sequence == 513}},
					}}}); err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}
			if scenario.streamBeforeExit {
				go sendInput()
			}
			waitInput := func() {
				t.Helper()
				select {
				case err := <-sent:
					if err != nil {
						t.Fatalf("in-flight input failed after reader stopped: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("stopped reader kept stdin backpressure blocked")
				}
			}
			if scenario.gateExit {
				waitInput()
				if err := os.WriteFile(filepath.Join(workspace, "release-exit"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			terminal := receiveProtocolExec(t, stream).GetTerminal()
			want := guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED
			if scenario.deadline {
				want = guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED
			}
			if terminal.GetKind() != want {
				t.Fatalf("terminal = %#v, want %v", terminal, want)
			}
			if terminal.GetExitCode() != scenario.exitCode {
				t.Fatalf("exit code = %d, want %d", terminal.GetExitCode(), scenario.exitCode)
			}
			if !scenario.streamBeforeExit {
				go sendInput()
			}
			if !scenario.gateExit {
				waitInput()
			}
			sendProtocolExecRequest(t, stream, protocolTestOperationBinding(binding, "after-stdin", 1), &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: "true"},
				OutputLimitBytes: 1024,
			})
			if admission := receiveProtocolExec(t, stream).GetAdmission(); admission.GetKind() != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
				t.Fatalf("next command admission = %#v", admission)
			}
			if result := receiveProtocolExec(t, stream).GetTerminal(); result.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || result.ExitCode != 0 {
				t.Fatalf("next command terminal = %#v", result)
			}
		})
	}
}
