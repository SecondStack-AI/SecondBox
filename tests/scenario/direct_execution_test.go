//go:build scenario_live

package scenario_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioDirectExecDeadlineDeliversTerminalAndReleasesQuota(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Execution.DataPlaneTransport = contracts.DataPlaneTransportDirect
	spec.Resources.ConcurrentOperations = 1
	profile := createScenarioProfile(t, fixture, "scenario-direct-exec", spec)
	handle, _ := createScenarioSandbox(t, fixture, profile, "direct-exec")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	streamCtx, stopStream := context.WithTimeout(ctx, 20*time.Second)
	defer stopStream()
	session, err := handle.CreateExecStream(streamCtx, secondboxclient.StreamingExecRequest{
		Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
			Mode: "shell", Command: "printf direct-output; sleep 60",
		}},
		Environment: secondboxclient.StringMap{}, DeadlineMilliseconds: 2000,
		MaximumOutputBytes: 4096, WindowBytes: 4096,
	}, uniqueScenarioKey(t, "direct-exec-deadline"), "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := handle.ConnectExecStream(streamCtx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := stream.GrantOutput(4096); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseInput(); err != nil {
		t.Fatal(err)
	}
	output, outcome := receiveScenarioExec(t, stream)
	if len(output) == 0 || outcome.ExecDeadlineExceeded == nil {
		t.Fatalf("direct Exec deadline lost output or terminal: output=%#v outcome=%s", output, describeScenarioExecOutcome(outcome))
	}
	probe := executeScenarioCommand(t, ctx, handle, "printf recovered", 1024, "post-direct-deadline")
	assertScenarioExited(t, probe, 0, "recovered", "")

	// Buffered exec always uses the proxied transport. With one operation slot
	// the probe runs only if the disconnected exec's cancellation completed
	// its session and released the slot.
	started := time.Now()
	disconnectContext, disconnect := context.WithTimeout(ctx, 2*time.Second)
	defer disconnect()
	if _, err := handle.Execute(
		disconnectContext,
		scenarioExecRequest("touch direct-disconnect-started; sleep 5; touch direct-disconnect-survived", 1024),
		uniqueScenarioKey(t, "direct-buffered-disconnect"),
		"",
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SecondBox scenario disconnected direct buffered Exec error = %v", err)
	}
	time.Sleep(time.Until(started.Add(7 * time.Second)))
	probe = executeScenarioCommand(
		t, ctx, handle,
		"test -e direct-disconnect-started || printf never-started; test -e direct-disconnect-survived && printf survived; printf done",
		1024, "post-direct-disconnect",
	)
	assertScenarioExited(t, probe, 0, "done", "")

	// A direct-transport command stopped by its client, or abandoned by a
	// disconnect, must release the single operation slot once the Runner
	// confirms the cancellation.
	stopped := createScenarioExecStream(t, ctx, handle, "printf started; sleep 60", 4096, 4096, "direct-stop")
	defer stopped.Close()
	if err := stopped.GrantOutput(4096); err != nil {
		t.Fatal(err)
	}
	if frame, err := stopped.Receive(); err != nil || frame.StreamOutputFrame == nil {
		t.Fatalf("SecondBox scenario direct command did not start: %#v, %v", frame, err)
	}
	if err := stopped.Cancel(); err != nil {
		t.Fatal(err)
	}
	if _, outcome := receiveScenarioExec(t, stopped); outcome.ExecCancelled == nil {
		t.Fatalf("SecondBox scenario stopped direct command outcome = %s", describeScenarioExecOutcome(outcome))
	}
	probe = executeScenarioCommandWhenSlotFree(t, ctx, handle, "printf stopped", "post-direct-stop")
	assertScenarioExited(t, probe, 0, "stopped", "")

	abandoned := createScenarioExecStream(t, ctx, handle, "printf started; sleep 60", 4096, 4096, "direct-abandon")
	if err := abandoned.GrantOutput(4096); err != nil {
		t.Fatal(err)
	}
	if frame, err := abandoned.Receive(); err != nil || frame.StreamOutputFrame == nil {
		t.Fatalf("SecondBox scenario abandoned direct command did not start: %#v, %v", frame, err)
	}
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}
	probe = executeScenarioCommandWhenSlotFree(t, ctx, handle, "printf abandoned", "post-direct-abandon")
	assertScenarioExited(t, probe, 0, "abandoned", "")
}

// executeScenarioCommandWhenSlotFree retries a buffered command refused only
// because a cancelled operation still holds the Profile's operation slot.
func executeScenarioCommandWhenSlotFree(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	command string,
	key string,
) secondboxclient.ExecOutcome {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		outcome, err := handle.Execute(ctx, scenarioExecRequest(command, 1024), uniqueScenarioKey(t, key), "")
		if err == nil {
			return outcome
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) || apiError.Problem == nil ||
			apiError.StatusCode != http.StatusTooManyRequests || apiError.Problem.Code != "quota_exceeded" {
			t.Fatalf("SecondBox scenario buffered Exec: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("SecondBox scenario cancelled operation kept its operation slot")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
