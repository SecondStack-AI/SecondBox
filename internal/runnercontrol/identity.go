package runnercontrol

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

var ErrRunnerCredentialInvalid = errors.New("SecondBox runner credential is invalid")

const runnerCredentialMinimumBytes = 32

// LoadCertificateAuthority loads the explicit PEM authority used for runner mTLS.
func LoadCertificateAuthority(certificatePath string) (*x509.Certificate, error) {
	contents, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner certificate authority load: %w", err)
	}
	block, remainder := pem.Decode(contents)
	if block == nil || len(remainder) != 0 || block.Type != "CERTIFICATE" {
		return nil, errors.New("SecondBox runner certificate authority requires exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner certificate authority parse: %w", err)
	}
	if !certificate.IsCA {
		return nil, errors.New("SecondBox runner certificate authority certificate is not a CA")
	}
	return certificate, nil
}

// CredentialAuthorityConfig contains the pre-shared runner credential and mTLS authority.
type CredentialAuthorityConfig struct {
	Credential    string
	CACertificate *x509.Certificate
}

// CredentialAuthority verifies one pre-shared credential over an mTLS-authenticated connection.
type CredentialAuthority struct {
	credentialHash [sha256.Size]byte
	caCertificate  *x509.Certificate
}

// RunnerIdentity is derived only from a verified client certificate.
type RunnerIdentity struct {
	RunnerID               string
	CredentialSerial       string
	CertificateFingerprint string
}

// NewCredentialAuthority constructs the runner credential verifier without database state.
func NewCredentialAuthority(config CredentialAuthorityConfig) (*CredentialAuthority, error) {
	if len(config.Credential) < runnerCredentialMinimumBytes || config.CACertificate == nil {
		return nil, errors.New("SecondBox runner credential authority requires an explicit credential and CA certificate")
	}
	if !config.CACertificate.IsCA {
		return nil, errors.New("SecondBox runner credential authority certificate is not a CA")
	}
	return &CredentialAuthority{
		credentialHash: sha256.Sum256([]byte(config.Credential)),
		caCertificate:  config.CACertificate,
	}, nil
}

// VerifyClientCertificate proves the shared credential and CA-signed certificate identity.
func (authority *CredentialAuthority) VerifyClientCertificate(
	_ context.Context,
	certificate *x509.Certificate,
	credential string,
) (RunnerIdentity, error) {
	if certificate == nil || len(credential) < runnerCredentialMinimumBytes {
		return RunnerIdentity{}, ErrRunnerCredentialInvalid
	}
	presentedHash := sha256.Sum256([]byte(credential))
	if subtle.ConstantTimeCompare(presentedHash[:], authority.credentialHash[:]) != 1 {
		return RunnerIdentity{}, ErrRunnerCredentialInvalid
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.caCertificate)
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return RunnerIdentity{}, fmt.Errorf("SecondBox runner certificate verification: %w", err)
	}
	runnerID, err := runnerIDFromCertificate(certificate)
	if err != nil {
		return RunnerIdentity{}, err
	}
	return RunnerIdentity{
		RunnerID: runnerID, CredentialSerial: certificate.SerialNumber.String(),
		CertificateFingerprint: certificateFingerprint(certificate.Raw),
	}, nil
}

// ServerTLSConfig requires a CA-verified runner certificate on every connection.
func (authority *CredentialAuthority) ServerTLSConfig(
	serverCertificate tls.Certificate,
) (*tls.Config, error) {
	if len(serverCertificate.Certificate) == 0 {
		return nil, errors.New("SecondBox runner control server certificate is required")
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(authority.caCertificate)
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs,
	}, nil
}

func certificateFingerprint(certificateDER []byte) string {
	digest := sha256.Sum256(certificateDER)
	return hex.EncodeToString(digest[:])
}

func runnerIDFromCertificate(certificate *x509.Certificate) (string, error) {
	for _, identityURI := range certificate.URIs {
		if identityURI.Scheme != "spiffe" || identityURI.Host != "secondbox" {
			continue
		}
		const prefix = "/runner/"
		if !strings.HasPrefix(identityURI.Path, prefix) {
			continue
		}
		runnerID, err := url.PathUnescape(strings.TrimPrefix(identityURI.Path, prefix))
		if err == nil && runnerID != "" && !strings.Contains(runnerID, "/") {
			return runnerID, nil
		}
	}
	return "", errors.New("SecondBox runner certificate has no valid runner identity URI")
}
