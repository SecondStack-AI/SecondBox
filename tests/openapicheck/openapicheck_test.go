package openapicheck_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/tests/openapicheck"
)

func contractPath(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve source path")
	}
	return filepath.Join(
		filepath.Dir(sourceFile), "..", "..",
		"contracts", "openapi", "v1", "secondbox.openapi.json",
	)
}

func loadContract(t *testing.T) *openapicheck.Document {
	t.Helper()
	document, err := openapicheck.Load(contractPath(t))
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return document
}

// TestValidatorRejectsUndeclaredAndMissingProperties covers the exact defect
// class that static contract tests missed: a required property the handler
// stopped emitting, and a property the handler emits that the contract never
// declared under additionalProperties:false.
func TestValidatorRejectsUndeclaredAndMissingProperties(t *testing.T) {
	document := loadContract(t)

	valid := `{"items":[]}`
	if err := document.ValidateResponse(
		http.MethodGet, "/v1/sandboxes", http.StatusOK, "application/json", []byte(valid),
	); err != nil {
		t.Fatalf("well-formed SandboxPage rejected: %v", err)
	}

	undeclared := `{"items":[],"projectId":"prj_legacy"}`
	err := document.ValidateResponse(
		http.MethodGet, "/v1/sandboxes", http.StatusOK, "application/json", []byte(undeclared),
	)
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("undeclared property accepted, error = %v", err)
	}

	missing := `{}`
	err = document.ValidateResponse(
		http.MethodGet, "/v1/sandboxes", http.StatusOK, "application/json", []byte(missing),
	)
	if err == nil || !strings.Contains(err.Error(), "required property") {
		t.Fatalf("missing required property accepted, error = %v", err)
	}
}

func TestValidatorEnforcesScalarConstraints(t *testing.T) {
	document := loadContract(t)
	for name, body := range map[string]string{
		"wrong type":       `{"items":"not-an-array"}`,
		"bad item shape":   `{"items":[{"id":"sbx_1"}]}`,
		"malformed cursor": `{"items":[],"nextCursor":17}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := document.ValidateResponse(
				http.MethodGet, "/v1/sandboxes", http.StatusOK, "application/json", []byte(body),
			); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

// TestValidatorRejectsUndeclaredRouteAndStatus proves a response the contract
// never described is a failure rather than an unchecked pass.
func TestValidatorRejectsUndeclaredRouteAndStatus(t *testing.T) {
	document := loadContract(t)

	err := document.ValidateResponse(
		http.MethodGet, "/v1/nonexistent", http.StatusOK, "application/json", []byte(`{}`),
	)
	if err == nil || !strings.Contains(err.Error(), "not declared by the contract") {
		t.Fatalf("undeclared route accepted, error = %v", err)
	}

	err = document.ValidateResponse(
		http.MethodGet, "/v1/sandboxes", http.StatusTeapot, "application/json", []byte(`{}`),
	)
	if err == nil || !strings.Contains(err.Error(), "does not declare status") {
		t.Fatalf("undeclared status accepted, error = %v", err)
	}
}

// TestValidatorRejectsUnimplementedKeywords proves the validator fails loudly
// rather than silently passing when the contract grows vocabulary it cannot
// check, so it cannot rot into a no-op.
func TestValidatorRejectsUnimplementedKeywords(t *testing.T) {
	document := loadContract(t)
	if err := document.ValidateAgainstSchema(
		map[string]any{"type": "string", "multipleOf": float64(2)}, "value",
	); err == nil || !strings.Contains(err.Error(), "does not implement") {
		t.Fatalf("unimplemented keyword accepted, error = %v", err)
	}
}

func TestHandlerReportsViolationsAndPassesTraffic(t *testing.T) {
	document := loadContract(t)
	var reported []string
	report := func(format string, args ...any) {
		reported = append(reported, format)
		_ = args
	}

	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		// Emits a property the contract does not declare.
		_, _ = writer.Write([]byte(`{"items":[],"projectId":"prj_legacy"}`))
	})
	server := httptest.NewServer(document.Handler(report, inner))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if len(reported) != 1 {
		t.Fatalf("reported %d violations, want 1", len(reported))
	}
}

func TestHandlerLeavesHijackedConnectionsAlone(t *testing.T) {
	document := loadContract(t)
	report := func(format string, args ...any) {
		t.Errorf("unexpected contract violation: "+format, args...)
	}
	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("wrapped writer does not support hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer connection.Close()
		_, _ = connection.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
	})
	server := httptest.NewServer(document.Handler(report, inner))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
}
