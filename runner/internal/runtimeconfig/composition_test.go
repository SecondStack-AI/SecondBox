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
