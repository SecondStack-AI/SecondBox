package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// A Sandbox the create Operation named but whose representation could not be
// fetched still exists on the deployment. Cell cleanup can only delete handles
// it was given, so createSandbox must hand one back even on that error path — a
// leaked Sandbox counts against subjectMaxSandboxes for as long as it lives, and
// enough of them manufacture a refusal that reads as saturation.
func TestSandboxHandleAddressedByIDIsEnoughToCleanUp(t *testing.T) {
	handle := secondboxclient.NewSandboxHandle(
		nil, secondboxclient.Sandbox{ID: "sandbox-01HZY"},
	)
	if handle == nil {
		t.Fatal("no handle was built from a known Sandbox identifier")
	}
	if handle.Snapshot().ID != "sandbox-01HZY" {
		t.Fatalf("handle identifier = %q", handle.Snapshot().ID)
	}
}

// cellResources must skip nil rather than record it, so a create that genuinely
// produced nothing cannot put a nil into the cleanup list.
func TestCellResourcesSkipsAbsentHandles(t *testing.T) {
	resources := &cellResources{}
	resources.add(nil)
	if len(resources.snapshot()) != 0 {
		t.Fatalf("cell resources recorded an absent handle: %d", len(resources.snapshot()))
	}
	resources.add(secondboxclient.NewSandboxHandle(
		nil, secondboxclient.Sandbox{ID: "sandbox-01HZZ"},
	))
	if len(resources.snapshot()) != 1 {
		t.Fatalf("cell resources = %d handles, want 1", len(resources.snapshot()))
	}
}

// Cleanup must survive the connection churn a saturated deployment produces.
// When the server times out a long poll it closes the connection, so the next
// request on a pooled keep-alive connection is reset -- precisely when a
// capacity run is under strain. Giving up there leaks the Sandbox, and leaked
// Sandboxes count against the subject quota for the rest of the ladder.
func TestTransientTransportErrorsAreDistinguishedFromAnswers(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "connection reset", err: syscall.ECONNRESET, transient: true},
		{name: "broken pipe", err: syscall.EPIPE, transient: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, transient: true},
		{
			name:      "wrapped reset",
			err:       fmt.Errorf("send getSandbox request: %w", syscall.ECONNRESET),
			transient: true,
		},
		{name: "no error", err: nil, transient: false},
		{
			name:      "a plain answer is not transient",
			err:       errors.New("SecondBox lifecycle Sandbox reached failed instead of ready"),
			transient: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isTransientTransportError(testCase.err); got != testCase.transient {
				t.Fatalf("transient = %v, want %v for %v", got, testCase.transient, testCase.err)
			}
		})
	}
}
