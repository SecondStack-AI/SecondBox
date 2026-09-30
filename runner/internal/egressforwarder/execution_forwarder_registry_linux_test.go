package egressforwarder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An interface sweep closes that interface's live listeners before it deletes
// the tables that restrict them, and leaves other interfaces' listeners open.
func TestExecutionForwarderRegistryRevokesOnlySweptInterfaces(t *testing.T) {
	registry := executionForwarderRegistry{forwarders: make(map[*ExecutionForwarder]struct{})}
	start := func(guestInterface string) *ExecutionForwarder {
		ctx, cancel := context.WithCancel(context.Background())
		forwarder := &ExecutionForwarder{policy: ExecutionListenerPolicy{GuestInterface: guestInterface}, cancel: cancel, done: make(chan struct{})}
		registry.add(forwarder)
		go func() {
			<-ctx.Done()
			registry.remove(forwarder)
			close(forwarder.done)
		}()
		return forwarder
	}
	swept, sibling, other := start("tap0"), start("tap0"), start("tap1")
	defer other.Revoke()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := registry.revoke(ctx, map[string]bool{"tap0": true}); err != nil {
		t.Fatal(err)
	}
	for _, forwarder := range []*ExecutionForwarder{swept, sibling} {
		select {
		case <-forwarder.done:
		default:
			t.Fatal("sweep returned before a live listener on its interface stopped")
		}
	}
	select {
	case <-other.done:
		t.Fatal("sweep revoked another interface's listener")
	default:
	}
}

// A sweep that begins while a forwarder is starting waits for its registration
// and revokes it; it never deletes a table whose listener it did not close.
func TestExecutionListenerSweepWaitsForStartingForwarder(t *testing.T) {
	nft := filepath.Join(t.TempDir(), "nft")
	if err := os.WriteFile(nft, []byte("#!/bin/sh\nprintf '{\"nftables\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executionListenerTablesMu.Lock()
	swept := make(chan error, 1)
	go func() { swept <- RemoveExecutionListenerRules(t.Context(), nft, []string{"tapstart"}) }()
	select {
	case err := <-swept:
		executionListenerTablesMu.Unlock()
		t.Fatalf("sweep ran during a forwarder startup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithCancel(context.Background())
	forwarder := &ExecutionForwarder{policy: ExecutionListenerPolicy{GuestInterface: "tapstart"}, cancel: cancel, done: make(chan struct{})}
	liveExecutionForwarders.add(forwarder)
	go func() {
		<-ctx.Done()
		liveExecutionForwarders.remove(forwarder)
		close(forwarder.done)
	}()
	executionListenerTablesMu.Unlock()
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	select {
	case <-forwarder.done:
	default:
		t.Fatal("sweep missed a forwarder that registered during it")
	}
}
