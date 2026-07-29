package runnercontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const testRunnerCredential = "runner-test-pre-shared-credential-material"

func TestPeerIdentityRejectsUnknownOrMismatchedRunnerCredentialAtConnect(t *testing.T) {
	caCertificate, caPrivateKey := testRunnerCertificateAuthority(t)
	clientCertificate := testRunnerClientCertificate(t, caCertificate, caPrivateKey, "runner-1")
	authority, err := NewCredentialAuthority(CredentialAuthorityConfig{
		Credential: testRunnerCredential, CACertificate: caCertificate,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{config: ServerConfig{CredentialVerifier: authority}}
	peerContext := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{clientCertificate},
		}},
	})

	for _, testCase := range []struct {
		name        string
		credentials []string
	}{
		{name: "absent"},
		{name: "empty", credentials: []string{""}},
		{name: "unknown", credentials: []string{"unknown-runner-credential-material-000000"}},
		{name: "multiple", credentials: []string{testRunnerCredential, testRunnerCredential}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := peerContext
			if testCase.credentials != nil {
				ctx = metadata.NewIncomingContext(
					ctx, metadata.Pairs(runnerCredentialMetadata, testCase.credentials[0]),
				)
				if len(testCase.credentials) > 1 {
					ctx = metadata.NewIncomingContext(
						peerContext,
						metadata.MD{runnerCredentialMetadata: testCase.credentials},
					)
				}
			}
			if _, err := server.peerIdentity(ctx); !errors.Is(err, ErrRunnerCredentialInvalid) {
				t.Fatalf("peerIdentity error = %v, want ErrRunnerCredentialInvalid", err)
			}
		})
	}

	validContext := metadata.NewIncomingContext(
		peerContext, metadata.Pairs(runnerCredentialMetadata, testRunnerCredential),
	)
	identity, err := server.peerIdentity(validContext)
	if err != nil {
		t.Fatal(err)
	}
	if identity.RunnerID != "runner-1" {
		t.Fatalf("RunnerID = %q, want runner-1", identity.RunnerID)
	}
}

func testRunnerCertificateAuthority(t *testing.T) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SecondBox test runner CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, privateKey
}

func testRunnerClientCertificate(
	t *testing.T,
	caCertificate *x509.Certificate,
	caPrivateKey ed25519.PrivateKey,
	runnerID string,
) *x509.Certificate {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: runnerID},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{{
			Scheme: "spiffe", Host: "secondbox", Path: "/runner/" + runnerID,
		}},
	}
	der, err := x509.CreateCertificate(
		rand.Reader, template, caCertificate, publicKey, caPrivateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
