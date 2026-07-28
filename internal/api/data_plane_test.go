package api

import (
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
