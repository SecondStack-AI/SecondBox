package main

import (
	"testing"
	"time"
)

func TestBurstOffersEveryArrivalAtOnce(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "burst-8", Kind: patternBurst, Count: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.offeredCount() != 8 {
		t.Fatalf("burst offered %d arrivals, want 8", schedule.offeredCount())
	}
	for index, offset := range schedule.offsets {
		if offset != 0 {
			t.Fatalf("burst arrival %d scheduled at %s, want zero", index, offset)
		}
	}
}

func TestSteadyFixedArrivalsMatchTheConfiguredRate(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "steady-2", Kind: patternSteady, ArrivalsPerSecond: 2,
		DurationSeconds: 10, Distribution: distributionFixed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.offeredCount() != 20 {
		t.Fatalf("steady offered %d arrivals, want 20", schedule.offeredCount())
	}
	for index := 1; index < schedule.offeredCount(); index++ {
		gap := schedule.offsets[index] - schedule.offsets[index-1]
		if gap != 500*time.Millisecond {
			t.Fatalf("steady gap %d = %s, want 500ms", index, gap)
		}
	}
}

// The Poisson schedule must be reproducible from its configured seed, otherwise
// a reported run cannot be replayed.
func TestSteadyPoissonIsReproducibleFromItsSeed(t *testing.T) {
	pattern := arrivalPattern{
		Name: "steady-poisson", Kind: patternSteady, ArrivalsPerSecond: 4,
		DurationSeconds: 30, Distribution: distributionPoisson, PoissonSeed: 20260730,
	}
	first, err := buildArrivalSchedule(pattern)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildArrivalSchedule(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if first.offeredCount() != second.offeredCount() {
		t.Fatalf("poisson counts differ: %d and %d", first.offeredCount(), second.offeredCount())
	}
	for index := range first.offsets {
		if first.offsets[index] != second.offsets[index] {
			t.Fatalf("poisson arrival %d differs: %s and %s",
				index, first.offsets[index], second.offsets[index])
		}
	}
	if first.offeredCount() == 0 {
		t.Fatal("poisson schedule produced no arrivals")
	}
	for index := 1; index < first.offeredCount(); index++ {
		if first.offsets[index] < first.offsets[index-1] {
			t.Fatalf("poisson arrivals are not monotonic at %d", index)
		}
	}
}

// A ramp must accelerate: later inter-arrival gaps are strictly smaller than
// earlier ones, which is what lets the pattern find the knee.
func TestRampAcceleratesArrivals(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "ramp", Kind: patternRamp,
		StartArrivalsPerSecond: 1, EndArrivalsPerSecond: 8, DurationSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.offeredCount() < 10 {
		t.Fatalf("ramp offered only %d arrivals", schedule.offeredCount())
	}
	firstGap := schedule.offsets[1] - schedule.offsets[0]
	last := schedule.offeredCount() - 1
	lastGap := schedule.offsets[last] - schedule.offsets[last-1]
	if lastGap >= firstGap {
		t.Fatalf("ramp did not accelerate: first gap %s, last gap %s", firstGap, lastGap)
	}
	if schedule.window() > 60*time.Second {
		t.Fatalf("ramp window %s exceeded its configured duration", schedule.window())
	}
}

func TestSawtoothRepeatsBurstsSeparatedByQuietIntervals(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "sawtooth", Kind: patternSawtooth, Count: 3, QuietSeconds: 10, Repeats: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.offeredCount() != 12 {
		t.Fatalf("sawtooth offered %d arrivals, want 12", schedule.offeredCount())
	}
	distinct := make(map[time.Duration]int)
	for _, offset := range schedule.offsets {
		distinct[offset]++
	}
	if len(distinct) != 4 {
		t.Fatalf("sawtooth produced %d bursts, want 4", len(distinct))
	}
	for offset, count := range distinct {
		if count != 3 {
			t.Fatalf("sawtooth burst at %s had %d arrivals, want 3", offset, count)
		}
	}
}

func TestUnknownPatternKindIsRejected(t *testing.T) {
	if _, err := buildArrivalSchedule(arrivalPattern{Name: "bogus", Kind: "spiral"}); err == nil {
		t.Fatal("unknown pattern kind was accepted")
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	samples := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 50 * time.Millisecond,
	}
	if got := percentile(samples, 0.50); got != 30*time.Millisecond {
		t.Fatalf("p50 = %s, want 30ms", got)
	}
	if got := percentile(samples, 0.95); got != 50*time.Millisecond {
		t.Fatalf("p95 = %s, want 50ms", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty percentile = %s, want zero", got)
	}
}
