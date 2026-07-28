package sandboxclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"secondstack/sandbox-service/pkg/contracts"
)

func TestGeneratedClientSurfacesResponseCloseFailure(t *testing.T) {
	closeFailure := errors.New("response close failed")
	httpClient := &http.Client{Transport: closeFailureRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &closeFailureBody{
				Reader:   strings.NewReader(`{"contractVersion":"sandbox.secondstack.ai/v1","created":false,"environment":{}}`),
				closeErr: closeFailure,
			},
			Header: make(http.Header),
		}, nil
	})}
	client, err := New("http://sandbox-service.internal", "sandbox-token", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveEnvironment(t.Context(), contracts.ResolveEnvironmentRequest{})
	if !errors.Is(err, closeFailure) {
		t.Fatalf("ResolveEnvironment() error = %v, want response close failure", err)
	}
}

type closeFailureRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip closeFailureRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type closeFailureBody struct {
	io.Reader
	closeErr error
}

func (body *closeFailureBody) Close() error {
	return body.closeErr
}
