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

func TestResponseWriteClientDisconnectLogsAndDoesNotPanic(t *testing.T) {
	var logs bytes.Buffer
	apiHandler := &handler{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	request := httptest.NewRequest("GET", "/v1/sandboxes/sbx_1/files?path=output.txt", nil)
	request.Header.Set("X-Request-ID", "request-disconnect")
	writer := &disconnectingWriter{maximumBytes: 4}

	apiHandler.writeResponseBytes(
		writer,
		request,
		"file download",
		[]byte("file-content"),
	)

	if writer.written != 4 {
		t.Fatalf("bytes written before disconnect = %d, want 4", writer.written)
	}
	for _, fragment := range []string{
		"SecondBox HTTP response write aborted",
		"file download",
		"client disconnected",
		"/v1/sandboxes/sbx_1/files",
		"request-disconnect",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("disconnect log %q does not contain %q", logs.String(), fragment)
		}
	}
}

func TestSplitActionUsesOnlyTheFinalKnownSuffix(t *testing.T) {
	resource, action, ok := splitAction("tenant/west:blue:suspend", "suspend", "reactivate")
	if !ok || resource != "tenant/west:blue" || action != "suspend" {
		t.Fatalf("split action = %q %q %t", resource, action, ok)
	}
	if _, _, ok := splitAction("tenant:suspend/child", "suspend"); ok {
		t.Fatal("non-final action marker was accepted")
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
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var problem contracts.Problem
	if err := json.Unmarshal(responseBody, &problem); err != nil {
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

func TestTenantEgressContextRequiredIsTypedAndNotRetryable(t *testing.T) {
	status, code, title, retryable := classifyError(ports.ErrTenantEgressContextRequired)
	if status != http.StatusConflict || code != "tenant_egress_context_required" ||
		title != "Profile requires a Tenant egress context" || retryable {
		t.Fatalf(
			"Tenant egress-context classification = %d %q %q retryable=%t",
			status, code, title, retryable,
		)
	}
}

func TestEgressContextUnavailableIsTypedAndRetryable(t *testing.T) {
	status, code, title, retryable := classifyError(ports.ErrEgressContextUnavailable)
	if status != http.StatusServiceUnavailable || code != "egress_context_unavailable" ||
		title != "Sandbox egress context is unavailable" || !retryable {
		t.Fatalf(
			"egress-context availability classification = %d %q %q retryable=%t",
			status, code, title, retryable,
		)
	}
}

func TestPrefixedInternalErrorIsGenericRetryableAndCorrelated(t *testing.T) {
	var logs bytes.Buffer
	apiHandler := &handler{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	writer := httptest.NewRecorder()
	writer.Header().Set("X-Request-ID", "request-internal-error")
	internalText := "SecondBox data-plane admission transaction: private database failure"

	apiHandler.writeError(writer, request, errors.New(internalText))

	response := writer.Result()
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var problem contracts.Problem
	if err := json.Unmarshal(responseBody, &problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError ||
		problem.Code != "internal_error" ||
		problem.Title != "SecondBox request failed" ||
		problem.RequestID != "request-internal-error" ||
		!problem.Retryable {
		t.Fatalf("internal error response status=%d problem=%#v", response.StatusCode, problem)
	}
	if strings.Contains(string(responseBody), internalText) {
		t.Fatalf("internal error response disclosed %q", internalText)
	}
	for _, fragment := range []string{"SecondBox HTTP request failed", "request-internal-error", internalText} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("internal error log %q does not contain %q", logs.String(), fragment)
		}
	}
}

func TestInvalidRequestErrorUsesStaticTitleAndCorrelation(t *testing.T) {
	var logs bytes.Buffer
	apiHandler := &handler{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	writer := httptest.NewRecorder()
	writer.Header().Set("X-Request-ID", "request-invalid-input")
	validationText := "SecondBox JSON request decoding failed: private decoder detail"

	apiHandler.writeError(
		writer,
		request,
		requestValidationError(errors.New(validationText)),
	)

	response := writer.Result()
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var problem contracts.Problem
	if err := json.Unmarshal(responseBody, &problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest ||
		problem.Code != "invalid_request" ||
		problem.Title != "Request is invalid" ||
		problem.RequestID != "request-invalid-input" ||
		problem.Retryable {
		t.Fatalf("invalid request response status=%d problem=%#v", response.StatusCode, problem)
	}
	if strings.Contains(string(responseBody), validationText) {
		t.Fatalf("invalid request response disclosed %q", validationText)
	}
	if logs.Len() != 0 {
		t.Fatalf("invalid request unexpectedly logged an error: %q", logs.String())
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
