package contracts

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestStartImageSelectionMayBeOmitted(t *testing.T) {
	var request StartSandboxRequest
	if err := json.Unmarshal([]byte(`{}`), &request); err != nil {
		t.Fatalf("start with pinned image: %v", err)
	}
	if metadata := request.Image.LifecycleMetadata(); len(metadata) != 0 {
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
	for _, body := range []string{`{"image":null}`, `{"image":{}}`, `{"image":{"reference":"invalid"}}`,
		`{"attributedExecution":{"authorizationRef":"command","expiresAt":"2026-09-10T12:00:00Z"}}`} {
		var request StartSandboxRequest
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Fatalf("accepted invalid image: %s", body)
		}
	}
}

func TestExecRequestsCarryAttributionAndRefuseNull(t *testing.T) {
	const attribution = `"attributedExecution":{"authorizationRef":"command","expiresAt":"2026-09-10T12:00:00Z"}`
	const buffered = `{"command":{"mode":"shell","command":"true"},"environment":{},"deadlineMilliseconds":1000,"maximumOutputBytes":64`
	const streaming = buffered + `,"windowBytes":4096`
	var bufferedRequest BufferedExecRequest
	if err := json.Unmarshal([]byte(buffered+","+attribution+"}"), &bufferedRequest); err != nil || bufferedRequest.AttributedExecution == nil ||
		bufferedRequest.AttributedExecution.AuthorizationRef != "command" {
		t.Fatalf("buffered attribution = %+v, error = %v", bufferedRequest.AttributedExecution, err)
	}
	var streamingRequest StreamingExecRequest
	if err := json.Unmarshal([]byte(streaming+","+attribution+"}"), &streamingRequest); err != nil || streamingRequest.AttributedExecution == nil {
		t.Fatalf("streaming attribution = %+v, error = %v", streamingRequest.AttributedExecution, err)
	}
	if err := json.Unmarshal([]byte(buffered+"}"), &bufferedRequest); err != nil || bufferedRequest.AttributedExecution != nil {
		t.Fatalf("omitted attribution = %+v, error = %v", bufferedRequest.AttributedExecution, err)
	}
	for _, body := range []string{
		buffered + `,"attributedExecution":null}`,
		buffered + `,"attributedExecution":{"authorizationRef":"command","expiresAt":"2026-09-10T12:00:00Z","gateway":"x"}}`,
	} {
		if err := json.Unmarshal([]byte(body), &bufferedRequest); err == nil {
			t.Fatalf("accepted invalid buffered attribution: %s", body)
		}
	}
	var refused *RequestFieldError
	err := json.Unmarshal([]byte(streaming+`,"attributedExecution":null}`), &streamingRequest)
	if !errors.As(err, &refused) || refused.Field != "attributedExecution" {
		t.Fatalf("null streaming attribution error = %v", err)
	}
}
