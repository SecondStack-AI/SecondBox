package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
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
