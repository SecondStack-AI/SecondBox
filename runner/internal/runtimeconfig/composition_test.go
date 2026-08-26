package runtimeconfig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderedRunnerFixturePassesProductionComposition(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "runner-environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var environment map[string]string
	if err := json.Unmarshal(data, &environment); err != nil {
		t.Fatal(err)
	}
	identity := t.TempDir()
	certificate, key, ca := issueTestIdentity(t, "runner-conformance")
	certificatePath := filepath.Join(identity, "runner.crt")
	keyPath := filepath.Join(identity, "runner.key")
	caPath := filepath.Join(identity, "runner-ca.crt")
	for path, value := range map[string][]byte{certificatePath: certificate, keyPath: key, caPath: ca} {
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environment["SECONDBOX_RUNNER_CREDENTIAL"] = strings.Repeat("c", 48)
	environment["SECONDBOX_RUNNER_CLIENT_CERTIFICATE"] = certificatePath
	environment["SECONDBOX_RUNNER_CLIENT_KEY"] = keyPath
	environment["SECONDBOX_RUNNER_CONTROL_PLANE_CA"] = caPath
	for name, value := range environment {
		t.Setenv(name, value)
	}
	composition, err := LoadFromEnvironment(false)
	if err != nil {
		t.Fatal(err)
	}
	if composition.Protocol.RunnerID != "runner-conformance" || composition.Firecracker == nil || composition.Connector == nil {
		t.Fatalf("composition = %#v", composition)
	}
	if err := composition.Connector.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SECONDBOX_RUNNER_LOG_DIR"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromEnvironment(false); err == nil || !strings.Contains(err.Error(), "SECONDBOX_RUNNER_LOG_DIR") {
		t.Fatalf("missing entrypoint setting error = %v", err)
	}
}

func issueTestIdentity(t *testing.T, runnerID string) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	identityURI, err := url.Parse("spiffe://secondbox/runner/" + runnerID)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: runnerID}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{identityURI}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestLoadGVisorCompositionRequiresCompleteEnvironment(t *testing.T) {
	complete := map[string]string{
		"SECONDBOX_GVISOR_RUNSC_PATH":                        "/opt/secondbox/bin/runsc",
		"SECONDBOX_GVISOR_AGENT_PATH":                        "/opt/secondbox/bin/secondbox-guest-agent",
		"SECONDBOX_GVISOR_FLAT_ROOT_PATH":                    "/var/lib/secondbox/gvisor/flat-root",
		"SECONDBOX_GVISOR_MATERIALIZATION_PATH":              "/var/lib/secondbox/gvisor/materialization.json",
		"SECONDBOX_GVISOR_RUNTIME_DIR":                       "/run/secondbox/gvisor",
		"SECONDBOX_GVISOR_MATERIALIZATION_DIGEST":            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"SECONDBOX_GVISOR_MAXIMUM_VCPUS":                     "8",
		"SECONDBOX_GVISOR_MAXIMUM_MEMORY_BYTES":              "17179869184",
		"SECONDBOX_GVISOR_MAXIMUM_DISK_BYTES":                "107374182400",
		"SECONDBOX_GVISOR_MAXIMUM_INSTANCES":                 "8",
		"SECONDBOX_GVISOR_MAXIMUM_OPERATIONS":                "64",
		"SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES": "8589934592",
		"SECONDBOX_GVISOR_NETWORK_PROFILE":                   "0",
	}
	for name, value := range complete {
		t.Setenv(name, value)
	}
	composition, templateBytes, err := loadGVisorComposition()
	if err != nil {
		t.Fatal(err)
	}
	if composition.RunscPath != complete["SECONDBOX_GVISOR_RUNSC_PATH"] ||
		composition.RuntimeDir != complete["SECONDBOX_GVISOR_RUNTIME_DIR"] ||
		composition.AgentPath != complete["SECONDBOX_GVISOR_AGENT_PATH"] ||
		composition.FlatRootPath != complete["SECONDBOX_GVISOR_FLAT_ROOT_PATH"] ||
		composition.MaterializationPath != complete["SECONDBOX_GVISOR_MATERIALIZATION_PATH"] ||
		composition.MaterializationDigest != complete["SECONDBOX_GVISOR_MATERIALIZATION_DIGEST"] ||
		composition.MaximumVCPUs != 8 || composition.MaximumMemoryBytes != 17179869184 ||
		composition.MaximumDiskBytes != 107374182400 || composition.MaximumInstances != 8 ||
		composition.MaximumOperations != 64 || templateBytes != 8589934592 ||
		composition.NetworkProfile != 0 {
		t.Fatalf("gVisor composition = %#v templateBytes=%d", composition, templateBytes)
	}

	t.Run("dns upstream", func(t *testing.T) {
		t.Setenv("SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM", "10.201.0.10:53")
		composition, _, err := loadGVisorComposition()
		if err != nil || composition.DNSUpstream != "10.201.0.10:53" {
			t.Fatalf("dns upstream composition = %#v, %v", composition, err)
		}
	})

	t.Run("network profile", func(t *testing.T) {
		t.Setenv("SECONDBOX_GVISOR_NETWORK_PROFILE", "1")
		composition, _, err := loadGVisorComposition()
		if err != nil || composition.NetworkProfile != 1 {
			t.Fatalf("network profile composition = %#v, %v", composition, err)
		}
		t.Setenv("SECONDBOX_GVISOR_NETWORK_PROFILE", "one")
		if _, _, err := loadGVisorComposition(); err == nil {
			t.Fatal("malformed network profile was accepted")
		}
		t.Setenv("SECONDBOX_GVISOR_NETWORK_PROFILE", "")
		if _, _, err := loadGVisorComposition(); err == nil {
			t.Fatal("an omitted network profile was accepted; sharing profile 0 silently lets reconciliation cross runners")
		}
	})

	for _, required := range []string{
		"SECONDBOX_GVISOR_RUNSC_PATH",
		"SECONDBOX_GVISOR_MATERIALIZATION_DIGEST",
		"SECONDBOX_GVISOR_MAXIMUM_VCPUS",
	} {
		t.Run(required, func(t *testing.T) {
			t.Setenv(required, "")
			if _, _, err := loadGVisorComposition(); err == nil {
				t.Fatalf("missing %s was accepted", required)
			}
		})
	}
	t.Run("relative path rejected", func(t *testing.T) {
		t.Setenv("SECONDBOX_GVISOR_RUNSC_PATH", "bin/runsc")
		if _, _, err := loadGVisorComposition(); err == nil {
			t.Fatal("relative runsc path was accepted")
		}
	})
}
