//go:build scenario_live

package scenario_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioExecutesBufferedAndStreamingCommands(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-execution",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, _ := createScenarioSandbox(t, fixture, profile, "execution")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)

	t.Run("buffered output and exit status", func(t *testing.T) {
		outcome := executeScenarioCommand(
			t,
			ctx,
			handle,
			"printf 'buffered-stdout'; printf 'buffered-stderr' >&2",
			1<<20,
			"buffered-success",
		)
		assertScenarioExited(t, outcome, 0, "buffered-stdout", "buffered-stderr")

		outcome = executeScenarioCommand(
			t,
			ctx,
			handle,
			"printf 'failed-stdout'; printf 'failed-stderr' >&2; exit 23",
			1<<20,
			"buffered-failure",
		)
		assertScenarioExited(t, outcome, 23, "failed-stdout", "failed-stderr")
	})

	t.Run("buffered output exhaustion is explicit", func(t *testing.T) {
		outcome := executeScenarioCommand(
			t,
			ctx,
			handle,
			"printf '0123456789abcdefghijklmnopqrstuvwxyz'",
			16,
			"buffered-exhausted",
		)
		if outcome.ExecOutputExhausted == nil ||
			outcome.ExecOutputExhausted.LimitBytes != 16 {
			t.Fatalf("SecondBox scenario exhausted Exec outcome = %#v", outcome)
		}
		stdout := decodeScenarioOutput(t, outcome.ExecOutputExhausted.Output.StdoutBase64)
		if len(stdout) > 16 || !strings.HasPrefix("0123456789abcdefghijklmnopqrstuvwxyz", stdout) {
			t.Fatalf("SecondBox scenario bounded stdout = %q", stdout)
		}
	})

	t.Run("stream output is ordered and stdin reaches the guest", func(t *testing.T) {
		stream := createScenarioExecStream(
			t,
			ctx,
			handle,
			"read line; printf 'stdin:%s\\n' \"$line\"; sleep 1; printf 'stderr-one\\n' >&2; sleep 1; printf 'stdout-two\\n'",
			65536,
			65536,
			"stream-ordered",
		)
		defer stream.Close()
		if err := stream.SendInputFrame([]byte("payload\n"), false); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseInput(); err != nil {
			t.Fatal(err)
		}
		if err := stream.GrantOutput(65536); err != nil {
			t.Fatal(err)
		}
		output, outcome := receiveScenarioExec(t, stream)
		want := []scenarioStreamOutput{
			{stream: "stdout", data: "stdin:payload\n"},
			{stream: "stderr", data: "stderr-one\n"},
			{stream: "stdout", data: "stdout-two\n"},
		}
		if len(output) != len(want) {
			t.Fatalf("SecondBox scenario ordered stream output = %#v", output)
		}
		for index := range want {
			if output[index] != want[index] {
				t.Fatalf("SecondBox scenario ordered stream output[%d] = %#v, want %#v", index, output[index], want[index])
			}
		}
		assertScenarioExited(t, outcome, 0, "stdin:payload\nstdout-two\n", "stderr-one\n")
	})

	t.Run("output waits for credit", func(t *testing.T) {
		stream := createScenarioExecStream(
			t,
			ctx,
			handle,
			"i=0; while [ \"$i\" -lt 5000 ]; do printf x; i=$((i+1)); done",
			8192,
			4096,
			"stream-credit",
		)
		defer stream.Close()
		if err := stream.CloseInput(); err != nil {
			t.Fatal(err)
		}
		first := make(chan scenarioReceiveResult, 1)
		go func() {
			frame, err := stream.Receive()
			first <- scenarioReceiveResult{frame: frame, err: err}
		}()
		select {
		case result := <-first:
			t.Fatalf("SecondBox scenario stream emitted before credit: frame=%#v error=%v", result.frame, result.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := stream.GrantOutput(4096); err != nil {
			t.Fatal(err)
		}
		result := <-first
		if result.err != nil || result.frame.StreamOutputFrame == nil {
			t.Fatalf("SecondBox scenario first credited frame = %#v error=%v", result.frame, result.err)
		}
		firstChunk := decodeScenarioOutput(t, result.frame.StreamOutputFrame.DataBase64)
		if len(firstChunk) == 0 || len(firstChunk) > 4096 {
			t.Fatalf("SecondBox scenario first credited chunk bytes = %d", len(firstChunk))
		}
		total := len(firstChunk)
		consumed := len(firstChunk)
		var outcome secondboxclient.ExecOutcome
		for {
			// Replenish only bytes actually consumed. Output frame boundaries are
			// backend- and guest-dependent, while the negotiated window is not.
			if err := stream.GrantOutput(int64(consumed)); err != nil {
				t.Fatal(err)
			}
			frame, err := stream.Receive()
			if err != nil {
				t.Fatalf("SecondBox scenario receive credited Exec stream: %v", err)
			}
			if frame.StreamOutputFrame != nil {
				content := decodeScenarioOutput(t, frame.StreamOutputFrame.DataBase64)
				consumed = len(content)
				total += consumed
				continue
			}
			if frame.StreamOutcomeFrame == nil {
				t.Fatalf("SecondBox scenario unsupported credited Exec frame = %#v", frame)
			}
			outcome = frame.StreamOutcomeFrame.Outcome
			break
		}
		if total != 5000 {
			t.Fatalf("SecondBox scenario credited output bytes = %d, want 5000", total)
		}
		if outcome.ExecExited == nil || outcome.ExecExited.ExitCode != 0 {
			t.Fatalf("SecondBox scenario credited outcome = %#v", outcome)
		}
	})

	t.Run("HTTP cancellation preserves Sandbox readiness", func(t *testing.T) {
		session := negotiateScenarioExecStream(
			t,
			ctx,
			handle,
			"sleep 60",
			4096,
			4096,
			"stream-cancel",
		)
		stream, err := handle.ConnectExecStream(ctx, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		cancelled := scenarioJSON[secondboxclient.ExecStreamSession](
			t,
			ctx,
			fixture.subject,
			"cancelSandboxExecStream",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{
					"sandboxId":     string(handle.Snapshot().ID),
					"execSessionId": string(session.ID),
				},
				Headers: func() http.Header {
					headers := handle.GenerationHeaders("")
					headers.Set("Idempotency-Key", uniqueScenarioKey(t, "stream-cancel-http"))
					return headers
				}(),
			},
		)
		if cancelled.ID != session.ID ||
			(cancelled.State != secondboxclient.SessionStateClosing &&
				cancelled.State != secondboxclient.SessionStateClosed) {
			t.Fatalf("SecondBox scenario cancelled Exec session = %#v", cancelled)
		}
		_, outcome := receiveScenarioExec(t, stream)
		if outcome.ExecCancelled == nil {
			t.Fatalf("SecondBox scenario cancelled outcome = %#v", outcome)
		}
		ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
		if ready.State != contracts.SandboxStateReady {
			t.Fatalf("SecondBox scenario Sandbox after cancellation = %#v", ready)
		}
		probe := executeScenarioCommand(t, ctx, handle, "printf ready", 1024, "post-cancel")
		assertScenarioExited(t, probe, 0, "ready", "")
	})

	t.Run("configured pair of concurrent executions completes", func(t *testing.T) {
		const executions = 2
		outcomes := make([]secondboxclient.ExecOutcome, executions)
		failures := make([]error, executions)
		var group sync.WaitGroup
		for index := 0; index < executions; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				runContext, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				outcomes[index], failures[index] = handle.Execute(
					runContext,
					scenarioExecRequest("sleep 1; printf concurrent-"+string(rune('0'+index)), 1024),
					uniqueScenarioKey(t, "concurrent-exec"),
					"",
				)
			}(index)
		}
		group.Wait()
		for index := 0; index < executions; index++ {
			if failures[index] != nil {
				t.Fatalf("SecondBox scenario concurrent Exec %d: %v", index, failures[index])
			}
			assertScenarioExited(
				t,
				outcomes[index],
				0,
				"concurrent-"+string(rune('0'+index)),
				"",
			)
		}
	})
}

type scenarioStreamOutput struct {
	stream string
	data   string
}

type scenarioReceiveResult struct {
	frame secondboxclient.ExecStreamFrame
	err   error
}

func scenarioExecRequest(command string, maximumOutputBytes int64) secondboxclient.BufferedExecRequest {
	return secondboxclient.BufferedExecRequest{
		Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
			Mode: "shell", Command: command,
		}},
		Environment:          secondboxclient.StringMap{},
		DeadlineMilliseconds: 30000,
		MaximumOutputBytes:   maximumOutputBytes,
	}
}

func executeScenarioCommand(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	command string,
	maximumOutputBytes int64,
	key string,
) secondboxclient.ExecOutcome {
	t.Helper()
	outcome, err := handle.Execute(
		ctx,
		scenarioExecRequest(command, maximumOutputBytes),
		uniqueScenarioKey(t, key),
		"",
	)
	if err != nil {
		t.Fatalf("SecondBox scenario buffered Exec: %v", err)
	}
	return outcome
}

func assertScenarioExited(
	t *testing.T,
	outcome secondboxclient.ExecOutcome,
	exitCode int,
	stdout string,
	stderr string,
) {
	t.Helper()
	if outcome.ExecExited == nil ||
		outcome.ExecExited.ExitCode != exitCode ||
		decodeScenarioOutput(t, outcome.ExecExited.Output.StdoutBase64) != stdout ||
		decodeScenarioOutput(t, outcome.ExecExited.Output.StderrBase64) != stderr {
		t.Fatalf("SecondBox scenario exited outcome = %s", describeScenarioExecOutcome(outcome))
	}
}

func decodeScenarioOutput(t *testing.T, encoded string) string {
	t.Helper()
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		t.Fatalf("SecondBox scenario output is not canonical base64: %v", err)
	}
	return string(decoded)
}

func negotiateScenarioExecStream(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	command string,
	maximumOutputBytes int64,
	windowBytes int64,
	key string,
) secondboxclient.ExecStreamSession {
	t.Helper()
	session, err := handle.CreateExecStream(
		ctx,
		secondboxclient.StreamingExecRequest{
			Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
				Mode: "shell", Command: command,
			}},
			Environment:          secondboxclient.StringMap{},
			DeadlineMilliseconds: 30000,
			MaximumOutputBytes:   maximumOutputBytes,
			WindowBytes:          windowBytes,
		},
		uniqueScenarioKey(t, key),
		"",
	)
	if err != nil {
		t.Fatalf("SecondBox scenario create Exec stream: %v", err)
	}
	return session
}

func createScenarioExecStream(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	command string,
	maximumOutputBytes int64,
	windowBytes int64,
	key string,
) *secondboxclient.ExecStream {
	t.Helper()
	session := negotiateScenarioExecStream(
		t,
		ctx,
		handle,
		command,
		maximumOutputBytes,
		windowBytes,
		key,
	)
	stream, err := handle.ConnectExecStream(ctx, session, nil)
	if err != nil {
		t.Fatalf("SecondBox scenario connect Exec stream: %v", err)
	}
	return stream
}

func receiveScenarioExec(
	t *testing.T,
	stream *secondboxclient.ExecStream,
) ([]scenarioStreamOutput, secondboxclient.ExecOutcome) {
	t.Helper()
	var output []scenarioStreamOutput
	for {
		frame, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("SecondBox scenario Exec stream ended without an outcome")
			}
			t.Fatalf("SecondBox scenario receive Exec stream: %v", err)
		}
		if frame.StreamOutputFrame != nil {
			output = append(output, scenarioStreamOutput{
				stream: frame.StreamOutputFrame.Stream,
				data:   decodeScenarioOutput(t, frame.StreamOutputFrame.DataBase64),
			})
			continue
		}
		if frame.StreamOutcomeFrame != nil {
			return output, frame.StreamOutcomeFrame.Outcome
		}
		t.Fatalf("SecondBox scenario unsupported Exec frame = %#v", frame)
	}
}
