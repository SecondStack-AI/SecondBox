package fleet

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fleetRoundTripper func(*http.Request) (*http.Response, error)

func (f fleetRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFleetAWSIdentityRejectsWrongRoleAndUnsignedBinding(t *testing.T) {
	for _, endpoint := range []struct{ region, host, partition string }{
		{"us-east-1", "sts.us-east-1.amazonaws.com", "aws"},
		{"us-gov-west-1", "sts.us-gov-west-1.amazonaws.com", "aws-us-gov"},
		{"cn-north-1", "sts.cn-north-1.amazonaws.com.cn", "aws-cn"},
	} {
		t.Run(endpoint.region, func(t *testing.T) {
			now := time.Now().UTC()
			proof := url.Values{"Action": {"GetCallerIdentity"}, "Version": {"2011-06-15"}, "X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-SignedHeaders": {"host;x-secondbox-enrollment"}, "X-Amz-Expires": {"60"}, "X-Amz-Date": {now.Format("20060102T150405Z")}}
			valid := "https://" + endpoint.host + "/?" + proof.Encode()
			caller := "arn:" + endpoint.partition + ":sts::123456789012:assumed-role/fleet-worker/i-0123456789abcdef0"
			calls := 0
			client := &http.Client{Transport: fleetRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Header.Get("X-SecondBox-Enrollment") != "csr-binding" {
					t.Fatal("CSR proof header absent")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>" + caller + "</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>"))}, nil
			})}
			cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-0123456789abcdef0", RunnerID: "runner-i-0123456789abcdef0", CreatedAt: now.Add(-time.Minute), State: "running"}}}
			verifier := AWSIdentityVerifier{AccountID: "123456789012", RoleName: "fleet-worker", Region: endpoint.region, Cloud: cloud, Client: client, Now: func() time.Time { return now }, EnrollmentWindow: time.Hour}
			request := EnrollmentRequest{InstanceID: "i-0123456789abcdef0", IdentityProofURL: valid}
			if err := verifier.Verify(t.Context(), request, "csr-binding", false); err != nil {
				t.Fatal(err)
			}
			caller = "arn:" + endpoint.partition + ":sts::123456789012:assumed-role/wrong/i-0123456789abcdef0"
			if err := verifier.Verify(t.Context(), request, "csr-binding", false); err == nil {
				t.Fatal("wrong role accepted")
			}
			for _, invalid := range []string{
				strings.Replace(valid, endpoint.host, "attacker.example", 1),
				strings.Replace(valid, "host%3Bx-secondbox-enrollment", "host", 1),
				strings.Replace(valid, "X-Amz-Expires=60", "X-Amz-Expires=600", 1),
				strings.Replace(valid, now.Format("20060102T150405Z"), now.Add(-time.Hour).Format("20060102T150405Z"), 1),
			} {
				request.IdentityProofURL = invalid
				before := calls
				if err := verifier.Verify(t.Context(), request, "csr-binding", false); err == nil {
					t.Fatalf("accepted invalid proof %s", invalid)
				}
				if calls != before {
					t.Fatal("invalid proof reached STS")
				}
			}
		})
	}
}
