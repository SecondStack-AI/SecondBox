package scenarioharness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type diagnosticRoundTrip func(*http.Request) (*http.Response, error)

func (trip diagnosticRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return trip(request)
}

func TestTimeoutDiagnosticsWithHTTPClientDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	captures := 0
	client := &http.Client{
		Timeout: 20 * time.Millisecond,
		Transport: &TimeoutDiagnosticsTransport{
			Base:    http.DefaultTransport,
			Capture: func() { captures++ },
		},
	}
	response, err := client.Get(server.URL + "/readyz")
	if response != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) || captures != 1 {
		t.Fatalf("HTTP timeout: error=%v captures=%d", err, captures)
	}
}

func TestTimeoutDiagnosticsPreservesFailureAndCapturesOnce(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, context.Canceled, errors.New("connection refused"), nil} {
		t.Run("error="+errorLabel(failure), func(t *testing.T) {
			captures := 0
			response := &http.Response{StatusCode: http.StatusOK}
			transport := &TimeoutDiagnosticsTransport{
				Base:    diagnosticRoundTrip(func(*http.Request) (*http.Response, error) { return response, failure }),
				Capture: func() { captures++ },
			}
			for range 2 {
				got, err := transport.RoundTrip(&http.Request{})
				if got != response || err != failure {
					t.Fatalf("response/error changed: %v, %v", got, err)
				}
			}
			want := 0
			if failure == context.DeadlineExceeded {
				want = 1
			}
			if captures != want {
				t.Fatalf("captures = %d, want %d", captures, want)
			}
		})
	}
}
func errorLabel(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
