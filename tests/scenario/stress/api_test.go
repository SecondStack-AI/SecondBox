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
