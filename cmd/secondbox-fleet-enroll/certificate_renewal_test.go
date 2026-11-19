package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/fleet"
)

func TestFleetEnrollmentRenewalRefreshesLeavesAndGatewayCopyWithoutChangingBootstrap(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "worker-renewal-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(csrPEM, identity string, serial int64) string {
		block, _ := pem.Decode([]byte(csrPEM))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(identity)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: identity}, URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Duration(serial) * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	identityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			io.WriteString(w, "metadata-test-token")
		case "/latest/dynamic/instance-identity/document":
			io.WriteString(w, `{"instanceId":"i-0123456789abcdef0","region":"us-east-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer identityServer.Close()
	for name, value := range map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLELOCALONLY", "AWS_SECRET_ACCESS_KEY": "local-only-signing-fixture", "AWS_SESSION_TOKEN": "local-session-fixture", "AWS_EC2_METADATA_SERVICE_ENDPOINT": identityServer.URL, "AWS_EC2_METADATA_DISABLED": "false", "AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": ""} {
		t.Setenv(name, value)
	}
	blank := filepath.Join(root, "aws-empty")
	if err := os.WriteFile(blank, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", blank)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", blank)
	count := 0
	var first fleet.EnrollmentRequest
	var latest fleet.EnrollmentResponse
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request fleet.EnrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		count++
		proof, err := url.Parse(request.IdentityProofURL)
		if err != nil || proof.Host != "sts.us-east-1.amazonaws.com" || proof.Query().Get("X-Amz-SignedHeaders") != "host;x-secondbox-enrollment" {
			t.Error("request lacks fresh STS signed binding")
		}
		if count == 1 {
			first = request
		} else if !request.Renew || request.Nonce != first.Nonce || request.RunnerCSR != first.RunnerCSR || request.GatewayCSR != first.GatewayCSR || fleet.EnrollmentBinding(first) == fleet.EnrollmentBinding(request) {
			t.Error("renewal changed bootstrap key or failed to bind renewal mode")
		}
		latest = fleet.EnrollmentResponse{RunnerID: "runner-i-0123456789abcdef0", RunnerCertificate: issue(request.RunnerCSR, "spiffe://secondbox/runner/runner-i-0123456789abcdef0", int64(count)), RunnerCA: caPEM, GatewayCertificate: issue(request.GatewayCSR, "spiffe://secondstack/runner-gateway/runner-i-0123456789abcdef0", int64(count)), GatewayCA: caPEM, ExpiresAt: now.Add(time.Duration(count) * time.Hour), RunnerEnvironment: "runner-config", GatewayEnvironment: "gateway-config", EgressContexts: "{}", RegistryFiles: map[string]string{"tenants.json": "{}"}}
		_ = json.NewEncoder(w).Encode(latest)
	}))
	defer server.Close()
	endpointCA := filepath.Join(root, "enrollment-ca.pem")
	if err := os.WriteFile(endpointCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "worker")
	if err := run(server.URL, "us-east-1", endpointCA, output, false, ""); err != nil {
		t.Fatal(err)
	}
	gatewayCopy := filepath.Join(root, "gateway-copy.crt")
	if err := os.WriteFile(gatewayCopy, []byte(latest.GatewayCertificate), 0600); err != nil {
		t.Fatal(err)
	}
	immutable := map[string][]byte{}
	for _, name := range []string{"runner.key", "gateway.key", "runner-ca.crt", "workload-ca.crt", "runner.env", "agent-runner-gateway.env", "egress-contexts.json", "enrollment.nonce", "image-registry/tenants.json"} {
		content, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		immutable[name] = content
	}
	// Model a missed timer/reboot with expired local leaves. Fresh instance
	// role proof, rather than a still-valid leaf, must authorize recovery.
	expiredRunner := issue(first.RunnerCSR, "spiffe://secondbox/runner/runner-i-0123456789abcdef0", 0)
	expiredGateway := issue(first.GatewayCSR, "spiffe://secondstack/runner-gateway/runner-i-0123456789abcdef0", 0)
	for path, content := range map[string]string{filepath.Join(output, "runner.crt"): expiredRunner, filepath.Join(output, "client.crt"): expiredGateway, gatewayCopy: expiredGateway} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(server.URL, "us-east-1", endpointCA, output, true, gatewayCopy); err != nil {
		t.Fatal(err)
	}
	for name, want := range immutable {
		got, err := os.ReadFile(filepath.Join(output, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("renewal changed bootstrap file %s: %v", name, err)
		}
	}
	for path, want := range map[string]string{filepath.Join(output, "runner.crt"): latest.RunnerCertificate, filepath.Join(output, "client.crt"): latest.GatewayCertificate, gatewayCopy: latest.GatewayCertificate} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("renewal did not refresh %s: %v", path, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(output, "runner.crt"))
	invalid := latest
	invalid.GatewayCA = "replacement-ca"
	if err := applyRenewalHandoff(output, gatewayCopy, invalid, time.Now()); err == nil {
		t.Fatal("authority substitution accepted")
	}
	invalid = latest
	invalid.RunnerCertificate = latest.GatewayCertificate
	if err := applyRenewalHandoff(output, gatewayCopy, invalid, time.Now()); err == nil {
		t.Fatal("key/identity substitution accepted")
	}
	after, _ := os.ReadFile(filepath.Join(output, "runner.crt"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused handoff partially updated leaves")
	}
	linked := filepath.Join(root, "linked-copy.crt")
	if err := os.Symlink(gatewayCopy, linked); err != nil {
		t.Fatal(err)
	}
	if err := applyRenewalHandoff(output, linked, latest, time.Now()); err == nil {
		t.Fatal("renewal followed a certificate symlink")
	}
	if _, err := loadEnrollmentCSR(root, "missing.key", true); err == nil || !strings.Contains(err.Error(), "existing") {
		t.Fatal("renewal generated a missing private key")
	}
}
