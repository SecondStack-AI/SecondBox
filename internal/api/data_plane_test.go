package api

import (
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"net/http"
	"testing"
)

func TestRequireBinaryContentType(t *testing.T) {
	for _, contentType := range []string{"application/octet-stream", "application/octet-stream; version=1"} {
		request, err := http.NewRequest(http.MethodPut, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", contentType)
		if err := requireBinaryContentType(request); err != nil {
			t.Fatalf("requireBinaryContentType(%q) = %v", contentType, err)
		}
	}
	for _, contentType := range []string{"", "application/json", "not a media type"} {
		request, err := http.NewRequest(http.MethodPut, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", contentType)
		if err := requireBinaryContentType(request); err == nil {
			t.Fatalf("requireBinaryContentType(%q) unexpectedly succeeded", contentType)
		}
	}
}

func TestDataPlaneAbsenceCodesAreDistinct(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{ports.ErrWorkspaceFileNotFound, "file_not_found"},
		{ports.ErrSandboxNotFound, "not_found"},
		{ports.ErrLifecycleUnavailable, "execution_node_unavailable"},
		{ports.ErrGenerationFenced, "generation_fenced"},
		{ports.ErrLeaseInactive, "lease_fenced"},
	} {
		_, code, _, _ := classifyError(test.err)
		if code != test.code {
			t.Fatalf("%v code = %q, want %q", test.err, code, test.code)
		}
	}
}
