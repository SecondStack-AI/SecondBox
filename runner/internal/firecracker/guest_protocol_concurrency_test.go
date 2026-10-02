package firecracker

import (
	"context"
	"fmt"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

// TestGuestOperationsDoNotWaitForARunningCommand proves that a long Exec or an
// open Terminal does not hold up the Sandbox's other operations: each one
// completes against the real guest agent while the command is still running.
func TestGuestOperationsDoNotWaitForARunningCommand(t *testing.T) {
	socketPath, _, _ := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	session := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer session.Close()

	for _, running := range []struct {
		name  string
		start func(context.Context) error
	}{
		{"buffered exec", func(ctx context.Context) error {
			_, err := session.ExecuteBuffered(ctx, "assignment-1", &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 20"},
				OutputLimitBytes: 1024,
			}, nil)
			return err
		}},
		{"terminal", func(ctx context.Context) error {
			_, err := session.ExecutePTY(ctx, "assignment-1", &guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 20"},
				Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
				Streaming:        true,
				OutputLimitBytes: 1024,
			}, make(chan GuestPTYControl), func([]byte) error { return nil })
			return err
		}},
	} {
		t.Run(running.name, func(t *testing.T) {
			runningContext, stopRunning := context.WithCancel(ctx)
			runningDone := make(chan error, 1)
			go func() { runningDone <- running.start(runningContext) }()
			defer func() {
				stopRunning()
				select {
				case <-runningDone:
				case <-time.After(20 * time.Second):
					t.Error("the running command did not end after cancellation")
				}
			}()
			// Let the running command reach the guest first.
			time.Sleep(200 * time.Millisecond)

			besideDone := make(chan error, 1)
			go func() {
				mkdir, err := session.ExecuteFileOperation(ctx, "assignment-1", &guestv1.FileRequest{
					Operation:             guestv1.FileOperation_FILE_OPERATION_MKDIR,
					WorkspaceRelativePath: "beside/" + t.Name(),
					Recursive:             true,
				}, nil)
				if err == nil && mkdir.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
					err = fmt.Errorf("mkdir terminal = %+v", mkdir.Terminal)
				}
				if err != nil {
					besideDone <- err
					return
				}
				exec, err := session.ExecuteBuffered(ctx, "assignment-1", &guestv1.ExecRequest{
					Command:          &guestv1.ExecRequest_Shell{Shell: "printf beside"},
					OutputLimitBytes: 1024,
				}, nil)
				if err == nil && (exec.Terminal.GetExitCode() != 0 || string(exec.Stdout) != "beside") {
					err = fmt.Errorf("exec = %+v %q", exec.Terminal, exec.Stdout)
				}
				besideDone <- err
			}()
			select {
			case err := <-besideDone:
				if err != nil {
					t.Fatalf("operation beside a running %s: %v", running.name, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("operations waited for the running %s", running.name)
			}
			select {
			case err := <-runningDone:
				t.Fatalf("the running %s ended early: %v", running.name, err)
			default:
			}
		})
	}
}
