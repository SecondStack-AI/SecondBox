package firecracker

import (
	"bytes"
	"testing"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestGuestPTYRequestPreservesCommandDirectoryEnvironmentDeadlineAndDimensions(t *testing.T) {
	deadline := uint64(4_200_000)
	open := &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/printf", "%s", "exact"}}},
		Cwd:              "nested",
		Environment:      []*runnerprotocol.EnvironmentEntry{{Name: "BINARY", Value: []byte{0, 0xfe}}},
		DeadlineUnixMs:   deadline,
		OutputLimitBytes: 4096,
		AllocatePty:      true,
		PtyRows:          41,
		PtyColumns:       121,
		Stdin:            []byte{0, 1, 0xff},
		Streaming:        true,
	}
	request, err := guestExecRequest(open)
	if err != nil {
		t.Fatal(err)
	}
	argv := request.GetArgv().GetArgument()
	if len(argv) != 3 || argv[0] != "/bin/printf" || argv[1] != "%s" || argv[2] != "exact" ||
		request.Cwd != "nested" ||
		request.DeadlineUnixMs != deadline ||
		request.OutputLimitBytes != 4096 ||
		!request.Streaming ||
		request.Pty.GetRows() != 41 ||
		request.Pty.GetColumns() != 121 ||
		!bytes.Equal(request.Stdin, []byte{0, 1, 0xff}) ||
		len(request.Environment) != 1 ||
		request.Environment[0].Name != "BINARY" ||
		!bytes.Equal(request.Environment[0].Value, []byte{0, 0xfe}) {
		t.Fatalf("translated guest PTY request = %#v", request)
	}
	open.GetArgv().Argument[0] = "changed"
	open.Environment[0].Value[0] = 9
	open.Stdin[0] = 9
	if request.GetArgv().GetArgument()[0] != "/bin/printf" ||
		request.Environment[0].Value[0] != 0 ||
		request.Stdin[0] != 0 {
		t.Fatal("translated guest PTY request aliases runner protocol memory")
	}
}

func TestRunnerExecTerminalMappings(t *testing.T) {
	tests := map[guestv1.ExecTerminalKind]runnerprotocol.ExecTerminalKind{
		guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED:            runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
		guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED:      runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED,
		guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED,
		guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED:         runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
		guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED:  runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED,
	}
	for guest, want := range tests {
		if got := runnerExecTerminalKind(guest); got != want {
			t.Errorf("runnerExecTerminalKind(%s) = %s, want %s", guest, got, want)
		}
	}
}

func TestRunnerSpawnFailureReasonMappings(t *testing.T) {
	tests := map[guestv1.SpawnFailureReason]runnerprotocol.SpawnFailureReason{
		guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND:            runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND,
		guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED:    runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED,
		guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD:          runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD,
		guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE: runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE,
	}
	for guest, want := range tests {
		if got := runnerSpawnFailureReason(guest); got != want {
			t.Errorf("runnerSpawnFailureReason(%s) = %s, want %s", guest, got, want)
		}
	}
}

func TestRunnerFileTerminalMappings(t *testing.T) {
	tests := map[guestv1.FileTerminalKind]runnerprotocol.FileTerminalKind{
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED:         runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND:         runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH:      runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_SYMLINK_REJECTED:  runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED:    runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED:         runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED,
	}
	for guest, want := range tests {
		if got := runnerFileTerminalKind(guest); got != want {
			t.Errorf("runnerFileTerminalKind(%s) = %s, want %s", guest, got, want)
		}
	}
}
