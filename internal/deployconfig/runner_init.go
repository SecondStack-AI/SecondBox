package deployconfig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func RunnerInit(manifestPath, runnerID, target string) error {
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	absoluteManifest, _ := filepath.Abs(manifestPath)
	base := filepath.Dir(absoluteManifest)
	var declared *Runner
	for index := range manifest.Runners {
		if manifest.Runners[index].RunnerID == runnerID {
			declared = &manifest.Runners[index]
			break
		}
	}
	if declared == nil {
		return manifestError("runner-init runner_id is not declared: "+runnerID, nil)
	}
	absoluteTarget, err := filepath.Abs(target)
	if err != nil {
		return manifestError("runner-init target", err)
	}
	if declared.Placement == "same-host" {
		expected := declared.IdentityHostDirectory
		if !filepath.IsAbs(expected) {
			expected = filepath.Join(base, expected)
		}
		if filepath.Clean(expected) != absoluteTarget {
			return manifestError("runner-init target does not match the declared same-host identity directory", nil)
		}
	}
	if _, err := os.Lstat(absoluteTarget); err == nil {
		return manifestError("runner-init refuses an existing target", nil)
	} else if !os.IsNotExist(err) {
		return manifestError("runner-init inspect target", err)
	}
	parent := filepath.Dir(absoluteTarget)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return manifestError("runner-init parent must be an existing non-symbolic-link directory", err)
	}
	staging, err := os.MkdirTemp(parent, ".secondbox-runner-identity-")
	if err != nil {
		return err
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	caCertPath, err := resolveRegularReference(base, manifest.RunnerTrust.CACertificateFile)
	if err != nil {
		return manifestError("runner-init CA certificate", err)
	}
	caKeyPath, err := resolvePrivateReference(base, manifest.RunnerTrust.CAPrivateKeyFile)
	if err != nil {
		return manifestError("runner-init CA private key", err)
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	caBlock, remainder := pem.Decode(caPEM)
	if caBlock == nil || len(remainder) != 0 || caBlock.Type != "CERTIFICATE" {
		return manifestError("runner-init CA certificate must contain exactly one PEM certificate", nil)
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !caCert.IsCA {
		return manifestError("runner-init CA certificate is invalid", err)
	}
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return err
	}
	keyBlock, remainder := pem.Decode(keyPEM)
	if keyBlock == nil || len(remainder) != 0 {
		return manifestError("runner-init CA key must contain exactly one PEM key", nil)
	}
	caKey, err := parseRSAPrivateKey(keyBlock.Bytes)
	if err != nil {
		return manifestError("runner-init CA key", err)
	}
	if !caKey.PublicKey.Equal(caCert.PublicKey) {
		return manifestError("runner-init CA certificate and key do not match", nil)
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	identityURI, err := url.Parse("spiffe://secondbox/runner/" + url.PathEscape(runnerID))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	clientSerial, err := randomSerial()
	if err != nil {
		return err
	}
	clientTemplate := &x509.Certificate{SerialNumber: clientSerial, Subject: pkix.Name{CommonName: runnerID}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Duration(*manifest.RunnerTrust.CertificateLifetimeDays) * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{identityURI}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{"runner.crt": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), 0o600}, "runner.key": {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}), 0o600}, "runner-ca.crt": {caPEM, 0o644}}
	for name, file := range files {
		if err := writeAtomic(filepath.Join(staging, name), file.data, file.mode, false); err != nil {
			return err
		}
	}
	resolved, err := resolveManifest(manifest, base)
	if err != nil {
		return err
	}
	environment := resolved.RemoteRunnerEnvironment[runnerID]
	if declared.Placement == "same-host" {
		environment = resolveRunnerEnvironment(*declared, resolved.Environment["SECONDBOX_RUNNER_CREDENTIAL"])
	}
	if environment == nil {
		return fmt.Errorf("SecondBox deployment manifest: runner-init could not resolve runner %s", runnerID)
	}
	encoded, err := EncodeSystemdEnvironment(environment)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(staging, "runner.env"), encoded, 0o600, false); err != nil {
		return err
	}
	if err := os.Rename(staging, absoluteTarget); err != nil {
		return err
	}
	created = false
	return syncDirectory(parent)
}
