package runnercontrol

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type renewalControlServer struct {
	runnerprotocol.UnimplementedRunnerControlServer
}

func (renewalControlServer) Connect(stream runnerprotocol.RunnerControl_ConnectServer) error {
	p, _ := peer.FromContext(stream.Context())
	identity := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0].SerialNumber.String()
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(&runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_Welcome{Welcome: &runnerprotocol.RunnerWelcome{ConnectionId: identity}}}); err != nil {
			return err
		}
	}
}

type renewalCertificates struct {
	ca  *x509.Certificate
	key *ecdsa.PrivateKey
	der []byte
}

func newRenewalAuthority(t *testing.T, name string) renewalCertificates {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, SubjectKeyId: []byte(name), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return renewalCertificates{ca, key, der}
}
func (a renewalCertificates) leaf(t *testing.T, key *ecdsa.PrivateKey, serial int64, expiry time.Time, identity string, server bool) []byte {
	t.Helper()
	uri, _ := url.Parse(identity)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: identity}, NotBefore: time.Now().Add(-time.Minute), NotAfter: expiry, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		leaf.DNSNames = []string{"control-plane"}
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, a.ca, key.Public(), a.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func renewalPrivatePEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
func writeRenewalLeaf(t *testing.T, path string, leaf []byte) {
	t.Helper()
	pending := path + ".new"
	if err := os.WriteFile(pending, leaf, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, path); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCertificateRenewalReconnectsAfterExpiryWithDistinctControlPlaneCA(t *testing.T) {
	runnerCA, serverCA := newRenewalAuthority(t, "runner-ca"), newRenewalAuthority(t, "control-plane-ca")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(now.Unix())
	runnerLeaf := runnerCA.leaf(t, key, 1, now.Add(time.Minute), "spiffe://secondbox/runner/runner-renewal", false)
	serverPair, err := tls.X509KeyPair(serverCA.leaf(t, serverKey, 1, now.Add(12*time.Hour), "control-plane", true), renewalPrivatePEM(t, serverKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(runnerCA.ca)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Time: func() time.Time { return time.Unix(clock.Load(), 0) }})))
	runnerprotocol.RegisterRunnerControlServer(server, renewalControlServer{})
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		if err := <-serveResult; err != nil {
			t.Error(err)
		}
	}()
	directory := t.TempDir()
	certPath, keyPath, caPath := filepath.Join(directory, "runner.crt"), filepath.Join(directory, "runner.key"), filepath.Join(directory, "server-ca.crt")
	writeRenewalLeaf(t, certPath, runnerLeaf)
	if err := os.WriteFile(keyPath, renewalPrivatePEM(t, key), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCA.der}), 0600); err != nil {
		t.Fatal(err)
	}
	connector, err := NewGRPCConnector(GRPCConnectorConfig{Address: listener.Addr().String(), ServerName: "control-plane", ClientCertificate: certPath, ClientKey: keyPath, CertificatePool: caPath, Credential: "renewal-test-private-credential-0000000"})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	exchange := func(stream RunnerProtocolStream) (string, error) {
		if err := stream.Send(&runnerprotocol.RunnerToControlPlane{}); err != nil {
			return "", err
		}
		response, err := stream.Recv()
		if err != nil {
			return "", err
		}
		return response.GetWelcome().ConnectionId, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := connector.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if serial, err := exchange(first); err != nil || serial != "1" {
		t.Fatalf("initial connection serial=%q error=%v", serial, err)
	}
	clock.Store(now.Add(2 * time.Minute).Unix())
	if serial, err := exchange(first); err != nil || serial != "1" {
		t.Fatalf("existing connection was disrupted by expiry: %s %v", serial, err)
	}
	_ = connector.Close()
	expiredCtx, expiredCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer expiredCancel()
	expired, err := connector.Connect(expiredCtx)
	if err == nil {
		_, err = exchange(expired)
	}
	_ = connector.Close()
	if err == nil {
		t.Fatal("new control connection accepted expired leaf")
	}
	renewed := runnerCA.leaf(t, key, 2, now.Add(12*time.Hour), "spiffe://secondbox/runner/runner-renewal", false)
	writeRenewalLeaf(t, certPath, renewed)
	foreign, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := os.WriteFile(keyPath, renewalPrivatePEM(t, foreign), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := connector.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if serial, err := exchange(current); err != nil || serial != "2" {
		t.Fatalf("renewed same-key reconnect serial=%q error=%v", serial, err)
	}
	initial := connector.RunnerCertificate()
	_, pin, err := validateDataPlaneCertificate("runner-renewal", initial)
	if err != nil {
		t.Fatal(err)
	}
	direct := newDataPlaneListener()
	if err := direct.bind("127.0.0.1:0", initial, connector.RunnerCertificateForHandshake); err != nil {
		t.Fatal(err)
	}
	defer direct.close()
	rawListener, serverTLS := direct.listener, direct.tlsConfig.Clone()
	go func() {
		for {
			raw, err := rawListener.Accept()
			if err != nil {
				return
			}
			go func() {
				connection := tls.Server(raw, serverTLS)
				defer connection.Close()
				if err := connection.HandshakeContext(ctx); err != nil {
					return
				}
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	pinned, err := portdirect.TLSConfigForSPKIPin(pin)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := tls.Dial("tcp", direct.address(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if got := connection.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != 2 {
		t.Fatalf("direct listener kept old serial %d", got)
	}
	writeRenewalLeaf(t, certPath, runnerCA.leaf(t, key, 3, now.Add(12*time.Hour), "spiffe://secondbox/runner/runner-renewal", false))
	next, err := tls.Dial("tcp", direct.address(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if got := next.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != 3 {
		t.Fatalf("direct renewal serial %d", got)
	}
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(connection, echo); err != nil || string(echo) != "live" {
		t.Fatalf("renewal disrupted existing direct connection: %q %v", echo, err)
	}
	writeRenewalLeaf(t, certPath, runnerCA.leaf(t, foreign, 4, now.Add(12*time.Hour), "spiffe://secondbox/runner/runner-renewal", false))
	if _, err := connector.RunnerCertificateForHandshake(nil); err == nil {
		t.Fatal("renewal substituted a new private key")
	}
}

func TestRunnerRenewedLeafPreservesPinnedIssuerWhenInitialChainSuppliesIt(t *testing.T) {
	authority := newRenewalAuthority(t, "pinned-issuer")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	identity := "spiffe://secondbox/runner/runner-renewal"
	original := authority.leaf(t, key, 1, now.Add(time.Hour), identity, false)
	chain := append(append([]byte{}, original...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.der})...)
	pair, err := tls.X509KeyPair(chain, renewalPrivatePEM(t, key))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.crt")
	reload, err := newRunnerCertificateReloader(path, pair)
	if err != nil {
		t.Fatal(err)
	}
	rogue := newRenewalAuthority(t, "pinned-issuer")
	for name, leaf := range map[string][]byte{"changed-identity": authority.leaf(t, key, 2, now.Add(time.Hour), identity+"-other", false), "changed-signer": rogue.leaf(t, key, 3, now.Add(time.Hour), identity, false), "expired": authority.leaf(t, key, 4, now.Add(-time.Second), identity, false), "malformed": []byte("invalid")} {
		t.Run(name, func(t *testing.T) {
			writeRenewalLeaf(t, path, leaf)
			if _, err := reload.load(); err == nil {
				t.Fatal("unsafe replacement accepted")
			}
		})
	}
	renewed := authority.leaf(t, key, 5, now.Add(2*time.Hour), identity, false)
	writeRenewalLeaf(t, path, append(renewed, []byte("\n \n")...))
	loaded, err := reload.load()
	block, _ := pem.Decode(renewed)
	if err != nil || !bytes.Equal(loaded.Certificate[0], block.Bytes) {
		t.Fatalf("same issuer renewal error=%v", err)
	}
}
