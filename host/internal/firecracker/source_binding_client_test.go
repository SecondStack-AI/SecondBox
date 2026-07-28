package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type sourceBindingRoundTripper func(*http.Request) (*http.Response, error)

func (transport sourceBindingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestIntegrationSourceBindingClientUsesProviderNeutralIdentity(t *testing.T) {
	var paths []string
	client, err := NewIntegrationSourceBindingClient(
		"http://integration.example",
		"integration-token",
		&http.Client{Transport: sourceBindingRoundTripper(func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			if request.Header.Get("Authorization") != "Bearer integration-token" {
				t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
			}
			var payload map[string]any
			if decodeErr := json.NewDecoder(request.Body).Decode(&payload); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			body := `{"contractVersion":"integration.secondstack.ai/v1","deleted":false}`
			if strings.HasSuffix(request.URL.Path, ":register") {
				body = `{"sourceToken":"opaque-source-token"}`
			}
			if payload["environmentId"] != "environment-1" ||
				payload["instanceId"] != "instance-1" ||
				payload["sourceAddress"] != "10.0.0.7" ||
				payload["generation"] != "2" {
				t.Fatalf("request body = %#v", payload)
			}
			if strings.HasSuffix(request.URL.Path, ":register") {
				if len(payload) != 5 {
					t.Fatalf("register request fields = %#v", payload)
				}
				allowed, ok := payload["allowedConnectionIds"].([]any)
				if !ok || len(allowed) != 1 || allowed[0] != "connection-1" {
					t.Fatalf("register allowedConnectionIds = %#v", payload["allowedConnectionIds"])
				}
			} else if len(payload) != 4 {
				t.Fatalf("unregister request fields = %#v", payload)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceBinding{
		EnvironmentID: "environment-1", InstanceID: "instance-1",
		SourceAddress: "10.0.0.7", Generation: 2,
		AllowedConnectionIDs: []string{"connection-1"},
	}
	registration, err := client.Register(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if registration.SourceToken != "opaque-source-token" {
		t.Fatalf("source token = %q", registration.SourceToken)
	}
	if err := client.Unregister(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/internal/v1/egress/source-bindings:register" || paths[1] != "/internal/v1/egress/source-bindings:unregister" {
		t.Fatalf("paths = %#v", paths)
	}
}
