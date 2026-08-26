package microsandbox

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestHelperPortConnectionRejectsUseAfterCloseAndCancellation proves the
// guards that keep a racing caller from writing a stale request ID onto the
// shared helper channel after Close released its serialization.
func TestHelperPortConnectionRejectsUseAfterCloseAndCancellation(t *testing.T) {
	connection := &helperPortConnection{
		readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1),
	}
	connection.closed.Store(true)
	if err := connection.Write(context.Background(), []byte("stale")); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("write after close = %v", err)
	}
	if _, err := connection.Read(context.Background(), 1); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("read after close = %v", err)
	}

	open := &helperPortConnection{
		readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1),
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := open.Write(cancelled, []byte("late")); err != context.Canceled {
		t.Fatalf("cancelled write = %v", err)
	}
	if _, err := open.Read(cancelled, 1); err != context.Canceled {
		t.Fatalf("cancelled read = %v", err)
	}
}

// TestHelperPortConnectionWriteCancellationInterruptsBlockedFrames proves a
// cancelled context interrupts a frame write blocked on the shared helper
// connection instead of holding the serialization for the fallback deadline.
func TestHelperPortConnectionWriteCancellationInterruptsBlockedFrames(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	connection := &helperPortConnection{
		process: &helperProcess{control: local}, nextSequence: 1,
		readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() {
		completed <- connection.Write(ctx, []byte("blocked-frame"))
	}()
	select {
	case err := <-completed:
		t.Fatalf("write completed without a reader: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("cancelled blocked write returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt the blocked frame write")
	}
}
