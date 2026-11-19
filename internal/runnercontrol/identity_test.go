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
	"net"
	"net/url"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const testRunnerCredential = "runner-test-pre-shared-credential-material"

func TestRunnerConnectAcceptsEnrollmentCertificateChain(t *testing.T) {
	ca, caKey := testRunnerCertificateAuthority(t)
	keyPair := func(runner bool) tls.Certificate {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(30 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if runner {
			template.Subject.CommonName = "runner-fleet"
			template.DNSNames = nil
			template.URIs = []*url.URL{{Scheme: "spiffe", Host: "secondbox", Path: "/runner/runner-fleet"}}
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, public, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: private}
	}
	authority, err := NewCredentialAuthority(CredentialAuthorityConfig{Credential: testRunnerCredential, CACertificate: ca})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := authority.ServerTLSConfig(keyPair(false))
	if err != nil {
		t.Fatal(err)
	}
	config := validServerConfig()
	config.CredentialVerifier = authority
	config.SupportedVersions = VersionRange{Minimum: runnerv1.SupportedProtocolMinimum, Maximum: runnerv1.SupportedProtocolMaximum}
	config.EnabledFeatures = []runnerv1.RunnerFeature{runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE}
	control, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runnerv1.RegisterRunnerControlServer(server, control)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		if err := <-served; err != nil {
			t.Error(err)
		}
	}()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, test := range []struct {
		name, credential string
		chain, accepted  bool
	}{
		{name: "leaf only", credential: testRunnerCredential, accepted: true},
		{name: "fleet leaf and issuing CA", credential: testRunnerCredential, chain: true, accepted: true},
		{name: "chain cannot replace credential", credential: "unknown-runner-credential-material-000000", chain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pair := keyPair(true)
			if !test.chain {
				pair.Certificate = pair.Certificate[:1]
			}
			connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{pair}})))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream, err := runnerv1.NewRunnerControlClient(connection).Connect(metadata.AppendToOutgoingContext(ctx, runnerCredentialMetadata, test.credential))
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(helloFrame("runner-fleet", runnerv1.SupportedProtocolMinimum, runnerv1.SupportedProtocolMaximum)); err != nil {
				t.Fatal(err)
			}
			response, err := stream.Recv()
			if !test.accepted {
				if err == nil {
					t.Fatal("unrecognized credential accepted with a trusted chain")
				}
				return
			}
			if err != nil || response.GetWelcome() == nil {
				t.Fatalf("authenticated Runner welcome=%v error=%v", response.GetWelcome(), err)
			}
		})
	}
}

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
