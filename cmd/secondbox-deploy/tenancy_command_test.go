package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

func TestBootstrapTenancyCommandRecordsNoSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer recorded-platform-token"
		if strings.HasPrefix(r.URL.Path, "/v1/subjects") {
			expected = "Bearer transient-controller-token"
		}
		if r.Header.Get("Authorization") != expected {
			t.Error("unrecorded authority used")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tenants":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "GET /v1/tenants/local":
			_, _ = w.Write([]byte(`{"ref":"local","expiryPolicy":{"maximumAuthorityLifetimeSeconds":31536000,"maximumSubjectLifetimeSeconds":31536000}}`))
		case "POST /v1/tenants/local/controller-authorities":
			_, _ = w.Write([]byte(`{"authority":{"id":"controller-id","revision":1},"bearerToken":"transient-controller-token"}`))
		case "GET /v1/subjects/local-operator":
			_, _ = w.Write([]byte(`{"ref":"local-operator"}`))
		case "POST /v1/tenants/local/controller-authorities/controller-id:revoke":
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	operation := filepath.Join(root, "operation")
	plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now().UTC(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(root, "bin"), CLIConfigPath: filepath.Join(root, "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(fakeGuidedRelease(), releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: standardresources.BundleNames(), RetentionSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	plan.Network.APIAddress = server.Listener.Addr().String()
	if host, _, _ := net.SplitHostPort(plan.Network.APIAddress); host != "127.0.0.1" {
		t.Fatal("fixture must use loopback")
	}
	receipt, err := install.NewReceipt(plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range install.StageSequence {
		if err := receipt.CompleteStage(stage, time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
		if stage == install.StageReadiness {
			break
		}
	}
	if err := os.Mkdir(operation, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
		t.Fatal(err)
	}
	secret := installerPlannedPath(plan, "platform-token")
	if err := os.Mkdir(filepath.Dir(secret), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("recorded-platform-token"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECONDBOX_TOKEN", "ambient-token-must-not-be-used")
	var output, diagnostic bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever, Capabilities: cliui.ForWriter(&output, &diagnostic)}
	before, err := os.ReadFile(filepath.Join(operation, "install-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runBootstrapTenancy(t.Context(), []string{operation, "--check"}, renderer); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(operation, "install-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("check rewrote receipt")
	}
	for range 2 {
		if err := runBootstrapTenancy(t.Context(), []string{operation}, renderer); err != nil {
			t.Fatal(err)
		}
	}
	_, recorded, err := install.ReadOperationReadOnly(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded.TenancyBootstraps) != 2 || recorded.TenancyBootstraps[1].Evidence["subject"] != "existing; unchanged" {
		t.Fatalf("bootstrap journal=%#v", recorded.TenancyBootstraps)
	}
	encoded, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"recorded-platform-token", "transient-controller-token", "ambient-token-must-not-be-used"} {
		if strings.Contains(string(encoded)+output.String()+diagnostic.String(), secret) {
			t.Fatal("token escaped to receipt or presentation")
		}
	}
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	if err := runBootstrapTenancy(t.Context(), []string{operation}, renderer); err == nil {
		t.Fatal("missing recorded platform token accepted")
	}
}
