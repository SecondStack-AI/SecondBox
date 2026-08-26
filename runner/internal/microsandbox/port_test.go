package microsandbox

import (
	"context"
	"strings"
	"testing"
)

// TestHelperPortConnectionRejectsUseAfterCloseAndCancellation proves the
// guards that keep a racing caller from writing a stale request ID onto the
// shared helper channel after Close released its serialization.
func TestHelperPortConnectionRejectsUseAfterCloseAndCancellation(t *testing.T) {
	connection := &helperPortConnection{}
	connection.closed.Store(true)
	if err := connection.Write(context.Background(), []byte("stale")); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("write after close = %v", err)
	}
	if _, err := connection.Read(context.Background(), 1); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("read after close = %v", err)
	}

	open := &helperPortConnection{}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := open.Write(cancelled, []byte("late")); err != context.Canceled {
		t.Fatalf("cancelled write = %v", err)
	}
	if _, err := open.Read(cancelled, 1); err != context.Canceled {
		t.Fatalf("cancelled read = %v", err)
	}
}
