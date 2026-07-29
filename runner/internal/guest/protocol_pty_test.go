package microvmguest

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

func TestProtocolPTYProcessIsRealTerminalWithMergedOutputAndResize(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "test -t 0 && test -t 1 && stty size; printf 'stderr\\n' >&2; read ready; stty size")
	process, err := startProtocolPTYProcess(cmd, &guestv1.PtyDimensions{Rows: 24, Columns: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	reader := bufio.NewReader(process)
	initialSize, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Write([]byte("ready\n")); err != nil {
		t.Fatal(err)
	}
	echoedInput, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	resizedOutput, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	remaining, _ := io.ReadAll(reader)
	normalized := strings.ReplaceAll(
		initialSize+stderr+echoedInput+resizedOutput+string(remaining),
		"\r",
		"",
	)
	if !strings.Contains(normalized, "24 80\n") ||
		!strings.Contains(normalized, "stderr\n") ||
		!strings.Contains(normalized, "40 120\n") {
		t.Fatalf("PTY output = %q", normalized)
	}
}

func TestProtocolPTYProcessWaitPreservesUnreadOutput(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf 'drain-after-wait'")
	process, err := startProtocolPTYProcess(cmd, &guestv1.PtyDimensions{Rows: 24, Columns: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(process)
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		t.Fatal(readErr)
	}
	if string(output) != "drain-after-wait" {
		t.Fatalf("PTY output after wait = %q", output)
	}
}

func TestProtocolPTYProcessCancellationKillsProcessGroupAndWaits(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	process, err := startProtocolPTYProcess(cmd, &guestv1.PtyDimensions{Rows: 24, Columns: 80})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := process.KillAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if process.ProcessState() == nil {
		t.Fatal("PTY process has no terminal wait state")
	}
}
