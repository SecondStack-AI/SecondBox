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

func TestCreateImageSelectionMayUseProfileAssets(t *testing.T) {
	var request CreateSandboxRequest
	if err := json.Unmarshal([]byte(`{"profile":"fixed","metadata":{}}`), &request); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["image"]; exists {
		t.Fatalf("omitted selection serialized as an image: %s", body)
	}
	for _, body := range []string{`{"image":null}`, `{"image":{}}`, `{"image":{"reference":"invalid"}}`} {
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Fatalf("accepted invalid create image: %s", body)
		}
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
