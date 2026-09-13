package scenarioharness

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// TimeoutDiagnosticsTransport captures stack state at the first HTTP timeout,
// before fixture cleanup or subsequent polling can obscure the original failure.
// It preserves the response and error, including the client's timeout semantics.
type TimeoutDiagnosticsTransport struct {
	Base    http.RoundTripper
	Capture func()
	once    sync.Once
}

func (transport *TimeoutDiagnosticsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.Base.RoundTrip(request)
	var timeout net.Error
	deadline, hasDeadline := request.Context().Deadline()
	// For a wrapped transport, http.Client can close Request.Cancel before
	// the context timer fires. The underlying error then says "request
	// canceled"; Client.Do only classifies it as a timeout after we return.
	deadlineExpired := hasDeadline && !time.Now().Before(deadline)
	if err != nil && (deadlineExpired || (errors.As(err, &timeout) && timeout.Timeout())) {
		transport.once.Do(transport.Capture)
	}
	return response, err
}
