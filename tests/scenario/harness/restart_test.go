package scenarioharness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRestartService(t *testing.T) {
	for _, mode := range []string{"running", "retry", "hung start", "dead", "inspect error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			attempts := 0
			err := RestartService(ctx, func(ctx context.Context) error {
				attempts++
				if mode == "hung start" && attempts == 1 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}, func(context.Context) (bool, error) {
				switch mode {
				case "running":
					return true, nil
				case "retry", "hung start":
					return attempts == 2, nil
				case "inspect error":
					return false, errors.New("inspect unavailable")
				default:
					return false, nil
				}
			})
			wantFailure := mode == "dead" || mode == "inspect error"
			if (err != nil) != wantFailure {
				t.Fatalf("restart error = %v, want failure %v", err, wantFailure)
			}
			wantAttempts := 2
			if mode == "running" {
				wantAttempts = 1
			}
			if attempts != wantAttempts {
				t.Fatalf("start attempts = %d, want %d", attempts, wantAttempts)
			}
		})
	}
}
