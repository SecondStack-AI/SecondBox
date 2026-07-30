package main

import (
	"errors"
	"testing"

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
