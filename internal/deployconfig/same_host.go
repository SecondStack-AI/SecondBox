package deployconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

func validateSameHostRunnerHost(runner Runner, controlPlaneCAPath string) error {
	for uid := *runner.FirecrackerJailerUIDStart; uid < *runner.FirecrackerJailerUIDStart+*runner.FirecrackerJailerUIDCount; uid++ {
		account, err := user.LookupId(strconv.FormatInt(uid, 10))
		if err == nil {
			return fmt.Errorf("firecracker jailer UID %d is already assigned to host account %q", uid, account.Username)
		}
		var unknown user.UnknownUserIdError
		if !errors.As(err, &unknown) {
			return fmt.Errorf("inspect host account assignment for firecracker jailer UID %d: %w", uid, err)
		}
	}
	for name, path := range map[string]string{
		"identity_host_directory":  runner.IdentityHostDirectory,
		"artifact_host_directory":  runner.ArtifactHostDirectory,
		"state_host_directory":     runner.StateHostDirectory,
		"workspace_host_directory": runner.WorkspaceHostDirectory,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s must be an existing absolute non-symbolic-link directory: %w", name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must be an existing absolute non-symbolic-link directory", name)
		}
	}
	rootDevice, err := filesystemDevice("/")
	if err != nil {
		return fmt.Errorf("inspect host root filesystem: %w", err)
	}
	workspaceDevice, err := filesystemDevice(runner.WorkspaceHostDirectory)
	if err != nil {
		return fmt.Errorf("inspect workspace host filesystem: %w", err)
	}
	if workspaceDevice == rootDevice {
		return fmt.Errorf("workspace_host_directory must use a dedicated non-root filesystem")
	}

	identityCAPath, err := resolveRegularReference("", filepath.Join(runner.IdentityHostDirectory, "runner-ca.crt"))
	if err != nil {
		return fmt.Errorf("Runner CA certificate: %w", err)
	}
	identityCertificatePath, err := resolveRegularReference("", filepath.Join(runner.IdentityHostDirectory, "runner.crt"))
	if err != nil {
		return fmt.Errorf("Runner certificate: %w", err)
	}
	identityKeyPath, err := resolvePrivateReference("", filepath.Join(runner.IdentityHostDirectory, "runner.key"))
	if err != nil {
		return fmt.Errorf("Runner private key: %w", err)
	}
	controlPlaneCA, err := os.ReadFile(controlPlaneCAPath)
	if err != nil {
		return err
	}
	identityCA, err := os.ReadFile(identityCAPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(controlPlaneCA, identityCA) {
		return fmt.Errorf("Runner identity must trust the configured control-plane Runner CA")
	}
	caBlock, remainder := pem.Decode(controlPlaneCA)
	if caBlock == nil || len(remainder) != 0 || caBlock.Type != "CERTIFICATE" {
		return fmt.Errorf("configured control-plane Runner CA is invalid")
	}
	caCertificate, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse configured control-plane Runner CA: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(identityCertificatePath, identityKeyPath)
	if err != nil {
		return fmt.Errorf("Runner certificate and private key: %w", err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse Runner certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("verify Runner certificate: %w", err)
	}
	wantURI, err := url.Parse("spiffe://secondbox/runner/" + url.PathEscape(runner.RunnerID))
	if err != nil {
		return err
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != wantURI.String() {
		return fmt.Errorf("Runner certificate identity does not match runner_id %s", runner.RunnerID)
	}
	return nil
}
