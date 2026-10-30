package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
)

func TestHealthReportsRunningControlPlaneReleaseIdentity(t *testing.T) {
	linkedVersion, linkedCommit := buildinfo.Version, buildinfo.SourceCommit
	t.Cleanup(func() { buildinfo.Version, buildinfo.SourceCommit = linkedVersion, linkedCommit })
	buildinfo.Version, buildinfo.SourceCommit = "0.30.0", "0123456789abcdef0123456789abcdef01234567"

	apiHandler := &handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	writer := httptest.NewRecorder()
	apiHandler.health(writer, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if writer.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", writer.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health body %q: %v", writer.Body.String(), err)
	}
	want := map[string]string{
		"status":       "healthy",
		"version":      "0.30.0",
		"sourceCommit": "0123456789abcdef0123456789abcdef01234567",
	}
	if len(body) != len(want) {
		t.Fatalf("health body = %v, want %v", body, want)
	}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("health body = %v, want %v", body, want)
		}
	}
}
