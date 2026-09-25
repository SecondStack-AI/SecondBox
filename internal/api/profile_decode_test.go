package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestProfileDecodeRetainsStrictIngressAndPermissiveReads(t *testing.T) {
	const future = `{"attributedExecution":{"gateway":"gateway","maximumConnections":128},"futurePolicy":{"value":true}}`
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal([]byte(future), &spec); err != nil {
		t.Fatalf("ordinary Profile read must tolerate future fields: %v", err)
	}
	for _, body := range []string{
		`{"spec":` + future + `}`,
		`{"spec":{"attributedExecutionCeiling":null}}`,
		`{"spec":{"attributedExecutionCeiling":{"maximumConnections":128,"gateway":"forged"}}}`,
	} {
		request := httptest.NewRequest("POST", "/v1/profiles/profile/revisions", strings.NewReader(body))
		var revision contracts.ReviseProfileRequest
		if err := decodeStrictJSON(request, &revision); err == nil {
			t.Fatalf("strict Profile ingress accepted %s", body)
		}
	}
	request := httptest.NewRequest("POST", "/v1/profiles/profile/revisions", strings.NewReader(`{"spec":{"attributedExecutionCeiling":{"maximumConnections":4096}}}`))
	var revision contracts.ReviseProfileRequest
	if err := decodeStrictJSON(request, &revision); err != nil {
		t.Fatalf("valid ceiling decoding: %v", err)
	}
}
