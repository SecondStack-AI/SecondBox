package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"testing"
	"time"

	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type enrollmentVerifierFixture struct{}

func (enrollmentVerifierFixture) Verify(context.Context, EnrollmentRequest, string) error { return nil }

type enrollmentGatewayFixture struct{ issued int }

func (f *enrollmentGatewayFixture) Issue(context.Context, string, string, time.Duration) (string, string, error) {
	f.issued++
	return "gateway-certificate", "gateway-ca", nil
}
func (*enrollmentGatewayFixture) Retire(context.Context, string) error { return nil }

func openFleetTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("SECONDBOX_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := fmt.Sprintf("fleet_enrollment_%d_%d", os.Getpid(), time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)") })
	parsed.Path = "/" + name
	if err := postgresmigrations.Apply(t.Context(), parsed.String()); err != nil {
		t.Fatal(err)
	}
	database, err := pgxpool.New(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database
}

func TestFleetEnrollmentPersistsCertificateBindingWithoutRegistrySecrets(t *testing.T) {
	database := openFleetTestDatabase(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet-test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &enrollmentGatewayFixture{}
	authority := EnrollmentAuthority{Database: database, Pool: "fleet", Verifier: enrollmentVerifierFixture{}, Gateway: gateway, CACertificate: ca, CAKey: key, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Lifetime: time.Hour, Now: func() time.Time { return now }, RegistryFiles: map[string]string{"tenants.json": "registry-secret"}}
	csr := func() string {
		private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, private)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	}
	request := EnrollmentRequest{InstanceID: "i-0123456789abcdef0", Nonce: fmt.Sprintf("%064x", 1), RunnerCSR: csr(), GatewayCSR: csr()}
	response, err := authority.Enroll(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := authority.Enroll(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.RunnerCertificate != replay.RunnerCertificate || gateway.issued != 1 || replay.RegistryFiles["tenants.json"] != "registry-secret" {
		t.Fatal("enrollment replay did not preserve public receipt and private handoff")
	}
	var persisted string
	if err := database.QueryRow(t.Context(), `SELECT response_json::text FROM secondbox.fleet_enrollments WHERE runner_id=$1`, response.RunnerID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var secret bool
	if err := database.QueryRow(t.Context(), `SELECT response_json::text LIKE '%registry-secret%' FROM secondbox.fleet_enrollments WHERE runner_id=$1`, response.RunnerID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret {
		t.Fatal("registry credentials persisted in certificate receipt")
	}
	changed := request
	changed.RunnerCSR = csr()
	if _, err := authority.Enroll(t.Context(), changed); err == nil {
		t.Fatal("changed CSR accepted for existing instance")
	}
	authority.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := authority.Enroll(t.Context(), request); err == nil {
		t.Fatal("expired enrollment replay accepted")
	}
}
