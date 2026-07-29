// Package observability owns fixed-cardinality in-process timing observations.
package observability

import (
	"sort"
	"sync"
	"time"
)

// DurationBucketsSeconds is the shared cumulative histogram boundary set.
var DurationBucketsSeconds = [...]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// DurationHistogram is one cumulative histogram snapshot.
type DurationHistogram struct {
	Count        uint64
	SumSeconds   float64
	BucketCounts []uint64
}

// HTTPDuration identifies one bounded route-template and status-class series.
type HTTPDuration struct {
	Route       string
	StatusClass string
	Histogram   DurationHistogram
}

type httpSeriesKey struct {
	route       string
	statusClass string
}

type durationHistogram struct {
	count        uint64
	sumSeconds   float64
	bucketCounts [len(DurationBucketsSeconds)]uint64
}

// TimingRecorder retains process-lifetime fixed-cardinality HTTP observations.
type TimingRecorder struct {
	mu   sync.RWMutex
	http map[httpSeriesKey]*durationHistogram
}

// NewTimingRecorder creates an empty process-lifetime timing recorder.
func NewTimingRecorder() *TimingRecorder {
	return &TimingRecorder{http: make(map[httpSeriesKey]*durationHistogram)}
}

// ObserveHTTP records one completed request using only bounded dimensions.
func (recorder *TimingRecorder) ObserveHTTP(
	route string,
	statusClass string,
	duration time.Duration,
) {
	if recorder == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	if duration < 0 {
		duration = 0
	}
	key := httpSeriesKey{route: route, statusClass: statusClass}
	seconds := duration.Seconds()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	histogram := recorder.http[key]
	if histogram == nil {
		histogram = &durationHistogram{}
		recorder.http[key] = histogram
	}
	histogram.count++
	histogram.sumSeconds += seconds
	for index, upperBound := range DurationBucketsSeconds {
		if seconds <= upperBound {
			histogram.bucketCounts[index]++
		}
	}
}

// HTTPSnapshot returns a stable copy sorted by route and status class.
func (recorder *TimingRecorder) HTTPSnapshot() []HTTPDuration {
	if recorder == nil {
		return []HTTPDuration{}
	}
	recorder.mu.RLock()
	defer recorder.mu.RUnlock()
	snapshot := make([]HTTPDuration, 0, len(recorder.http))
	for key, source := range recorder.http {
		buckets := make([]uint64, len(source.bucketCounts))
		copy(buckets, source.bucketCounts[:])
		snapshot = append(snapshot, HTTPDuration{
			Route: key.route, StatusClass: key.statusClass,
			Histogram: DurationHistogram{
				Count: source.count, SumSeconds: source.sumSeconds, BucketCounts: buckets,
			},
		})
	}
	sort.Slice(snapshot, func(left, right int) bool {
		if snapshot[left].Route == snapshot[right].Route {
			return snapshot[left].StatusClass < snapshot[right].StatusClass
		}
		return snapshot[left].Route < snapshot[right].Route
	})
	return snapshot
}
