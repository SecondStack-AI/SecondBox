package scenarioharness

import (
	"context"
	"fmt"
	"time"
)

// RestartService bounds both start attempts and state inspection by ctx. Each
// attempt gets half the remaining budget, leaving time for one retry even when
// the first start command hangs. Callers retain command output for diagnostics.
func RestartService(ctx context.Context, start func(context.Context) error, running func(context.Context) (bool, error)) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fmt.Errorf("SecondBox scenario restart requires a deadline")
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		budget := time.Until(deadline) / time.Duration(2-attempt)
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		startErr := start(attemptCtx)
		last = startErr
		for attemptCtx.Err() == nil {
			ready, err := running(attemptCtx)
			if err != nil {
				last = err
			}
			if ready && err == nil && startErr == nil {
				cancel()
				return nil
			}
			select {
			case <-attemptCtx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
		cancel()
	}
	return fmt.Errorf("SecondBox scenario service did not return to running after two starts: %v (last command error: %v)", ctx.Err(), last)
}
