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

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
)

type renewalVerifierFunc func(context.Context, EnrollmentRequest, string, bool) error

type renewalGatewayFunc func(context.Context, string, string, time.Duration) (string, string, error)

func (f renewalGatewayFunc) Issue(ctx context.Context, runner, csr string, lifetime time.Duration) (string, string, error) {
	return f(ctx, runner, csr, lifetime)
}

func (renewalGatewayFunc) Retire(context.Context, string) error { return nil }

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
	t.Run("slow gateway permits heartbeat and retirement", func(t *testing.T) {
		now = start.Add(4 * time.Hour)
		request.IdentityProofURL = proof()
		expectedBinding = EnrollmentBinding(request)
		if _, err := database.Exec(t.Context(), `INSERT INTO secondbox.runner_pools(name,state,architectures_json,capabilities_json,ready_runner_count,revision,created_at,updated_at) VALUES('code','ready','["amd64"]','[]',0,1,$1,$1)`, now); err != nil {
			t.Fatal(err)
		}
		stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), database.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		defer stateStore.Close()
		const connection = "renewal-live-connection"
		if err := stateStore.OpenConnection(t.Context(), runnercontrol.RunnerIdentity{RunnerID: initial.RunnerID, CredentialSerial: "renewal-live-leaf"}, connection, 8, now); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(t.Context(), `UPDATE secondbox.runners SET active_connection_id=$2 WHERE id=$1`, initial.RunnerID, connection); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		authority.Gateway = renewalGatewayFunc(func(ctx context.Context, _, _ string, _ time.Duration) (string, string, error) {
			close(entered)
			select {
			case <-release:
				// A response already signed before retirement can arrive late.
				return "renewed-gateway-certificate", "gateway-ca", nil
			case <-ctx.Done():
				return "", "", ctx.Err()
			}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := authority.Enroll(ctx, request); done <- err }()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("renewal did not reach gateway: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		heartbeatCtx, heartbeatCancel := context.WithTimeout(t.Context(), time.Second)
		_, heartbeatErr := stateStore.RecordHeartbeat(heartbeatCtx, &runnerv1.RunnerHeartbeat{
			MessageId: "renewing-heartbeat", Sequence: 1, RunnerId: initial.RunnerID, ConnectionId: connection,
			ObservedAtUnixMs: uint64(now.UnixMilli()), DrainPhase: runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
			StartupTiming: &runnerv1.StartupTiming{SampleCount: 1, P95Milliseconds: 100},
			Allocatable:   &runnerv1.Capacity{VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8, Operations: 32},
			Reserved:      &runnerv1.Capacity{}, StoragePressure: &runnerv1.StoragePressureObservation{Status: "healthy", ObservedAtUnixMs: uint64(now.UnixMilli())},
		}, now)
		heartbeatCancel()
		retireCtx, retireCancel := context.WithTimeout(t.Context(), time.Second)
		_, retirementErr := database.Exec(retireCtx, `UPDATE secondbox.runners SET fleet_state='retired' WHERE id=$1`, initial.RunnerID)
		retireCancel()
		close(release)
		renewalErr := <-done
		authority.Gateway = gateway
		if heartbeatErr != nil {
			t.Errorf("renewal blocked live heartbeat: %v", heartbeatErr)
		}
		if retirementErr != nil {
			t.Errorf("renewal blocked retirement: %v", retirementErr)
		}
		if renewalErr == nil {
			t.Error("late issuer reply bypassed retirement")
		}
	})
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
