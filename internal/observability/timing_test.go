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
