package lifecycle

import "testing"

func TestAutomaticStopCorrelationIsComplete(t *testing.T) {
	operationID, requestID := stopCorrelation("", "", "stop-effect-1")
	if operationID != "stop-effect-1" || requestID != "request-stop-effect-1" {
		t.Fatalf("automatic stop correlation = %q/%q", operationID, requestID)
	}
}

func TestPublicStopCorrelationIsPreserved(t *testing.T) {
	operationID, requestID := stopCorrelation(
		"operation-stop-1",
		"request-stop-1",
		"stop-effect-1",
	)
	if operationID != "operation-stop-1" || requestID != "request-stop-1" {
		t.Fatalf("public stop correlation = %q/%q", operationID, requestID)
	}
}
