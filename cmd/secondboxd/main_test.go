package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestConfiguredRunnerFeaturesRequireLocalWorkspace(t *testing.T) {
	if _, err := configuredRunnerFeatures([]string{"evidence"}); err == nil ||
		!strings.Contains(err.Error(), "require local-workspace") {
		t.Fatalf("portable-only runner feature config error = %v", err)
	}
	features, err := configuredRunnerFeatures([]string{"evidence", "local-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 2 ||
		features[1] != runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE {
		t.Fatalf("configured features = %v", features)
	}
}

func TestRetryablePostgresContentionDoesNotTreatStoreErrorsAsFatal(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("wrapped deadlock: %w", &pgconn.PgError{Code: "40P01"}),
		fmt.Errorf("wrapped serialization failure: %w", &pgconn.PgError{Code: "40001"}),
		fmt.Errorf("wrapped store contention: %w", ports.ErrSerializationContention),
	} {
		if !retryablePostgresContention(err) {
			t.Fatalf("retryablePostgresContention(%v) = false", err)
		}
	}
	if retryablePostgresContention(errors.New("connection failed")) {
		t.Fatal("an unrelated store error was treated as retryable contention")
	}
}
