//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioConcurrentInstancesRemainIsolated(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-concurrent-instance-isolation",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	first, _ := createScenarioSandbox(t, fixture, profile, "concurrent-isolation-first")
	second, _ := createScenarioSandbox(t, fixture, profile, "concurrent-isolation-second")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	firstReady := waitForSandbox(t, ctx, first, secondboxclient.SandboxStateReady)
	secondReady := waitForSandbox(t, ctx, second, secondboxclient.SandboxStateReady)
	if firstReady.Instance == nil || secondReady.Instance == nil ||
		firstReady.Instance.ID == secondReady.Instance.ID ||
		firstReady.Workspace.ID == secondReady.Workspace.ID {
		t.Fatalf("SecondBox concurrent isolation identities first=%#v second=%#v", firstReady, secondReady)
	}

	firstStream := createScenarioExecStream(
		t, ctx, first,
		"read marker; printf '%s' \"$marker\" > /workspace/isolation-marker; sleep 1; cat /workspace/isolation-marker",
		4096, 4096, "concurrent-isolation-first-stream",
	)
	defer firstStream.Close()
	secondStream := createScenarioExecStream(
		t, ctx, second,
		"read marker; printf '%s' \"$marker\" > /workspace/isolation-marker; sleep 1; cat /workspace/isolation-marker",
		4096, 4096, "concurrent-isolation-second-stream",
	)
	defer secondStream.Close()
	for _, streamInput := range []struct {
		stream *secondboxclient.ExecStream
		input  string
	}{{firstStream, "first-instance\n"}, {secondStream, "second-instance\n"}} {
		if err := streamInput.stream.SendInputFrame([]byte(streamInput.input), false); err != nil {
			t.Fatal(err)
		}
		if err := streamInput.stream.CloseInput(); err != nil {
			t.Fatal(err)
		}
		if err := streamInput.stream.GrantOutput(4096); err != nil {
			t.Fatal(err)
		}
	}
	firstOutput, firstOutcome := receiveScenarioExec(t, firstStream)
	secondOutput, secondOutcome := receiveScenarioExec(t, secondStream)
	assertScenarioExited(t, firstOutcome, 0, "first-instance", "")
	assertScenarioExited(t, secondOutcome, 0, "second-instance", "")
	if len(firstOutput) == 0 || len(secondOutput) == 0 {
		t.Fatalf("SecondBox concurrent streams lacked output first=%#v second=%#v", firstOutput, secondOutput)
	}

	firstDisk := readScenarioFile(t, ctx, fixture.subject, first, "isolation-marker")
	secondDisk := readScenarioFile(t, ctx, fixture.subject, second, "isolation-marker")
	if !bytes.Equal(firstDisk, []byte("first-instance")) ||
		!bytes.Equal(secondDisk, []byte("second-instance")) {
		t.Fatalf("SecondBox concurrent Workspace isolation first=%q second=%q", firstDisk, secondDisk)
	}

	networkProbe := "curl --silent --show-error --connect-timeout 2 --max-time 2 http://example.com/ >/dev/null"
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "microsandbox" {
		networkProbe = "wget -q -T 2 -O /dev/null http://example.com/"
	}
	for name, handle := range map[string]*secondboxclient.SandboxHandle{
		"first":  first,
		"second": second,
	} {
		outcome := executeScenarioCommand(
			t, ctx, handle,
			networkProbe,
			4096, "concurrent-isolation-deny-all-"+name,
		)
		if outcome.ExecExited == nil || outcome.ExecExited.ExitCode == 0 {
			t.Fatalf("SecondBox concurrent %s network namespace escaped deny-all: %#v", name, outcome)
		}
	}
}
