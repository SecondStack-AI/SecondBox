package scheduler

import (
	"context"
	"testing"
	"time"
)

// Full jitter draws from [0, window]. A fixed or purely exponential delay makes
// contention worse, because every loser of the same race wakes at the same
// instant and collides again.
func TestSerializationBackoffDrawsWithinAnExponentialWindow(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		window := serializationBackoffBase << attempt
		if window > serializationBackoffCeiling {
			window = serializationBackoffCeiling
		}
		for draw := 0; draw < 200; draw++ {
			delay := serializationBackoff(attempt)
			if delay < 0 || delay > window {
				t.Fatalf(
					"attempt %d drew %v outside [0, %v]", attempt, delay, window,
				)
			}
		}
	}
}

func TestSerializationBackoffIsCapped(t *testing.T) {
	for _, attempt := range []int{20, 60, 1000} {
		for draw := 0; draw < 50; draw++ {
			if delay := serializationBackoff(attempt); delay > serializationBackoffCeiling {
				t.Fatalf("attempt %d drew %v above the ceiling", attempt, delay)
			}
		}
	}
}

// Successive attempts must be able to wait longer, or the backoff is not
// backing off. Full jitter makes any single pair of draws unordered, so this
// compares the observed maximum across many draws.
func TestSerializationBackoffWindowGrowsWithAttempts(t *testing.T) {
	widest := func(attempt int) time.Duration {
		var highest time.Duration
		for draw := 0; draw < 400; draw++ {
			if delay := serializationBackoff(attempt); delay > highest {
				highest = delay
			}
		}
		return highest
	}
	early, later := widest(0), widest(5)
	if later <= early {
		t.Fatalf("attempt 5 peaked at %v, not above attempt 0 at %v", later, early)
	}
}

func TestSleepWithContextStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepWithContext(ctx, time.Hour) {
		t.Fatal("a cancelled context still slept the full delay")
	}
}

func TestSleepWithContextCompletesAShortDelay(t *testing.T) {
	if !sleepWithContext(context.Background(), time.Millisecond) {
		t.Fatal("a short delay did not complete")
	}
}
