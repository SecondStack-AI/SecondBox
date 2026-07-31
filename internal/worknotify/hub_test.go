package worknotify

import (
	"testing"
)

func TestHubCoalescesMatchingWakeups(t *testing.T) {
	hub := NewHub()
	wakeups, cancel := hub.Subscribe(KindRunnerCommand, "runner-one")
	defer cancel()

	hub.Publish(KindRunnerCommand, "runner-two")
	select {
	case <-wakeups:
		t.Fatal("unmatched runner notification woke subscription")
	default:
	}

	hub.Publish(KindRunnerCommand, "runner-one")
	hub.Publish(KindRunnerCommand, "runner-one")
	select {
	case <-wakeups:
	default:
		t.Fatal("matching notification did not wake subscription")
	}
	select {
	case <-wakeups:
		t.Fatal("matching notifications were not coalesced")
	default:
	}
}

func TestHubCancellationIsIdempotent(t *testing.T) {
	hub := NewHub()
	wakeups, cancel := hub.Subscribe(KindLifecycle, "")
	cancel()
	cancel()
	hub.Publish(KindLifecycle, "")
	select {
	case <-wakeups:
		t.Fatal("cancelled subscription received a notification")
	default:
	}
}

func TestDecodePostgresPayloadRejectsInvalidAuthority(t *testing.T) {
	for _, encoded := range []string{
		`{"kind":"runner_command","key":""}`,
		`{"kind":"lifecycle","key":"runner-one"}`,
		`{"kind":"unknown","key":""}`,
		`{"kind":"assignment","key":"","extra":true}`,
		`{"kind":"assignment","key":""} trailing`,
		`{"kind":"data_plane_session","key":""}`,
	} {
		if _, err := decodePostgresPayload(encoded); err == nil {
			t.Fatalf("decodePostgresPayload(%q) succeeded", encoded)
		}
	}
	payload, err := decodePostgresPayload(`{"kind":"runner_command","key":"runner-one"}`)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Kind != KindRunnerCommand || payload.Key != "runner-one" {
		t.Fatalf("payload = %#v", payload)
	}
	// A replica that cannot decode a kind stops rather than dropping a hint, so
	// accepting this kind has to ship before anything emits it.
	session, err := decodePostgresPayload(`{"kind":"data_plane_session","key":"dps_1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if session.Kind != KindDataPlaneSession || session.Key != "dps_1" {
		t.Fatalf("data-plane payload = %#v", session)
	}
}
