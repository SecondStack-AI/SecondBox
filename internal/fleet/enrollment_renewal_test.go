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
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type renewalVerifierFunc func(context.Context, EnrollmentRequest, string, bool) error

func (f renewalVerifierFunc) Verify(ctx context.Context, r EnrollmentRequest, b string, existing bool) error {
	return f(ctx, r, b, existing)
}

func TestFleetSameKeyRenewalAfterExpiryStillRequiresFreshCloudOwnership(t *testing.T) {
	database := openFleetTestDatabase(t)
	start := time.Now().UTC().Truncate(time.Second)
	now := start
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "renewal-ca"}, NotBefore: start.Add(-time.Hour), NotAfter: start.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	newKey := func() *ecdsa.PrivateKey {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		return k
	}
	csr := func(k *ecdsa.PrivateKey) string {
		der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k)
		if e != nil {
			t.Fatal(e)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	}
	runnerKey, gatewayKey := newKey(), newKey()
	request := EnrollmentRequest{InstanceID: "i-0123456789abcdef0", Nonce: fmt.Sprintf("%064x", 1), RunnerCSR: csr(runnerKey), GatewayCSR: csr(gatewayKey)}
	proof := func() string {
		q := url.Values{"Action": {"GetCallerIdentity"}, "Version": {"2011-06-15"}, "X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-SignedHeaders": {"host;x-secondbox-enrollment"}, "X-Amz-Expires": {"60"}, "X-Amz-Date": {now.Format("20060102T150405Z")}}
		return "https://sts.us-east-1.amazonaws.com/?" + q.Encode()
	}
	calls := 0
	callerRole := "worker"
	expectedBinding := ""
	client := &http.Client{Transport: fleetRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-SecondBox-Enrollment") != expectedBinding {
			t.Fatal("fresh STS proof lost CSR/mode binding")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:sts::123456789012:assumed-role/" + callerRole + "/" + request.InstanceID + "</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>"))}, nil
	})}
	node := Node{InstanceID: request.InstanceID, RunnerID: "runner-" + request.InstanceID, State: "running", CreatedAt: start.Add(-time.Minute)}
	cloud := &fleetCloudFixture{nodes: []Node{node}}
	verifier := AWSIdentityVerifier{AccountID: "123456789012", RoleName: "worker", Region: "us-east-1", Cloud: cloud, Client: client, Now: func() time.Time { return now }, EnrollmentWindow: 10 * time.Minute}
	gateway := &enrollmentGatewayFixture{}
	authority := EnrollmentAuthority{Database: database, Pool: "code", Verifier: verifier, Gateway: gateway, CACertificate: ca, CAKey: key, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Lifetime: time.Hour, RegistryFiles: map[string]string{"tenants.json": "bootstrap-only-secret"}, Now: func() time.Time { return now }}
	enroll := func(r EnrollmentRequest) (EnrollmentResponse, error) {
		expectedBinding = EnrollmentBinding(r)
		return authority.Enroll(t.Context(), r)
	}
	request.IdentityProofURL = proof()
	initial, err := enroll(request)
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(2 * time.Hour)
	request.IdentityProofURL = proof()
	if _, err := enroll(request); err == nil {
		t.Fatal("old instance bypassed bootstrap launch window without renewal")
	}
	request.Renew = true
	// Randomized CSR signatures can differ while the public keys stay fixed.
	request.RunnerCSR, request.GatewayCSR = csr(runnerKey), csr(gatewayKey)
	renewed, err := enroll(request)
	if err != nil {
		t.Fatal(err)
	}
	parse := func(s string) *x509.Certificate {
		b, _ := pem.Decode([]byte(s))
		if b == nil {
			t.Fatal("missing certificate")
		}
		c, e := x509.ParseCertificate(b.Bytes)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	oldLeaf, newLeaf := parse(initial.RunnerCertificate), parse(renewed.RunnerCertificate)
	_, issuingChain := pem.Decode([]byte(initial.RunnerCertificate))
	if string(issuingChain) != authority.CAPEM {
		t.Fatal("bootstrap Runner keypair does not carry its immutable issuing chain")
	}
	if initial.RunnerID != renewed.RunnerID || initial.RunnerCA != renewed.RunnerCA || !newLeaf.NotAfter.After(oldLeaf.NotAfter) || !newLeaf.PublicKey.(*ecdsa.PublicKey).Equal(oldLeaf.PublicKey) || gateway.issued != 2 {
		t.Fatal("renewal changed identity or failed to advance expiry")
	}
	if len(renewed.RegistryFiles) != 0 || renewed.RunnerEnvironment != "" || renewed.GatewayEnvironment != "" {
		t.Fatal("renewal redistributed the bootstrap handoff")
	}
	before := calls
	cached, err := enroll(request)
	if err != nil || cached.RunnerCertificate != renewed.RunnerCertificate || gateway.issued != 2 || calls != before+1 {
		t.Fatalf("cached renewal skipped fresh ownership or reissued: error=%v", err)
	}
	for _, change := range []string{"runner-key", "gateway-key", "new-instance", "wrong-role", "absent-asg", "terminated", "stale-sts"} {
		t.Run(change, func(t *testing.T) {
			candidate := request
			cloud.nodes = []Node{node}
			callerRole = "worker"
			switch change {
			case "runner-key":
				candidate.RunnerCSR = csr(newKey())
			case "gateway-key":
				candidate.GatewayCSR = csr(newKey())
			case "new-instance":
				candidate.InstanceID = "i-11111111111111111"
			case "wrong-role":
				callerRole = "wrong"
			case "absent-asg":
				cloud.nodes = nil
			case "terminated":
				cloud.nodes[0].State = "terminated"
			case "stale-sts":
				candidate.IdentityProofURL = strings.Replace(candidate.IdentityProofURL, now.Format("20060102T150405Z"), start.Format("20060102T150405Z"), 1)
			}
			if _, err := enroll(candidate); err == nil {
				t.Fatal("unsafe renewal accepted")
			}
			if gateway.issued != 2 {
				t.Fatal("refused proof issued gateway certificate")
			}
		})
	}
	callerRole = "worker"
	cloud.nodes = []Node{node}
	if _, err := database.Exec(t.Context(), `INSERT INTO secondbox.runners(id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,software_version,active_connection_id,last_sequence,drain_phase,reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,sandbox_start_p95_milliseconds,revision,created_at,updated_at,fleet_state) VALUES($1,'code','renewal','offline','[]','[]','{}','[]',1,1,'test','',0,'drained','{}','{}',0,0,1,$2,$2,'active')`, initial.RunnerID, now); err != nil {
		t.Fatal(err)
	}
	authority.Verifier = renewalVerifierFunc(func(ctx context.Context, r EnrollmentRequest, b string, existing bool) error {
		if err := verifier.Verify(ctx, r, b, existing); err != nil {
			return err
		}
		_, err := database.Exec(ctx, `UPDATE secondbox.runners SET fleet_state='retired' WHERE id=$1`, initial.RunnerID)
		return err
	})
	if _, err := enroll(request); err == nil {
		t.Fatal("retirement during fresh cloud verification was not rechecked")
	}
	if gateway.issued != 2 {
		t.Fatal("retired Runner reissued gateway authority")
	}
}
