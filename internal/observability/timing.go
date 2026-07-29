// Package observability owns fixed-cardinality in-process timing observations.
package observability

import (
	"math"
	"sort"
	"sync"
	"time"
)

// DurationBucketsSeconds is the shared cumulative histogram boundary set.
var DurationBucketsSeconds = [...]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

const (
	// HTTPWindowResolution is the fixed resolution of the bounded current-deployment view.
	HTTPWindowResolution = time.Minute
	// HTTPWindowRetention is the maximum queryable in-process API timing window.
	HTTPWindowRetention = time.Hour
)

// DurationHistogram is one cumulative histogram snapshot.
type DurationHistogram struct {
	Count          uint64
	SumSeconds     float64
	MaximumSeconds float64
	BucketCounts   []uint64
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
	count          uint64
	sumSeconds     float64
	maximumSeconds float64
	bucketCounts   [len(DurationBucketsSeconds)]uint64
}

// TimingRecorder retains process-lifetime and bounded-window HTTP observations.
type TimingRecorder struct {
	mu          sync.RWMutex
	http        map[httpSeriesKey]*durationHistogram
	httpWindows map[int64]map[httpSeriesKey]*durationHistogram
}

// NewTimingRecorder creates an empty process-lifetime timing recorder.
func NewTimingRecorder() *TimingRecorder {
	return &TimingRecorder{
		http:        make(map[httpSeriesKey]*durationHistogram),
		httpWindows: make(map[int64]map[httpSeriesKey]*durationHistogram),
	}
}

// ObserveHTTP records one completed request using only bounded dimensions.
func (recorder *TimingRecorder) ObserveHTTP(
	route string,
	statusClass string,
	duration time.Duration,
) {
	recorder.ObserveHTTPAt(route, statusClass, duration, time.Now().UTC())
}

// ObserveHTTPAt records one completed request in the cumulative and rolling views.
func (recorder *TimingRecorder) ObserveHTTPAt(
	route string,
	statusClass string,
	duration time.Duration,
	observedAt time.Time,
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
	window := observedAt.UTC().Truncate(HTTPWindowResolution).Unix()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	observeDuration(recorder.http, key, seconds)
	windowSeries := recorder.httpWindows[window]
	if windowSeries == nil {
		windowSeries = make(map[httpSeriesKey]*durationHistogram)
		recorder.httpWindows[window] = windowSeries
	}
	observeDuration(windowSeries, key, seconds)
	oldest := observedAt.UTC().Add(-HTTPWindowRetention).Truncate(HTTPWindowResolution).Unix()
	for minute := range recorder.httpWindows {
		if minute < oldest {
			delete(recorder.httpWindows, minute)
		}
	}
}

func observeDuration(
	series map[httpSeriesKey]*durationHistogram,
	key httpSeriesKey,
	seconds float64,
) {
	histogram := series[key]
	if histogram == nil {
		histogram = &durationHistogram{}
		series[key] = histogram
	}
	histogram.count++
	histogram.sumSeconds += seconds
	histogram.maximumSeconds = max(histogram.maximumSeconds, seconds)
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
	return snapshotHTTPSeries(recorder.http)
}

// HTTPSnapshotBetween returns a stable rolling-window copy sorted by route and status class.
func (recorder *TimingRecorder) HTTPSnapshotBetween(
	since time.Time,
	until time.Time,
) []HTTPDuration {
	if recorder == nil {
		return []HTTPDuration{}
	}
	sinceMinute := since.UTC().Truncate(HTTPWindowResolution).Unix()
	untilMinute := until.UTC().Truncate(HTTPWindowResolution).Unix()
	recorder.mu.RLock()
	defer recorder.mu.RUnlock()
	combined := make(map[httpSeriesKey]*durationHistogram)
	for minute, series := range recorder.httpWindows {
		if minute < sinceMinute || minute > untilMinute {
			continue
		}
		for key, histogram := range series {
			mergeMutableHistogram(combined, key, histogram)
		}
	}
	return snapshotHTTPSeries(combined)
}

func snapshotHTTPSeries(series map[httpSeriesKey]*durationHistogram) []HTTPDuration {
	snapshot := make([]HTTPDuration, 0, len(series))
	for key, source := range series {
		snapshot = append(snapshot, HTTPDuration{
			Route: key.route, StatusClass: key.statusClass,
			Histogram: snapshotDurationHistogram(source),
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

func snapshotDurationHistogram(source *durationHistogram) DurationHistogram {
	buckets := make([]uint64, len(source.bucketCounts))
	copy(buckets, source.bucketCounts[:])
	return DurationHistogram{
		Count: source.count, SumSeconds: source.sumSeconds,
		MaximumSeconds: source.maximumSeconds, BucketCounts: buckets,
	}
}

func mergeMutableHistogram(
	destination map[httpSeriesKey]*durationHistogram,
	key httpSeriesKey,
	source *durationHistogram,
) {
	target := destination[key]
	if target == nil {
		target = &durationHistogram{}
		destination[key] = target
	}
	target.count += source.count
	target.sumSeconds += source.sumSeconds
	target.maximumSeconds = max(target.maximumSeconds, source.maximumSeconds)
	for index, count := range source.bucketCounts {
		target.bucketCounts[index] += count
	}
}

// MergeHistograms combines cumulative histograms with the shared bucket set.
func MergeHistograms(histograms []DurationHistogram) DurationHistogram {
	merged := DurationHistogram{BucketCounts: make([]uint64, len(DurationBucketsSeconds))}
	for _, histogram := range histograms {
		if len(histogram.BucketCounts) != len(DurationBucketsSeconds) {
			continue
		}
		merged.Count += histogram.Count
		merged.SumSeconds += histogram.SumSeconds
		merged.MaximumSeconds = max(merged.MaximumSeconds, histogram.MaximumSeconds)
		for index, count := range histogram.BucketCounts {
			merged.BucketCounts[index] += count
		}
	}
	return merged
}

// PercentileMilliseconds estimates one percentile from the shared cumulative buckets.
func PercentileMilliseconds(histogram DurationHistogram, quantile float64) *int64 {
	if histogram.Count == 0 || quantile <= 0 || quantile > 1 ||
		len(histogram.BucketCounts) != len(DurationBucketsSeconds) {
		return nil
	}
	rank := uint64(math.Ceil(float64(histogram.Count) * quantile))
	for index, count := range histogram.BucketCounts {
		if count >= rank {
			value := int64(math.Ceil(DurationBucketsSeconds[index] * 1000))
			return &value
		}
	}
	value := int64(math.Ceil(histogram.MaximumSeconds * 1000))
	return &value
}
