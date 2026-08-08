package cliui_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/creack/pty"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate CLI UI test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func buildBinaries(t *testing.T) (string, string, string) {
	t.Helper()
	root := repositoryRoot(t)
	directory := t.TempDir()
	secondbox := filepath.Join(directory, "secondbox")
	deploy := filepath.Join(directory, "secondbox-deploy")
	command := exec.Command("go", "build", "-o", secondbox, "./cmd/secondbox")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build secondbox: %v\n%s", err, output)
	}
	command = exec.Command("go", "build", "-o", deploy, "./cmd/secondbox-deploy")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build secondbox-deploy: %v\n%s", err, output)
	}
	working := filepath.Join(directory, "outside-checkout")
	if err := os.Mkdir(working, 0o700); err != nil {
		t.Fatal(err)
	}
	return secondbox, deploy, working
}

func TestReleaseBinariesSelectPipeAndPTYRenderers(t *testing.T) {
	secondbox, deploy, working := buildBinaries(t)
	command := exec.Command(secondbox, "version")
	command.Dir = working
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	machine, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(machine) != "{\"version\":\"0.0.0-development\",\"sourceCommit\":\"development\"}\n" {
		t.Fatalf("piped version changed: %q", machine)
	}
	if bytes.Contains(machine, []byte("\x1b")) {
		t.Fatalf("piped version contains controls: %q", machine)
	}

	for _, size := range []struct{ columns, rows uint16 }{{40, 10}, {120, 30}} {
		command = exec.Command(secondbox, "--color", "never", "version")
		command.Dir = working
		terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: size.columns, Rows: size.rows})
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(terminal)
		_ = terminal.Close()
		waitErr := command.Wait()
		if readErr != nil && !strings.Contains(readErr.Error(), "input/output error") {
			t.Fatal(readErr)
		}
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		text := strings.ReplaceAll(string(content), "\r\n", "\n")
		if !strings.Contains(text, "SecondBox CLI\n") || !strings.Contains(text, "Version") {
			t.Fatalf("PTY %dx%d version: %q", size.columns, size.rows, text)
		}
		if strings.Contains(text, "\x1b") {
			t.Fatalf("--color never PTY contains controls: %q", text)
		}
	}

	command = exec.Command(deploy)
	command.Dir = working
	command.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	help, readErr := io.ReadAll(terminal)
	_ = terminal.Close()
	waitErr := command.Wait()
	if readErr != nil && !strings.Contains(readErr.Error(), "input/output error") {
		t.Fatal(readErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("deploy help exit = %v, want 1", waitErr)
	}
	text := strings.ReplaceAll(string(help), "\r\n", "\n")
	if !strings.Contains(text, "SecondBox Deploy\n\nUsage\n") || !strings.Contains(text, "Commands\n") {
		t.Fatalf("deploy PTY help is not structured: %q", text)
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("NO_COLOR help contains controls: %q", text)
	}
}
