package api

import (
	"net/http"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
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
		err    error
		code   string
		status int
	}{
		{ports.ErrWorkspaceFileNotFound, "file_not_found", http.StatusNotFound},
		{ports.ErrSandboxNotFound, "not_found", http.StatusNotFound},
		{ports.ErrLifecycleUnavailable, "execution_node_unavailable", http.StatusConflict},
		{ports.ErrGenerationFenced, "generation_fenced", http.StatusConflict},
		{ports.ErrLeaseInactive, "lease_fenced", http.StatusConflict},
		{runnercontrol.ErrWorkspaceFull, "workspace_full", http.StatusInsufficientStorage},
	} {
		status, code, _, _ := classifyError(test.err)
		if code != test.code || status != test.status {
			t.Fatalf("%v = (%d, %q), want (%d, %q)", test.err, status, code, test.status, test.code)
		}
	}
}
