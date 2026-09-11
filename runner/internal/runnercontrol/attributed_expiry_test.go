package runnercontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestAttributedAssignmentExpiresWithoutGuestCompletion(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed_stop", true: "failed_stop"}[failed], func(t *testing.T) {
			backend := &attributedDisconnectBackend{recordingAssignmentBackend: recordingAssignmentBackend{
				instance: BackendInstance{BackendKind: "firecracker", BackendReference: "instance"},
			}}
			failure := errors.New("test expiry teardown failure")
			if failed {
				backend.fenceErr = failure
			}
			service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{})
			if err != nil {
				t.Fatal(err)
			}
			assignment := resolvedAssignmentCommand()
			assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
			expiresAt := time.Now().Add(200 * time.Millisecond)
			assignment.AttributedExecution = &runnerprotocol.AttributedExecution{AuthorizationRef: "expires", ExpiresAtUnixMs: uint64(expiresAt.UnixMilli())}
			t.Cleanup(func() { service.removeActiveAssignment(assignment.Fence.AssignmentId) })
			if err := service.handleAssignment(t.Context(), &recordingProtocolStream{}, assignment); err != nil {
				t.Fatal(err)
			}
			if len(service.activeAssignments()) != 1 {
				t.Fatal("assignment was not admitted")
			}
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case err := <-service.attributedFailures:
					if !failed || !errors.Is(err, failure) {
						t.Fatalf("expiry failure: %v", err)
					}
					if len(service.activeAssignments()) != 1 {
						t.Fatal("unconfirmed stop removed advertised assignment")
					}
					return
				case <-ticker.C:
					if !failed && len(service.activeAssignments()) == 0 {
						if time.Now().Before(expiresAt.Add(-time.Millisecond)) || backend.fenceCalls.Load() != 1 {
							t.Fatal("incorrect expiry fence timing or count")
						}
						return
					}
				case <-deadline.C:
					t.Fatal("host expiry waited for guest completion")
				}
			}
		})
	}
}

func TestAttributedExplicitFenceCancelsHostExpiry(t *testing.T) {
	backend := &attributedDisconnectBackend{recordingAssignmentBackend: recordingAssignmentBackend{instance: BackendInstance{BackendKind: "firecracker", BackendReference: "instance"}}}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{})
	if err != nil {
		t.Fatal(err)
	}
	assignment := resolvedAssignmentCommand()
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
	expiresAt := time.Now().Add(200 * time.Millisecond)
	assignment.AttributedExecution = &runnerprotocol.AttributedExecution{AuthorizationRef: "stopped", ExpiresAtUnixMs: uint64(expiresAt.UnixMilli())}
	if err := service.handleAssignment(t.Context(), &recordingProtocolStream{}, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := service.fenceAssignment(context.Background(), &runnerprotocol.FenceCommand{Fence: assignment.Fence}); err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Until(expiresAt.Add(25 * time.Millisecond)))
	if backend.fenceCalls.Load() != 1 || len(service.activeAssignments()) != 0 {
		t.Fatal("explicit fence retained its expiry timer")
	}
}
