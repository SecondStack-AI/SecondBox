//go:build scenario_live

package scenario_test

import (
	"context"
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
}
