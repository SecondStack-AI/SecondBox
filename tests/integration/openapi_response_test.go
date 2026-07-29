package integration_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/tests/openapicheck"
)

var (
	contractOnce     sync.Once
	contractDocument *openapicheck.Document
	contractLoadErr  error
)

// canonicalContract loads the OpenAPI document once for the whole suite.
func canonicalContract(t *testing.T) *openapicheck.Document {
	t.Helper()
	contractOnce.Do(func() {
		_, sourceFile, _, ok := runtime.Caller(0)
		if !ok {
			contractLoadErr = errors.New("cannot resolve integration source path")
			return
		}
		contractDocument, contractLoadErr = openapicheck.Load(filepath.Join(
			filepath.Dir(sourceFile), "..", "..",
			"contracts", "openapi", "v1", "secondbox.openapi.json",
		))
	})
	if contractLoadErr != nil {
		t.Fatalf("load canonical contract: %v", contractLoadErr)
	}
	return contractDocument
}

// contractServer starts one control-plane test server whose every JSON response
// is validated against the canonical contract as the suite exercises it.
//
// Static contract tests prove the document is self-consistent; they cannot
// prove the server honours it. Routing every integration request through this
// wrapper means a handler that stops emitting a required property, or starts
// emitting one the contract never declared, fails the suite at the call site
// that produced it.
func contractServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(canonicalContract(t).Handler(t.Errorf, handler))
}

// newFixtureID produces identifiers that satisfy the contract's OpaqueID
// constraints. Short sequential identifiers such as "sbx_8" can never occur in
// production, where NewOpaqueID emits far longer values, so fixtures that used
// them were exercising shapes the contract forbids.
func newFixtureID(prefix string) string {
	return fmt.Sprintf("%s_%08d", prefix, integrationIdentitySequence.Add(1))
}
