package runtimemanager

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttributedExecutionGuardSingleExec(t *testing.T) {
	expiry := time.Now().Add(time.Minute)
	guard, err := NewAttributedExecutionGuard("assignment", expiry)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		assignment string
		deadline   time.Time
	}{
		{"other", expiry}, {"assignment", expiry.Add(time.Second)}, {"assignment", time.Now().Add(-time.Second)},
	} {
		if err := guard.AdmitExec(test.assignment, test.deadline); err == nil {
			t.Fatal("invalid exec admitted")
		}
	}
	var winners atomic.Int32
	var attempts sync.WaitGroup
	for range 20 {
		attempts.Go(func() {
			if guard.AdmitExec("assignment", expiry) == nil {
				winners.Add(1)
			}
		})
	}
	attempts.Wait()
	if winners.Load() != 1 {
		t.Fatalf("admitted %d execs", winners.Load())
	}
	if err := guard.AdmitRead("assignment"); err != nil {
		t.Fatal(err)
	}
	if err := guard.AdmitRead("other"); err == nil {
		t.Fatal("foreign read admitted")
	}
}

func TestAttributedExecutionGuardExpiry(t *testing.T) {
	guard := &AttributedExecutionGuard{assignmentID: "assignment", expiresAt: time.Now().Add(-time.Second)}
	if err := guard.AdmitRead("assignment"); err == nil {
		t.Fatal("expired read admitted")
	}
	if err := guard.AdmitExec("assignment", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expired exec admitted")
	}
	for _, test := range []struct {
		assignment string
		expiry     time.Time
	}{
		{"", time.Now().Add(time.Minute)}, {"assignment", time.Time{}},
	} {
		if _, err := NewAttributedExecutionGuard(test.assignment, test.expiry); err == nil {
			t.Fatal("invalid guard constructed")
		}
	}
}
