package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateExecutionIdentityAllowsNonRootOnlyForHealthcheck(t *testing.T) {
	for _, test := range []struct {
		name        string
		healthcheck bool
		uid         int
		wantErr     bool
	}{
		{name: "root server", uid: 0},
		{name: "root healthcheck", healthcheck: true, uid: 0},
		{name: "manager healthcheck", healthcheck: true, uid: 1234},
		{name: "non-root server", uid: 1234, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateExecutionIdentity(test.healthcheck, test.uid)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateExecutionIdentity(%t, %d) error = %v, wantErr %t", test.healthcheck, test.uid, err, test.wantErr)
			}
		})
	}
}

func TestProbeSandboxHostHTTPRequiresAuthenticatedReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/ready" || request.Header.Get("Authorization") != "Bearer host-token" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := probeSandboxHostHTTP(t.Context(), server.URL, "host-token"); err != nil {
		t.Fatal(err)
	}
	if err := probeSandboxHostHTTP(t.Context(), server.URL, "wrong-token"); err == nil {
		t.Fatal("expected readiness authentication failure")
	}
}
