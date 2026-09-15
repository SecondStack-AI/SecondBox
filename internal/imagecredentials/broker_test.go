package imagecredentials

import (
	"testing"
	"time"
)

func TestImageCredentialBrokerExpiresOperationBinding(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	broker, err := NewBroker(func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	credentials := PullCredentials{Username: "puller", Token: "pull-token"}
	if err := broker.Store("operation-1", credentials); err != nil {
		t.Fatal(err)
	}
	if got, found := broker.Load("operation-1"); !found || got != credentials {
		t.Fatalf("loaded credentials = %#v, %v", got, found)
	}
	now = now.Add(time.Minute)
	if _, found := broker.Load("operation-1"); found {
		t.Fatal("expired image credentials remain available")
	}
}

func TestImageCredentialBrokerReplacesIdempotentReplayBinding(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	broker, err := NewBroker(func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Store("operation-1", PullCredentials{Token: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := broker.Store("operation-1", PullCredentials{Token: "refreshed"}); err != nil {
		t.Fatal(err)
	}
	got, found := broker.Load("operation-1")
	if !found || got.Token != "refreshed" {
		t.Fatalf("refreshed credentials = %#v, %v", got, found)
	}
}
