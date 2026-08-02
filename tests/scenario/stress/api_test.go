package main

import (
	"errors"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestRetryableCleanupErrorIsLimitedToLifecycleRaces(t *testing.T) {
	for _, code := range []string{
		"precondition_failed",
		"workspace_mutation_conflict",
		"execution_node_unavailable",
		"home_runner_unavailable",
		"backpressure",
	} {
		if !retryableCleanupError(&secondboxclient.APIError{
			StatusCode: 409,
			Problem:    &secondboxclient.Problem{Code: code},
		}) {
			t.Fatalf("cleanup code %q was not retryable", code)
		}
	}
	if retryableCleanupError(&secondboxclient.APIError{
		StatusCode: 413,
		Problem:    &secondboxclient.Problem{Code: "limit_exceeded"},
	}) {
		t.Fatal("permanent cleanup error was retryable")
	}
	if retryableCleanupError(errors.New("transport failed")) {
		t.Fatal("untyped cleanup error was retryable")
	}
}

func TestStressAdmissionRetryDelayHonorsTypedHomeRunnerBackoff(t *testing.T) {
	retryAfterMilliseconds := int64(1000)
	delay, retry := stressAdmissionRetryDelay(&secondboxclient.APIError{
		StatusCode: 503,
		Problem: &secondboxclient.Problem{
			Code: "home_runner_unavailable", Retryable: true,
			RetryAfterMilliseconds: &retryAfterMilliseconds,
		},
	}, 25*time.Millisecond)
	if !retry || delay != time.Second {
		t.Fatalf("typed retry delay = %s, %t", delay, retry)
	}

	delay, retry = stressAdmissionRetryDelay(&secondboxclient.APIError{
		StatusCode: 503,
		Problem: &secondboxclient.Problem{
			Code: "home_runner_unavailable", Retryable: true,
		},
	}, 25*time.Millisecond)
	if !retry || delay != 25*time.Millisecond {
		t.Fatalf("fallback retry delay = %s, %t", delay, retry)
	}

	if _, retry := stressAdmissionRetryDelay(&secondboxclient.APIError{
		StatusCode: 429,
		Problem: &secondboxclient.Problem{
			Code: "quota_exceeded", Retryable: false,
		},
	}, 25*time.Millisecond); retry {
		t.Fatal("permanent admission failure was retried")
	}
}

func TestRetryStressRevisionConflictRefreshesOnlyForTypedPrecondition(t *testing.T) {
	conflict := &secondboxclient.APIError{
		StatusCode: 412,
		Problem:    &secondboxclient.Problem{Code: "precondition_failed"},
	}
	attempts := make([]int, 0, 3)
	err := retryStressRevisionConflict(t.Context(), func(attempt int) error {
		attempts = append(attempts, attempt)
		if attempt < 2 {
			return conflict
		}
		return nil
	})
	if err != nil {
		t.Fatalf("revision conflict retry failed: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("revision conflict attempts = %v, want [0 1 2]", attempts)
	}

	permanent := &secondboxclient.APIError{
		StatusCode: 404,
		Problem:    &secondboxclient.Problem{Code: "not_found"},
	}
	attempts = attempts[:0]
	err = retryStressRevisionConflict(t.Context(), func(attempt int) error {
		attempts = append(attempts, attempt)
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("permanent error = %v, want %v", err, permanent)
	}
	if len(attempts) != 1 {
		t.Fatalf("permanent error attempts = %v, want [0]", attempts)
	}
}

func TestFinishStressSnapshotCycleStartsSandboxAfterDeletion(t *testing.T) {
	state := secondboxclient.SandboxStateStopped
	deleted := false
	err := finishStressSnapshotCycle(
		func() error {
			if state != secondboxclient.SandboxStateStopped {
				t.Fatalf("Snapshot delete began in state %s", state)
			}
			deleted = true
			return nil
		},
		func() error {
			if !deleted {
				t.Fatal("Sandbox start ran before Snapshot delete completed")
			}
			state = secondboxclient.SandboxStateReady
			return nil
		},
	)
	if err != nil {
		t.Fatalf("finish Snapshot cycle failed: %v", err)
	}
	if state != secondboxclient.SandboxStateReady {
		t.Fatalf("Snapshot cycle ended in state %s, want ready", state)
	}
}
