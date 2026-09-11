package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type attributedAssignmentLifetime struct {
	stopMu sync.Mutex
	timer  *time.Timer
}

func (s *RunnerProtocolService) armAttributedAssignmentExpiry(ctx context.Context, assignment *runnerprotocol.AssignmentCommand) {
	if assignment.AttributedExecution == nil {
		return
	}
	fence := proto.CloneOf(assignment.Fence)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.attributedLifetimes == nil {
		s.attributedLifetimes = make(map[string]*attributedAssignmentLifetime)
	}
	if s.attributedLifetimes[fence.AssignmentId] != nil {
		return
	}
	lifetime := &attributedAssignmentLifetime{}
	s.attributedLifetimes[fence.AssignmentId] = lifetime
	expiresAt := time.UnixMilli(int64(assignment.AttributedExecution.ExpiresAtUnixMs))
	lifetime.timer = time.AfterFunc(time.Until(expiresAt), func() {
		s.directPorts.closeAssignment(fence.AssignmentId, "attributed execution expired")
		s.directDataPlane.closeAssignment(fence.AssignmentId, "attributed execution expired")
		_, err := s.fenceAssignment(context.WithoutCancel(ctx), &runnerprotocol.FenceCommand{Fence: fence})
		if err != nil {
			select {
			case s.attributedFailures <- fmt.Errorf("attributed execution expiry: %w", err):
			case <-ctx.Done():
				// Disconnection independently retries and reports any failed teardown.
			}
		}
	})
}

func (s *RunnerProtocolService) fenceAssignment(ctx context.Context, command *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	s.stateMu.Lock()
	lifetime := s.attributedLifetimes[command.Fence.AssignmentId]
	s.stateMu.Unlock()
	if lifetime != nil {
		lifetime.stopMu.Lock()
		defer lifetime.stopMu.Unlock()
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	evidence, err := s.backend.FenceAssignment(ctx, command)
	if err == nil && lifetime != nil && evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED && evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED {
		err = fmt.Errorf("attributed execution termination is unconfirmed: %s", evidence.Result)
	}
	if err == nil && lifetime != nil {
		s.removeActiveAssignment(command.Fence.AssignmentId)
	}
	return evidence, err
}

// Guest completion only requests teardown. Cancellation also tears down compute
// directly, without waiting for the guest to acknowledge its cancellation frame.
func (s *RunnerProtocolService) beginAttributedExec(ctx context.Context, fence *runnerprotocol.AssignmentFence, deadlineUnixMs uint64) (context.Context, func() error) {
	s.stateMu.Lock()
	lifetime := s.attributedLifetimes[fence.AssignmentId]
	s.stateMu.Unlock()
	if lifetime == nil {
		return ctx, func() error { return nil }
	}
	execCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(int64(deadlineUnixMs)))
	stopCompute := func() error {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stopCancel()
		_, err := s.fenceAssignment(stopCtx, &runnerprotocol.FenceCommand{Fence: cloneRunnerFence(fence)})
		return err
	}
	stopped := make(chan error, 1)
	stopCancellation := context.AfterFunc(execCtx, func() { stopped <- stopCompute() })
	return execCtx, func() error {
		defer cancel()
		if stopCancellation() {
			return stopCompute()
		}
		return <-stopped
	}
}

type attributedAssignmentBackend interface {
	AttributedAssignmentFences() []*runnerprotocol.AssignmentFence
}

// Ordinary Instances can survive reconnection; attributed authority cannot.
func (s *RunnerProtocolService) fenceDisconnectedAttributedAssignments(ctx context.Context) error {
	backend, supported := s.backend.(attributedAssignmentBackend)
	if !supported {
		return nil
	}
	fences := backend.AttributedAssignmentFences()
	for _, fence := range fences {
		s.directPorts.closeAssignment(fence.AssignmentId, "attributed control-plane connection lost")
		s.directDataPlane.closeAssignment(fence.AssignmentId, "attributed control-plane connection lost")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var failures error
	for _, fence := range fences {
		evidence, err := s.fenceAssignment(cleanupCtx, &runnerprotocol.FenceCommand{Fence: fence})
		if err == nil && evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED && evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED {
			err = fmt.Errorf("attributed disconnect termination is unconfirmed: %s", evidence.Result)
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("attributed disconnect fence %q: %w", fence.AssignmentId, err))
			continue
		}
		s.removeActiveAssignment(fence.AssignmentId)
	}
	return failures
}
