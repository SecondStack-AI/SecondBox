//go:build scenario_live && linux

package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/egressattribution"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func newScenarioAttributedSandbox(t *testing.T) (scenarioFixture, *secondboxclient.SandboxHandle, *net.UnixListener) {
	t.Helper()
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "microsandbox" {
		t.Skip("Microsandbox does not support attributed execution")
	}
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateStopped)
	*spec.Network.RequiresTenantEgressContext = true
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{Gateway: "execution.secondbox.internal", MaximumConnections: 2}
	profile := createScenarioProfile(t, fixture, "scenario-attributed", spec)
	handle, created := createScenarioSandbox(t, fixture, profile, "attributed")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_ATTRIBUTED_GATEWAY_DIR"), "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	return fixture, handle, gateway
}

func TestScenarioAttributedCommandsRetireEachGeneration(t *testing.T) {
	fixture, handle, gateway := newScenarioAttributedSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	previousInstance := ""
	previousGeneration := int64(0)
	for command := 1; command <= 2; command++ {
		reference := fmt.Sprintf("scenario-command-%d", command)
		expiry := time.Now().Add(time.Minute)
		start := requestScenarioLifecycle(t, ctx, handle, "attributed-start", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Start(ctx, secondboxclient.StartSandboxRequest{AttributedExecution: &contracts.AttributedExecutionRequest{AuthorizationRef: reference, ExpiresAt: expiry}}, options)
		})
		waitForScenarioOperation(t, ctx, fixture.subject, start)
		ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
		if ready.Instance == nil {
			t.Fatal("attributed start returned no Instance")
		}
		if ready.Instance.ID == previousInstance || ready.Generation <= previousGeneration {
			t.Fatalf("attributed start reused compute: %+v", ready)
		}
		previousInstance, previousGeneration = ready.Instance.ID, ready.Generation
		if err := gateway.SetDeadline(expiry); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			connection, err := gateway.AcceptUnix()
			if err != nil {
				result <- err
				return
			}
			defer connection.Close()
			actual, err := egressattribution.ReadRunnerExecutionAttribution(connection, 0, expiry)
			if err == nil && (actual.TenantRef != os.Getenv("SECONDBOX_SCENARIO_TENANT_REF") || actual.SubjectRef != os.Getenv("SECONDBOX_SCENARIO_SUBJECT_REF") || actual.SandboxID != ready.ID || actual.InstanceID != ready.Instance.ID || actual.Generation != ready.Generation || actual.AuthorizationRef != reference || actual.AssignmentID == "") {
				err = fmt.Errorf("incorrect admitted execution attribution: %+v", actual)
			}
			if err == nil {
				err = connection.SetDeadline(expiry)
			}
			request := make([]byte, len("guest-context: forged\n"))
			if err == nil {
				_, err = io.ReadFull(connection, request)
			}
			if err == nil && string(request) != "guest-context: forged\n" {
				err = fmt.Errorf("incorrect guest bytes: %q", request)
			}
			if err == nil {
				_, err = connection.Write([]byte("scenario-attributed"))
			}
			result <- err
		}()
		commandScript := fmt.Sprintf(`if [ %d -gt 1 ]; then test "$(cat generation-marker)" = 1 || exit 41; fi; printf %d > generation-marker; endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf 'guest-context: forged\n' | nc -w 2 "${endpoint%%:*}" "${endpoint##*:}"`, command, command)
		outcome := executeScenarioCommand(t, ctx, handle, commandScript, 1024, "attributed-exec")
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		assertScenarioExited(t, outcome, 0, "scenario-attributed", "")
		stopped := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
		if stopped.Instance != nil || stopped.Generation <= ready.Generation || stopped.DesiredState != contracts.SandboxDesiredStateStopped {
			t.Fatalf("attributed result did not retire compute: %+v", stopped)
		}
		if _, err := handle.Execute(ctx, scenarioExecRequest("printf forbidden", 1024), uniqueScenarioKey(t, "attributed-second-exec"), ""); err == nil {
			t.Fatal("command admitted after attributed generation completed")
		}
	}
}

func TestScenarioAttributedConnectionLossRevokesExecution(t *testing.T) {
	for _, service := range []string{"control-plane", "secondbox-runner"} {
		t.Run(service, func(t *testing.T) {
			fixture, handle, gateway := newScenarioAttributedSandbox(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			expiry := time.Now().Add(time.Minute)
			start := requestScenarioLifecycle(t, ctx, handle, "attributed-disconnect-start", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Start(ctx, secondboxclient.StartSandboxRequest{AttributedExecution: &contracts.AttributedExecutionRequest{AuthorizationRef: "scenario-disconnect", ExpiresAt: expiry}}, options)
			})
			waitForScenarioOperation(t, ctx, fixture.subject, start)
			ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
			if err := gateway.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			type executionResult struct {
				outcome secondboxclient.ExecOutcome
				err     error
			}
			done := make(chan executionResult, 1)
			go func() {
				request := scenarioExecRequest(`printf before-disconnect > disconnect-marker && sync || exit 1; endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`, 1024)
				request.DeadlineMilliseconds = 50000
				outcome, err := handle.Execute(ctx, request, uniqueScenarioKey(t, "attributed-disconnect-exec"), "")
				done <- executionResult{outcome: outcome, err: err}
			}()
			connection, err := gateway.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			actual, err := egressattribution.ReadRunnerExecutionAttribution(connection, 0, expiry)
			if err != nil || actual.AuthorizationRef != "scenario-disconnect" {
				t.Fatalf("attributed disconnect preface: %+v %v", actual, err)
			}
			if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			marker := make([]byte, len("ready"))
			if _, err := io.ReadFull(connection, marker); err != nil || string(marker) != "ready" {
				t.Fatalf("attributed descendant readiness: %q %v", marker, err)
			}
			if service == "secondbox-runner" {
				scenarioCompose(t, "kill", "-s", "SIGKILL", service)
				scenarioStartService(t, service)
			} else {
				scenarioCompose(t, "restart", "--no-deps", service)
			}
			if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("attributed connection survived control-plane loss: bytes=%d error=%v", n, err)
			}
			select {
			case result := <-done:
				if result.err == nil && result.outcome.ExecCancelled == nil && result.outcome.ExecInfrastructureFailed == nil {
					t.Fatalf("attributed disconnect returned no terminal failure: %+v", result.outcome)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("attributed exec remained active after control-plane loss")
			}
			waitForScenarioControlPlaneReady(t, fixture, 60*time.Second)
			waitForScenarioRunner(t, fixture, 90*time.Second)
			stopped := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
			if stopped.Instance != nil || stopped.Generation <= ready.Generation || stopped.DesiredState != contracts.SandboxDesiredStateStopped {
				t.Fatalf("attributed authority survived reconnection: %+v", stopped)
			}
			start = requestScenarioLifecycle(t, ctx, handle, "attributed-after-disconnect-start", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Start(ctx, secondboxclient.StartSandboxRequest{AttributedExecution: &contracts.AttributedExecutionRequest{AuthorizationRef: "scenario-after-disconnect", ExpiresAt: time.Now().Add(time.Minute)}}, options)
			})
			waitForScenarioOperation(t, ctx, fixture.subject, start)
			waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
			outcome := executeScenarioCommand(t, ctx, handle, "cat disconnect-marker", 1024, "attributed-after-disconnect-exec")
			assertScenarioExited(t, outcome, 0, "before-disconnect", "")
			waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
		})
	}
}

func TestScenarioAttributedConcurrentSandboxesKeepOwnAuthority(t *testing.T) {
	fixture, first, gateway := newScenarioAttributedSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	firstStopped := waitForSandbox(t, ctx, first, secondboxclient.SandboxStateStopped)
	second, created := createScenarioSandbox(t, fixture, contracts.Profile{Name: firstStopped.Profile}, "attributed-concurrent")
	waitForScenarioOperation(t, ctx, fixture.subject, created)
	waitForSandbox(t, ctx, second, secondboxclient.SandboxStateStopped)
	type commandResult struct {
		outcome secondboxclient.ExecOutcome
		err     error
	}
	type commandState struct {
		handle     *secondboxclient.SandboxHandle
		ready      contracts.Sandbox
		done       chan commandResult
		connection *net.UnixConn
	}
	commands := map[string]*commandState{
		"concurrent-first":  {handle: first, done: make(chan commandResult, 1)},
		"concurrent-second": {handle: second, done: make(chan commandResult, 1)},
	}
	expiry := time.Now().Add(time.Minute)
	for reference, command := range commands {
		start := requestScenarioLifecycle(t, ctx, command.handle, reference, func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return command.handle.Start(ctx, secondboxclient.StartSandboxRequest{AttributedExecution: &contracts.AttributedExecutionRequest{AuthorizationRef: reference, ExpiresAt: expiry}}, options)
		})
		waitForScenarioOperation(t, ctx, fixture.subject, start)
		command.ready = waitForSandbox(t, ctx, command.handle, secondboxclient.SandboxStateReady)
		if command.ready.Instance == nil {
			t.Fatal("concurrent attributed start returned no Instance")
		}
	}
	for reference, command := range commands {
		key := uniqueScenarioKey(t, reference)
		go func() {
			request := scenarioExecRequest(`endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf 'guest-context: other-account\n' | nc -w 30 "${endpoint%:*}" "${endpoint##*:}"`, 1024)
			request.DeadlineMilliseconds = 45000
			outcome, err := command.handle.Execute(ctx, request, key, "")
			command.done <- commandResult{outcome: outcome, err: err}
		}()
	}
	if err := gateway.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for range commands {
		connection, err := gateway.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(expiry); err != nil {
			t.Fatal(err)
		}
		actual, err := egressattribution.ReadRunnerExecutionAttribution(connection, 0, expiry)
		if err != nil {
			t.Fatal(err)
		}
		command := commands[actual.AuthorizationRef]
		if command == nil || command.connection != nil || actual.SandboxID != command.ready.ID || actual.InstanceID != command.ready.Instance.ID || actual.Generation != command.ready.Generation || actual.TenantRef != os.Getenv("SECONDBOX_SCENARIO_TENANT_REF") || actual.SubjectRef != os.Getenv("SECONDBOX_SCENARIO_SUBJECT_REF") || actual.AssignmentID == "" {
			t.Fatalf("concurrent attributed connection mixed authority: %+v", actual)
		}
		guestBytes := make([]byte, len("guest-context: other-account\n"))
		if _, err := io.ReadFull(connection, guestBytes); err != nil || string(guestBytes) != "guest-context: other-account\n" {
			t.Fatalf("concurrent attributed guest bytes: %q %v", guestBytes, err)
		}
		command.connection = connection
	}
	for _, reference := range []string{"concurrent-first", "concurrent-second"} {
		command := commands[reference]
		if _, err := command.connection.Write([]byte(reference)); err != nil {
			t.Fatal(err)
		}
		if err := command.connection.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-command.done:
			if result.err != nil {
				t.Fatal(result.err)
			}
			assertScenarioExited(t, result.outcome, 0, reference, "")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		stopped := waitForSandbox(t, ctx, command.handle, secondboxclient.SandboxStateStopped)
		if stopped.Instance != nil || stopped.DesiredState != contracts.SandboxDesiredStateStopped || stopped.Generation <= command.ready.Generation {
			t.Fatalf("concurrent attributed command did not retire: %+v", stopped)
		}
	}
}

// A missing gateway fails before guest readiness. The failed generation must
// remain fenced until the Runner retires it, then public lifecycle intents
// must progress without manual database repair.
func TestScenarioStartupFailureLifecycleRecovery(t *testing.T) {
	for _, action := range []string{"start", "stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			fixture, handle, gateway := newScenarioAttributedSandbox(t)
			if err := gateway.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			start := requestScenarioLifecycle(t, ctx, handle, "missing-gateway", func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Start(ctx, secondboxclient.StartSandboxRequest{
					AttributedExecution: &contracts.AttributedExecutionRequest{
						AuthorizationRef: "scenario-missing-gateway", ExpiresAt: time.Now().Add(time.Minute),
					},
				}, options)
			})
			_, err := fixture.subject.WaitOperation(ctx, start.ID, 100*time.Millisecond)
			var failure *secondboxclient.OperationFailure
			if !errors.As(err, &failure) || failure.Operation.State != contracts.OperationStateFailed || failure.Operation.Error == nil || failure.Operation.Error.Code != "startup_failed" {
				t.Fatalf("missing gateway error=%v", err)
			}
			failed := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateFailed)
			if failed.Instance == nil {
				t.Fatal("failed generation lost its Instance before Runner cleanup")
			}

			// The next start is ordinary: it must not inherit attribution from
			// the failed command, or get parked when that command is retired.
			recovery := requestScenarioLifecycle(t, ctx, handle, "recover-"+action, func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				switch action {
				case "start":
					return handle.Start(ctx, secondboxclient.StartSandboxRequest{}, options)
				case "stop":
					return handle.Stop(ctx, options)
				default:
					return handle.Delete(ctx, options)
				}
			})
			profile := createScenarioProfile(t, fixture, "scenario-lifecycle", scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning))
			other, created := createScenarioSandbox(t, fixture, profile, "after-startup-failure")
			waitForScenarioOperation(t, ctx, fixture.subject, created)
			waitForSandbox(t, ctx, other, secondboxclient.SandboxStateReady)
			waitForScenarioOperation(t, ctx, fixture.subject, recovery)
			wanted := secondboxclient.SandboxStateStopped
			if action == "start" {
				wanted = secondboxclient.SandboxStateReady
			} else if action == "delete" {
				wanted = secondboxclient.SandboxStateDeleted
			}
			recovered := waitForSandbox(t, ctx, handle, wanted)
			if recovered.Generation <= failed.Generation {
				t.Fatalf("recovery reused failed generation: before=%+v after=%+v", failed, recovered)
			}
			if action == "start" {
				if recovered.Instance == nil || recovered.Instance.ID == failed.Instance.ID {
					t.Fatalf("recovery reused failed Instance: %+v", recovered)
				}
				assertScenarioExited(t, executeScenarioCommand(t, ctx, handle, "printf recovered", 1024, "recovered-exec"), 0, "recovered", "")
			} else if recovered.Instance != nil {
				t.Fatalf("retired Sandbox still has an Instance: %+v", recovered)
			}
		})
	}
}
