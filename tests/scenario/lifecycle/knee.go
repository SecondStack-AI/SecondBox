package main

import "sort"

// kneePoint is the first rung of a ladder to show a particular kind of strain.
type kneePoint struct {
	Step            int    `json:"step"`
	Pattern         string `json:"pattern"`
	OfferedArrivals int    `json:"offeredArrivals"`
	Detail          string `json:"detail,omitempty"`
}

// capacitySummary is one ladder's outcome: how far it climbed before the
// deployment stopped absorbing the load, and what stopped it.
//
// Three knees are reported rather than one because they answer different
// questions and do not fire in a fixed order. A deployment bound by its own
// configuration refuses cleanly and never slows down; a deployment bound by the
// machine slows down, then fails, and may never refuse at all. Reporting only
// the first would hide which of those happened.
type capacitySummary struct {
	Measurement          string          `json:"measurement"`
	PatternKind          string          `json:"patternKind"`
	ResidentPopulation   int             `json:"residentPopulation"`
	Steps                int             `json:"steps"`
	LargestFullyAdmitted int             `json:"largestFullyAdmitted"`
	ConfiguredBinding    configuredLimit `json:"configuredBinding"`
	RefusalKnee          *kneePoint      `json:"refusalKnee"`
	LatencyKnee          *kneePoint      `json:"latencyKnee"`
	DistressKnee         *kneePoint      `json:"distressKnee"`
}

type ladderKey struct {
	measurement string
	patternKind string
	resident    int
}

// identifyLadders groups cells into ladders and summarises each. A ladder is a
// run of cells sharing a measurement, pattern kind and resident population whose
// offered arrivals strictly increase. Offered arrivals are the key rather than
// completed ones because what a rung asked for is a property of the schedule,
// while what it achieved is the outcome being measured.
func identifyLadders(
	results []cellResult,
	ratio float64,
	binding configuredLimit,
) []capacitySummary {
	order := make([]ladderKey, 0, len(results))
	grouped := make(map[ladderKey][]cellResult, len(results))
	for _, result := range results {
		key := ladderKey{
			measurement: result.Measurement,
			patternKind: result.PatternKind,
			resident:    result.ResidentPopulation,
		}
		if _, seen := grouped[key]; !seen {
			order = append(order, key)
		}
		last := grouped[key]
		if len(last) > 0 &&
			result.OfferedArrivals <= last[len(last)-1].OfferedArrivals {
			continue
		}
		grouped[key] = append(grouped[key], result)
	}
	summaries := make([]capacitySummary, 0, len(order))
	for _, key := range order {
		summaries = append(summaries, summariseLadder(key, grouped[key], ratio, binding))
	}
	return summaries
}

func summariseLadder(
	key ladderKey,
	rungs []cellResult,
	ratio float64,
	binding configuredLimit,
) capacitySummary {
	summary := capacitySummary{
		Measurement:        key.measurement,
		PatternKind:        key.patternKind,
		ResidentPopulation: key.resident,
		Steps:              len(rungs),
		ConfiguredBinding:  binding,
	}
	var baselineP95 int64 = -1
	for step, rung := range rungs {
		if fullyAdmitted(rung) && rung.OfferedArrivals > summary.LargestFullyAdmitted {
			summary.LargestFullyAdmitted = rung.OfferedArrivals
		}
		if summary.RefusalKnee == nil && len(rung.Refusals) > 0 {
			summary.RefusalKnee = pointAt(step, rung, dominantCode(rung.Refusals))
		}
		if summary.DistressKnee == nil {
			if rung.AbortedAtRail != "" {
				summary.DistressKnee = pointAt(step, rung, rung.AbortedAtRail)
			} else if len(rung.Failures) > 0 {
				summary.DistressKnee = pointAt(step, rung, dominantCode(rung.Failures))
			}
		}
		// A rung with no successes reports no latency at all, so it can neither
		// establish the baseline nor be compared against it.
		if rung.Latency == nil {
			continue
		}
		if baselineP95 < 0 {
			baselineP95 = rung.Latency.P95Milliseconds
			continue
		}
		if summary.LatencyKnee == nil && baselineP95 > 0 &&
			float64(rung.Latency.P95Milliseconds) >= float64(baselineP95)*ratio {
			summary.LatencyKnee = pointAt(step, rung, "")
		}
	}
	return summary
}

// fullyAdmitted reports whether a rung was absorbed without reservation: nothing
// refused, nothing failed, nothing shed by the driver, and every arrival
// completed.
func fullyAdmitted(rung cellResult) bool {
	return len(rung.Refusals) == 0 &&
		len(rung.Failures) == 0 &&
		rung.ShedArrivals == 0 &&
		rung.AbortedAtRail == "" &&
		rung.CompletedArrivals == int64(rung.OfferedArrivals)
}

func pointAt(step int, rung cellResult, detail string) *kneePoint {
	return &kneePoint{
		Step:            step,
		Pattern:         rung.Pattern,
		OfferedArrivals: rung.OfferedArrivals,
		Detail:          detail,
	}
}

// dominantCode names the most frequent code, breaking ties by name so a report
// is reproducible.
func dominantCode(counts map[string]int64) string {
	codes := make([]string, 0, len(counts))
	for code := range counts {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	dominant := ""
	var highest int64
	for _, code := range codes {
		if counts[code] > highest {
			dominant, highest = code, counts[code]
		}
	}
	return dominant
}
