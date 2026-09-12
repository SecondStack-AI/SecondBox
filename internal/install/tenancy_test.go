package install

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestBootstrapTenancy(t *testing.T) {
	for _, failure := range []string{"", "subject", "revoke"} {
		t.Run("failure="+failure, func(t *testing.T) {
			var tenant secondboxclient.CreateTenantRequest
			var subject secondboxclient.CreateSubjectRequest
			creates, revocations, applications := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				expected := "Bearer platform-secret"
				if strings.HasPrefix(r.URL.Path, "/v1/subjects") || r.URL.Path == "/v1/application-authorities" {
					expected = "Bearer controller-secret"
				}
				if r.Header.Get("Authorization") != expected {
					t.Errorf("wrong authority on %s", r.URL.Path)
					w.WriteHeader(401)
					return
				}
				if r.Method == "POST" && r.Header.Get("Idempotency-Key") == "" {
					t.Error("missing SDK idempotency key")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/tenants":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
				case "GET /v1/tenants/local":
					if tenant.Ref == "" {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(tenant)
				case "POST /v1/tenants":
					if err := json.NewDecoder(r.Body).Decode(&tenant); err != nil {
						t.Error(err)
					}
					creates++
					_ = json.NewEncoder(w).Encode(tenant)
				case "POST /v1/tenants/local/controller-authorities":
					var request secondboxclient.CreateTenantControllerAuthorityRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.ExpiresAt.After(time.Now().Add(11 * time.Minute)) {
						t.Error("transient controller expiry too long")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"authority": map[string]any{"id": "controller-id", "revision": 1}, "bearerToken": "controller-secret"})
				case "POST /v1/tenants/local/controller-authorities/controller-id:revoke":
					revocations++
					if r.Header.Get("If-Match") != secondboxclient.RevisionETag(1) {
						t.Error("missing SDK revision fence")
					}
					if failure == "revoke" {
						w.WriteHeader(500)
						return
					}
					_, _ = w.Write([]byte(`{}`))
				case "GET /v1/subjects/local-operator":
					if subject.Ref == "" {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(subject)
				case "POST /v1/subjects":
					if failure == "subject" {
						w.WriteHeader(500)
						return
					}
					if err := json.NewDecoder(r.Body).Decode(&subject); err != nil {
						t.Error(err)
					}
					creates++
					_ = json.NewEncoder(w).Encode(subject)
				case "POST /v1/application-authorities":
					var request secondboxclient.CreateApplicationAuthorityRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.ExpiresAt.Before(time.Now().Add(364 * 24 * time.Hour)) {
						t.Error("application expires before contract ceiling")
					}
					applications++
					_ = json.NewEncoder(w).Encode(map[string]any{"authority": map[string]any{"id": "application-id"}, "bearerToken": "application-secret"})
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			plan := validPlan(t)
			plan.HostFacts.InvokingUID = int64(os.Getuid())
			plan.Network.APIAddress = strings.TrimPrefix(server.URL, "http://")
			path := filepath.Join(t.TempDir(), "platform-token")
			if err := os.WriteFile(path, []byte("platform-secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
			plan.SecretTargets = []SecretTarget{{Category: "platform-authority", Path: path}}
			options := TenancyOptions{TenantRef: "local", SubjectRef: "local-operator", EgressContext: "secondbox-test", Application: true}
			result, err := BootstrapTenancy(t.Context(), plan, options, server.Client())
			if failure != "" {
				if err == nil || revocations != 1 {
					t.Fatalf("failure=%s err=%v revocations=%d", failure, err, revocations)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if creates != 2 || revocations != 1 || result.BearerToken != "application-secret" {
				t.Fatalf("bootstrap result=%#v creates=%d revokes=%d", result.Evidence, creates, revocations)
			}
			if tenant.EgressContext == nil || *tenant.EgressContext != "secondbox-test" || len(tenant.AllowedProfileGrants) != 3 || !slices.Contains(tenant.AllowedApplicationScopes, "sandbox:ports:direct") || tenant.ExpiryPolicy.MaximumAuthorityLifetimeSeconds != 31536000 || tenant.ExpiryPolicy.MaximumSubjectLifetimeSeconds != 31536000 || subject.ExpiresAt != nil || tenant.ExpiresAt != nil {
				t.Fatalf("incorrect development tenancy: %#v %#v", tenant, subject)
			}
			receipt := InstallReceipt{TenancyBootstraps: []StageRecord{{Stage: StageTenancyBootstrap, Evidence: result.Evidence}}}
			for _, value := range []any{plan, receipt, result} {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"platform-secret", "controller-secret", "application-secret"} {
					if strings.Contains(string(encoded), secret) {
						t.Fatal("serialized bootstrap contains secret")
					}
				}
			}
			options.Application = false
			result, err = BootstrapTenancy(t.Context(), plan, options, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if creates != 2 || revocations != 2 || applications != 1 || result.Evidence["tenant"] != "existing; unchanged" || result.Evidence["subject"] != "existing; unchanged" {
				t.Fatalf("rerun mutated existing tenancy: %#v", result.Evidence)
			}
			options.Check = true
			if _, err := BootstrapTenancy(t.Context(), plan, options, server.Client()); err != nil {
				t.Fatal(err)
			}
			if revocations != 2 || creates != 2 {
				t.Fatal("check mutated tenancy")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := BootstrapTenancy(context.Background(), plan, options, server.Client()); err == nil {
				t.Fatal("missing platform token accepted")
			}
		})
	}
}
