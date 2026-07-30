package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// arrivalSchedule is the offered load for one pattern, expressed as offsets from
// the start of the measurement window. The schedule is computed up front and is
// never adjusted by observed latency: that is what makes the benchmark open
// loop. A closed-loop driver stops offering work when the system slows down and
// therefore can never show a queue growing.
type arrivalSchedule struct {
	name    string
	kind    string
	offsets []time.Duration
}

func (schedule arrivalSchedule) offeredCount() int {
	return len(schedule.offsets)
}

// window is the span the pattern intends to cover. It bounds the occupancy
// sampler and is reported alongside the completed-rate so offered and achieved
// throughput can be compared directly.
func (schedule arrivalSchedule) window() time.Duration {
	if len(schedule.offsets) == 0 {
		return 0
	}
	return schedule.offsets[len(schedule.offsets)-1]
}

func buildArrivalSchedule(pattern arrivalPattern) (arrivalSchedule, error) {
	schedule := arrivalSchedule{name: pattern.Name, kind: pattern.Kind}
	switch pattern.Kind {
	case patternBurst:
		// Every arrival is offered at once. This measures absorption and the
		// time required to drain a standing burst.
		schedule.offsets = make([]time.Duration, pattern.Count)
		for index := range schedule.offsets {
			schedule.offsets[index] = 0
		}
	case patternSteady:
		offsets, err := steadyOffsets(pattern)
		if err != nil {
			return arrivalSchedule{}, err
		}
		schedule.offsets = offsets
	case patternRamp:
		schedule.offsets = rampOffsets(pattern)
	case patternSawtooth:
		schedule.offsets = sawtoothOffsets(pattern)
	default:
		return arrivalSchedule{}, fmt.Errorf(
			"SecondBox lifecycle cannot schedule unknown pattern kind %q", pattern.Kind,
		)
	}
	if len(schedule.offsets) == 0 {
		return arrivalSchedule{}, fmt.Errorf(
			"SecondBox lifecycle pattern %q produced no arrivals", pattern.Name,
		)
	}
	return schedule, nil
}

func steadyOffsets(pattern arrivalPattern) ([]time.Duration, error) {
	window := time.Duration(pattern.DurationSeconds) * time.Second
	switch pattern.Distribution {
	case distributionFixed:
		interval := time.Duration(float64(time.Second) / pattern.ArrivalsPerSecond)
		if interval <= 0 {
			return nil, fmt.Errorf(
				"SecondBox lifecycle steady %q arrival interval collapsed to zero", pattern.Name,
			)
		}
		var offsets []time.Duration
		for offset := time.Duration(0); offset < window; offset += interval {
			offsets = append(offsets, offset)
		}
		return offsets, nil
	case distributionPoisson:
		// Exponential inter-arrival times reproduce bursty real-world arrival,
		// which a fixed interval cannot. The seed is mandatory configuration so
		// a reported run can be replayed exactly.
		source := rand.New(rand.NewSource(pattern.PoissonSeed))
		var offsets []time.Duration
		offset := time.Duration(0)
		for {
			gap := source.ExpFloat64() / pattern.ArrivalsPerSecond
			offset += time.Duration(gap * float64(time.Second))
			if offset >= window {
				break
			}
			offsets = append(offsets, offset)
		}
		return offsets, nil
	default:
		return nil, fmt.Errorf(
			"SecondBox lifecycle steady %q has unknown distribution %q", pattern.Name, pattern.Distribution,
		)
	}
}

// rampOffsets increases the arrival rate linearly across the window. Integrating
// the rate gives the arrival times directly, so the pattern finds the knee
// empirically instead of requiring the operator to guess concurrency levels.
func rampOffsets(pattern arrivalPattern) []time.Duration {
	window := float64(pattern.DurationSeconds)
	start := pattern.StartArrivalsPerSecond
	end := pattern.EndArrivalsPerSecond
	slope := (end - start) / window
	var offsets []time.Duration
	elapsed := 0.0
	for {
		rate := start + slope*elapsed
		if rate <= 0 {
			break
		}
		elapsed += 1 / rate
		if elapsed >= window {
			break
		}
		offsets = append(offsets, time.Duration(elapsed*float64(time.Second)))
	}
	return offsets
}

// sawtoothOffsets alternates a burst with a quiet interval, proving that
// capacity is actually released between bursts rather than degrading
// monotonically.
func sawtoothOffsets(pattern arrivalPattern) []time.Duration {
	var offsets []time.Duration
	quiet := time.Duration(pattern.QuietSeconds) * time.Second
	for repeat := 0; repeat < pattern.Repeats; repeat++ {
		base := time.Duration(repeat) * quiet
		for index := 0; index < pattern.Count; index++ {
			offsets = append(offsets, base)
		}
	}
	return offsets
}

// arrivalRateAt reports the instantaneous offered rate of a schedule inside a
// one-second window, used to correlate latency with offered load in the report.
func arrivalRateAt(schedule arrivalSchedule, at time.Duration) float64 {
	if len(schedule.offsets) == 0 {
		return 0
	}
	lower := at - time.Second/2
	upper := at + time.Second/2
	count := 0
	for _, offset := range schedule.offsets {
		if offset >= lower && offset < upper {
			count++
		}
	}
	return float64(count)
}

func percentile(samples []time.Duration, fraction float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	if fraction <= 0 {
		return samples[0]
	}
	index := int(math.Ceil(fraction*float64(len(samples)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(samples) {
		index = len(samples) - 1
	}
	return samples[index]
}
