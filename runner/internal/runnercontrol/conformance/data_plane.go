package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// DataPlaneFixture binds the reusable provider-neutral operation suite to one
// already-ready assignment. Firecracker and Microsandbox both execute this suite.
type DataPlaneFixture struct {
	Backend runnercontrol.DataPlaneBackend
	PTY     runnercontrol.PTYDataPlaneBackend
	Port    runnercontrol.PortBackend
	Fence   *runnerprotocol.AssignmentFence
}

func RunDataPlane(t *testing.T, fixture DataPlaneFixture) {
	t.Helper()
	if fixture.Backend == nil || fixture.PTY == nil || fixture.Port == nil || fixture.Fence == nil {
		t.Fatal("data-plane conformance fixture is incomplete")
	}
	t.Run("typed exec bounds and cancellation", func(t *testing.T) {
		spawned, err := fixture.Backend.ExecuteBuffered(t.Context(), fixture.Fence, execArgv("/definitely/missing"))
		if err != nil || spawned.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED || spawned.Terminal.GetSpawnFailureReason() != runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND {
			t.Fatalf("spawn failure = %#v, %v", spawned, err)
		}
		exhausted := execShell("printf 0123456789")
		exhausted.OutputLimitBytes = 5
		bounded, err := fixture.Backend.ExecuteBuffered(t.Context(), fixture.Fence, exhausted)
		if err != nil || string(bounded.Stdout) != "01234" || bounded.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED || bounded.Terminal.GetLimitBytes() != 5 {
			t.Fatalf("bounded output = %#v, %v", bounded, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(25*time.Millisecond, cancel)
		cancelled, err := fixture.Backend.ExecuteBuffered(ctx, fixture.Fence, execShell("sleep 5"))
		if err != nil || cancelled.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
			t.Fatalf("cancelled exec = %#v, %v", cancelled, err)
		}
	})
	t.Run("stream credit and channels", func(t *testing.T) {
		controls := make(chan runnercontrol.ExecControl, 3)
		controls <- runnercontrol.ExecControl{Input: &runnerprotocol.ExecInput{Data: []byte("value")}}
		controls <- runnercontrol.ExecControl{Input: &runnerprotocol.ExecInput{EndOfInput: true}}
		outputs := make(chan string, 2)
		completed := make(chan error, 1)
		go func() {
			open := execShell("value=$(cat); printf stdout:$value; printf stderr:$value >&2")
			open.Streaming = true
			terminal, err := fixture.Backend.ExecuteStreaming(t.Context(), fixture.Fence, open, controls, func(channel runnerprotocol.ExecOutputChannel, data []byte) error {
				outputs <- channel.String() + ":" + string(data)
				return nil
			})
			if err == nil && terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
				err = fmt.Errorf("unexpected terminal %s", terminal.GetKind())
			}
			completed <- err
		}()
		select {
		case output := <-outputs:
			t.Fatalf("output bypassed caller credit: %q", output)
		case <-time.After(50 * time.Millisecond):
		}
		controls <- runnercontrol.ExecControl{Credit: 1024}
		close(controls)
		seen := map[string]bool{}
		for len(seen) != 2 {
			select {
			case output := <-outputs:
				seen[output] = true
			case <-time.After(2 * time.Second):
				t.Fatalf("stream output timed out: %v", seen)
			}
		}
		if !seen["EXEC_OUTPUT_CHANNEL_STDOUT:stdout:value"] || !seen["EXEC_OUTPUT_CHANNEL_STDERR:stderr:value"] {
			t.Fatalf("stream channels = %v", seen)
		}
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("binary workspace files", func(t *testing.T) {
		path := "data-plane-conformance/value.bin"
		mkdir, err := fixture.Backend.ExecuteFile(t.Context(), fixture.Fence, &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "data-plane-conformance"}, nil)
		if err != nil || mkdir.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
			t.Fatalf("mkdir = %#v, %v", mkdir, err)
		}
		content := []byte{0, 1, 2, 0xfe, 0xff}
		checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		written, err := fixture.Backend.ExecuteFile(t.Context(), fixture.Fence, &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: path, ExpectedSize: uint64(len(content)), ExpectedChecksum: checksum}, content)
		if err != nil || written.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
			t.Fatalf("write = %#v, %v", written, err)
		}
		read, err := fixture.Backend.ExecuteFile(t.Context(), fixture.Fence, &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: path, ExpectedSize: 1024}, nil)
		if err != nil || !bytes.Equal(read.Content, content) || read.Metadata.GetChecksum() != checksum {
			t.Fatalf("read = %#v, %v", read, err)
		}
		exists, err := fixture.Backend.ExecuteFile(t.Context(), fixture.Fence, &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_EXISTS, WorkspaceRelativePath: path}, nil)
		if err != nil || !exists.Metadata.GetExists() {
			t.Fatalf("exists = %#v, %v", exists, err)
		}
		removed, err := fixture.Backend.ExecuteFile(t.Context(), fixture.Fence, &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_REMOVE, WorkspaceRelativePath: "data-plane-conformance", Recursive: true}, nil)
		if err != nil || removed.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
			t.Fatalf("remove = %#v, %v", removed, err)
		}
	})
	t.Run("pty cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(25*time.Millisecond, cancel)
		open := execShell("sleep 5")
		open.Streaming, open.AllocatePty, open.PtyRows, open.PtyColumns = true, true, 24, 80
		terminal, err := fixture.PTY.ExecutePTY(ctx, fixture.Fence, open, make(chan runnercontrol.PTYControl), func([]byte) error { return nil })
		if err != nil || terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
			t.Fatalf("PTY cancellation = %#v, %v", terminal, err)
		}
	})
}

func execShell(shell string) *runnerprotocol.ExecOpen {
	return &runnerprotocol.ExecOpen{Command: &runnerprotocol.ExecOpen_Shell{Shell: shell}, DeadlineUnixMs: uint64(time.Now().Add(2 * time.Second).UnixMilli()), OutputLimitBytes: 1024}
}

func execArgv(arguments ...string) *runnerprotocol.ExecOpen {
	return &runnerprotocol.ExecOpen{Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: arguments}}, DeadlineUnixMs: uint64(time.Now().Add(2 * time.Second).UnixMilli()), OutputLimitBytes: 1024}
}
