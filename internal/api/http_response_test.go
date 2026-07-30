package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestArtifactDownloadClientDisconnectLogsAndDoesNotPanic(t *testing.T) {
	var logs bytes.Buffer
	apiHandler := &handler{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	request := httptest.NewRequest("GET", "/v1/artifacts/art_1/content", nil)
	request.Header.Set("X-Request-ID", "request-disconnect")
	writer := &disconnectingWriter{maximumBytes: 4}

	apiHandler.writeResponseBytes(
		writer,
		request,
		"Artifact download",
		[]byte("artifact-content"),
	)

	if writer.written != 4 {
		t.Fatalf("bytes written before disconnect = %d, want 4", writer.written)
	}
	for _, fragment := range []string{
		"SecondBox HTTP response write aborted",
		"Artifact download",
		"client disconnected",
		"/v1/artifacts/art_1/content",
		"request-disconnect",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("disconnect log %q does not contain %q", logs.String(), fragment)
		}
	}
}

func TestHomeRunnerUnavailableCarriesTypedRetryBackoff(t *testing.T) {
	apiHandler := &handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	writer := httptest.NewRecorder()
	writer.Header().Set("X-Request-ID", "request-home-runner-backoff")

	apiHandler.writeError(writer, request, ports.ErrHomeRunnerUnavailable)

	response := writer.Result()
	defer response.Body.Close()
	var problem contracts.Problem
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable ||
		response.Header.Get("Retry-After") != "1" ||
		problem.Code != "home_runner_unavailable" ||
		!problem.Retryable ||
		problem.RetryAfterMilliseconds == nil ||
		*problem.RetryAfterMilliseconds != 1000 {
		t.Fatalf(
			"home Runner response status=%d Retry-After=%q problem=%#v",
			response.StatusCode,
			response.Header.Get("Retry-After"),
			problem,
		)
	}
}

type disconnectingWriter struct {
	maximumBytes int
	written      int
}

func (writer *disconnectingWriter) Write(content []byte) (int, error) {
	remaining := writer.maximumBytes - writer.written
	if remaining <= 0 {
		return 0, errors.New("client disconnected")
	}
	if remaining > len(content) {
		remaining = len(content)
	}
	writer.written += remaining
	return remaining, errors.New("client disconnected")
}
