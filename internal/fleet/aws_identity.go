package fleet

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type AWSIdentityVerifier struct {
	AccountID        string
	RoleName         string
	Region           string
	Cloud            Cloud
	Client           *http.Client
	Now              func() time.Time
	EnrollmentWindow time.Duration
}

// existingKeyBound is supplied only after the authority has verified the
// instance's stored key binding. A request's renewal flag alone grants nothing.
func (verifier AWSIdentityVerifier) Verify(ctx context.Context, enrollment EnrollmentRequest, binding string, existingKeyBound bool) error {
	if verifier.AccountID == "" || verifier.RoleName == "" || verifier.Region == "" || verifier.Cloud == nil || verifier.Client == nil || verifier.Now == nil || verifier.EnrollmentWindow <= 0 {
		return errors.New("SecondBox AWS fleet enrollment verifier is incomplete")
	}
	proof, err := url.Parse(enrollment.IdentityProofURL)
	expectedHost := "sts." + verifier.Region + ".amazonaws.com"
	if err != nil || proof.Scheme != "https" || proof.Host != expectedHost || proof.User != nil || proof.Fragment != "" || proof.Path != "/" || len(proof.RawQuery) > 8192 {
		return errors.New("SecondBox AWS fleet enrollment proof endpoint is invalid")
	}
	query := proof.Query()
	if query.Get("Action") != "GetCallerIdentity" || query.Get("Version") != "2011-06-15" || query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" ||
		query.Get("X-Amz-SignedHeaders") != "host;x-secondbox-enrollment" {
		return errors.New("SecondBox AWS fleet enrollment proof lacks its signed CSR binding")
	}
	expires, err := strconv.Atoi(query.Get("X-Amz-Expires"))
	if err != nil || expires < 1 || expires > 60 {
		return errors.New("SecondBox AWS fleet enrollment proof expiry is invalid")
	}
	issued, err := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
	if err != nil {
		return errors.New("SecondBox AWS fleet enrollment proof time is invalid")
	}
	now := verifier.Now().UTC()
	if issued.After(now.Add(30*time.Second)) || now.After(issued.Add(time.Duration(expires)*time.Second)) {
		return errors.New("SecondBox AWS fleet enrollment proof expired")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, proof.String(), nil)
	if err != nil {
		return errors.New("SecondBox AWS fleet enrollment proof request is invalid")
	}
	request.Header.Set("X-SecondBox-Enrollment", binding)
	proofClient := *verifier.Client
	proofClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("SecondBox AWS fleet enrollment STS redirects are forbidden")
	}
	response, err := proofClient.Do(request)
	if err != nil {
		return errors.New("SecondBox AWS fleet enrollment STS transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("SecondBox AWS fleet enrollment STS proof was refused")
	}
	var caller struct {
		Result struct {
			Account string `xml:"Account"`
			ARN     string `xml:"Arn"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&caller); err != nil {
		return errors.New("SecondBox AWS fleet enrollment STS response is invalid")
	}
	expectedARN := "arn:aws:sts::" + verifier.AccountID + ":assumed-role/" + verifier.RoleName + "/" + enrollment.InstanceID
	if caller.Result.Account != verifier.AccountID || caller.Result.ARN != expectedARN {
		return errors.New("SecondBox AWS fleet enrollment caller is not its expected instance role")
	}
	nodes, err := verifier.Cloud.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node.InstanceID == enrollment.InstanceID && node.RunnerID == "runner-"+enrollment.InstanceID &&
			(node.State == "pending" || node.State == "running") && (existingKeyBound || now.Sub(node.CreatedAt) <= verifier.EnrollmentWindow) {
			return nil
		}
	}
	return errors.New("SecondBox AWS fleet enrollment instance is absent, stale or outside its fleet")
}

func ValidEnrollmentEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && !strings.Contains(parsed.Host, " ")
}
