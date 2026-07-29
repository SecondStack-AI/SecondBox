package observability

import (
	"testing"
	"time"
)

func TestTimingRecorderUsesBoundedRouteAndStatusSeries(t *testing.T) {
	recorder := NewTimingRecorder()
	recorder.ObserveHTTP("", "4xx", 6*time.Millisecond)
	recorder.ObserveHTTP("GET /v1/sandboxes/{sandboxID}", "2xx", 250*time.Millisecond)
	recorder.ObserveHTTP("GET /v1/sandboxes/{sandboxID}", "2xx", time.Second)

	snapshot := recorder.HTTPSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("HTTP timing series = %d, want 2", len(snapshot))
	}
	if snapshot[0].Route != "GET /v1/sandboxes/{sandboxID}" ||
		snapshot[0].StatusClass != "2xx" ||
		snapshot[0].Histogram.Count != 2 ||
		snapshot[0].Histogram.BucketCounts[5] != 1 ||
		snapshot[0].Histogram.BucketCounts[7] != 2 {
		t.Fatalf("matched route histogram = %#v", snapshot[0])
	}
	if snapshot[1].Route != "unmatched" ||
		snapshot[1].Histogram.Count != 1 ||
		snapshot[1].Histogram.BucketCounts[0] != 0 ||
		snapshot[1].Histogram.BucketCounts[1] != 1 {
		t.Fatalf("unmatched route histogram = %#v", snapshot[1])
	}
}

func TestTimingRecorderReturnsOnlyRequestedRollingBucketsAndPercentiles(t *testing.T) {
	recorder := NewTimingRecorder()
	now := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	recorder.ObserveHTTPAt("GET /healthz", "2xx", 8*time.Millisecond, now.Add(-2*time.Hour))
	recorder.ObserveHTTPAt("GET /healthz", "2xx", 8*time.Millisecond, now.Add(-5*time.Minute))
	recorder.ObserveHTTPAt("GET /healthz", "2xx", 80*time.Millisecond, now.Add(-time.Minute))

	snapshot := recorder.HTTPSnapshotBetween(now.Add(-10*time.Minute), now)
	if len(snapshot) != 1 || snapshot[0].Histogram.Count != 2 {
		t.Fatalf("rolling HTTP timing = %#v", snapshot)
	}
	p50 := PercentileMilliseconds(snapshot[0].Histogram, 0.50)
	p95 := PercentileMilliseconds(snapshot[0].Histogram, 0.95)
	if p50 == nil || *p50 != 10 || p95 == nil || *p95 != 100 {
		t.Fatalf("rolling percentiles p50=%v p95=%v", p50, p95)
	}
	merged := MergeHistograms([]DurationHistogram{snapshot[0].Histogram})
	if merged.Count != 2 || merged.MaximumSeconds != 0.08 {
		t.Fatalf("merged HTTP timing = %#v", merged)
	}
}
