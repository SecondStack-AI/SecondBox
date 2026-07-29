package openapicheck

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
)

// Reporter receives one message per contract violation. Tests pass t.Errorf.
type Reporter func(format string, args ...any)

// Handler wraps inner so that every JSON response it produces is validated
// against the contract before the test observes it. Streaming upgrades are
// passed through untouched: a hijacked connection is no longer an HTTP response
// the contract describes.
func (document *Document) Handler(report Reporter, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder := &responseRecorder{ResponseWriter: writer, status: http.StatusOK}
		inner.ServeHTTP(recorder, request)
		if recorder.hijacked {
			return
		}
		contentType := recorder.Header().Get("Content-Type")
		if err := document.ValidateResponse(
			request.Method, request.URL.Path, recorder.status, contentType, recorder.body.Bytes(),
		); err != nil {
			report("%v", err)
		}
	})
}

// responseRecorder mirrors the response body while forwarding it downstream.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
	hijacked    bool
}

func (recorder *responseRecorder) WriteHeader(status int) {
	if recorder.wroteHeader {
		return
	}
	recorder.status = status
	recorder.wroteHeader = true
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *responseRecorder) Write(payload []byte) (int, error) {
	if !recorder.wroteHeader {
		recorder.WriteHeader(http.StatusOK)
	}
	// Only JSON bodies are validated, so only JSON bodies are buffered. This
	// keeps artifact and workspace downloads streaming as they do in production.
	if isJSONContentType(recorder.Header().Get("Content-Type")) {
		recorder.body.Write(payload)
	}
	return recorder.ResponseWriter.Write(payload)
}

func (recorder *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := recorder.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("openapicheck: response writer does not support hijacking")
	}
	recorder.hijacked = true
	return hijacker.Hijack()
}

func (recorder *responseRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func isJSONContentType(contentType string) bool {
	for index := 0; index < len(contentType); index++ {
		if contentType[index] == ';' {
			contentType = contentType[:index]
			break
		}
	}
	switch contentType {
	case "application/json", "application/problem+json":
		return true
	default:
		return false
	}
}
