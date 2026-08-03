//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	relayretention "github.com/SecondStack-AI/SecondBox/tests/scenario/relay_retention"
	"github.com/gorilla/websocket"
)

func TestScenarioRelayRetentionMeasurement(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Lifecycle.LeaseSeconds = 900
	spec.Ports = []contracts.PortPolicy{{
		Name: "web", Port: 8080, Protocol: "tcp", MaximumSessions: 4, MaximumSessionSeconds: 60,
	}}
	profile := createScenarioProfile(t, fixture, "scenario-relay-retention", spec)
	handle, _ := createScenarioSandbox(t, fixture, profile, "relay-retention")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	lease := acquireScenarioLease(t, ctx, fixture, handle, 600, "relay-retention-lease")

	collector, err := relayretention.Open(
		ctx,
		requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_DATABASE_URL"),
		scenarioRunnerID,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := collector.Close(); err != nil {
			t.Errorf("close relay-retention collector: %v", err)
		}
	})
	if err := collector.StartNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	postgresVersion, err := collector.PostgresVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report := relayretention.NewReport(time.Now(), postgresVersion, collector.SeparateFrameRetention())
	var measuredSessionIDs []string
	for cycleNumber := 1; cycleNumber <= relayretention.MeasurementCycles; cycleNumber++ {
		cycleStarted := time.Now().UTC()
		before, err := collector.RelationSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		runRelayRetentionPTY(t, ctx, handle, lease.ID, cycleNumber)
		runRelayRetentionFile(t, ctx, fixture, handle, cycleNumber)
		runRelayRetentionPort(t, ctx, fixture, handle, lease.ID, cycleNumber)
		sessionIDs, err := collector.SessionIDs(ctx, handle.Snapshot().ID, cycleStarted)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessionIDs) != 4 {
			t.Fatalf("relay-retention cycle %d sessions = %v, want Terminal, File write/read, and relay Port", cycleNumber, sessionIDs)
		}
		measuredSessionIDs = append(measuredSessionIDs, sessionIDs...)
		afterWork, err := collector.RelationSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		framesAfterWork, err := collector.FrameStats(ctx, sessionIDs)
		if err != nil {
			t.Fatal(err)
		}
		cleanupContext, cleanupCancel := context.WithTimeout(ctx, 30*time.Second)
		err = collector.WaitForFrameCleanup(cleanupContext, sessionIDs)
		cleanupCancel()
		if err != nil {
			t.Fatal(err)
		}
		framesAfterSweep, err := collector.FrameStats(ctx, sessionIDs)
		if err != nil {
			t.Fatal(err)
		}
		if err := collector.VacuumFull(ctx); err != nil {
			t.Fatal(err)
		}
		afterVacuum, err := collector.RelationSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		report.Cycles = append(report.Cycles, relayretention.Cycle{
			Number: cycleNumber, SessionIDs: sessionIDs,
			FramesAfterWork: framesAfterWork, FramesAfterSweep: framesAfterSweep,
			Before: before, AfterWork: afterWork, AfterVacuum: afterVacuum,
		})
	}
	if err := collector.StopNotifications(); err != nil {
		t.Fatal(err)
	}
	report.Notifications = collector.Counts(measuredSessionIDs)
	output := os.Getenv("SECONDBOX_RELAY_RETENTION_OUTPUT")
	if output == "" {
		output = filepath.Join(t.TempDir(), "relay-retention.json")
	}
	if err := relayretention.WriteReport(output, report); err != nil {
		t.Fatal(err)
	}
	t.Logf("SecondBox relay-retention machine-readable result: %s", output)
}

func runRelayRetentionPTY(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	cycle int,
) {
	t.Helper()
	session, err := handle.CreateTerminal(ctx, secondboxclient.CreateTerminalRequest{
		Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
			Mode: "shell",
			Command: fmt.Sprintf(
				"stty -echo; IFS= read -r input; test ${#input} -eq %d; head -c %d /dev/zero | tr '\\000' P",
				relayretention.InteractivePTYInputBytes-1,
				relayretention.InteractivePTYOutputBytes,
			),
		}},
		Environment: secondboxclient.StringMap{}, Rows: 24, Columns: 80,
		DeadlineMilliseconds: 60000, Detachable: false,
	}, uniqueScenarioKey(t, fmt.Sprintf("relay-retention-pty-%d", cycle)), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := handle.ConnectTerminal(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := terminal.GrantOutput(relayretention.InteractivePTYOutputBytes + 1024); err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte{'I'}, relayretention.InteractivePTYInputBytes-1)
	input = append(input, '\n')
	if err := terminal.SendInput(input); err != nil {
		t.Fatal(err)
	}
	var outputBytes int
	for {
		frame, err := terminal.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if frame.StreamOutcomeFrame != nil {
			if frame.StreamOutcomeFrame.Outcome.ExecExited == nil {
				t.Fatalf("relay-retention PTY outcome = %#v", frame.StreamOutcomeFrame.Outcome)
			}
			break
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(frame.TerminalOutputFrame.DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		outputBytes += len(decoded)
	}
	if outputBytes != relayretention.InteractivePTYOutputBytes {
		t.Fatalf("relay-retention PTY output bytes = %d, want %d", outputBytes, relayretention.InteractivePTYOutputBytes)
	}
}

func runRelayRetentionFile(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	cycle int,
) {
	t.Helper()
	content := make([]byte, relayretention.LargeFileBytes)
	for index := range content {
		content[index] = byte((index + cycle) % 251)
	}
	path := fmt.Sprintf("relay-retention-%d.bin", cycle)
	writeScenarioFile(t, ctx, fixture.subject, handle, path, content)
	read := readScenarioFile(t, ctx, fixture.subject, handle, path)
	if !bytes.Equal(read, content) {
		t.Fatalf("relay-retention File cycle %d read %d changed bytes", cycle, len(read))
	}
}

func runRelayRetentionPort(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	leaseID string,
	cycle int,
) {
	t.Helper()
	listener := executeScenarioCommand(t, ctx, handle, fmt.Sprintf(`nohup python3 -c '
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("127.0.0.1",8080)); s.listen(2)
while True:
    c,_=s.accept(); remaining=%d
    while remaining:
        data=c.recv(min(65536,remaining))
        if not data: break
        c.sendall(data); remaining-=len(data)
    c.close()
    if remaining == 0: break
s.close()
' >/workspace/relay-retention-port-%d.log 2>&1 </dev/null &
python3 -c 'import socket,time
for _ in range(50):
    try:
        socket.create_connection(("127.0.0.1",8080),1).close(); raise SystemExit(0)
    except OSError: time.sleep(.1)
raise SystemExit(1)'`, relayretention.RelayPortTotalBytes, cycle), 4096, fmt.Sprintf("relay-retention-port-listener-%d", cycle))
	assertScenarioExited(t, listener, 0, "", "")
	session := createScenarioPortSession(t, ctx, fixture, handle, leaseID, fmt.Sprintf("relay-retention-port-%d", cycle))
	if session.Transport != contracts.PortTransportRelay {
		t.Fatalf("relay-retention Port transport = %q, want relay", session.Transport)
	}
	connection := dialScenarioPortTunnel(t, ctx, session.Endpoint)
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{byte(cycle)}, relayretention.RelayPortFrameBytes)
	for sent := 0; sent < relayretention.RelayPortTotalBytes; sent += len(payload) {
		if err := connection.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			t.Fatal(err)
		}
		received := make([]byte, 0, len(payload))
		for len(received) < len(payload) {
			messageType, chunk, err := connection.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if messageType != websocket.BinaryMessage {
				t.Fatalf("relay-retention Port message type = %d", messageType)
			}
			received = append(received, chunk...)
		}
		if !bytes.Equal(received, payload) {
			t.Fatal("relay-retention Port echo changed payload")
		}
	}
	_, _, err := connection.ReadMessage()
	if err == nil || !strings.Contains(err.Error(), "close 1000") {
		t.Fatalf("relay-retention Port terminal close = %v", err)
	}
}
