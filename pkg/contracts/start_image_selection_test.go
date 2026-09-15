package contracts

import (
	"encoding/json"
	"testing"
)

func TestStartImageSelectionMayBeOmitted(t *testing.T) {
	var request StartSandboxRequest
	if err := json.Unmarshal([]byte(`{}`), &request); err != nil {
		t.Fatalf("start with pinned image: %v", err)
	}
	if metadata := MergeExecutionImageMetadata(request.Image, nil); len(metadata) != 0 {
		t.Fatalf("omitted image produced selection metadata: %v", metadata)
	}
}

func TestStartImageSelectionRejectsExplicitInvalidImage(t *testing.T) {
	for _, body := range []string{`{"image":null}`, `{"image":{}}`, `{"image":{"reference":"invalid"}}`} {
		var request StartSandboxRequest
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Fatalf("accepted invalid image: %s", body)
		}
	}
}
