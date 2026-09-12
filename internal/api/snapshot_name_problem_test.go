package api

import (
	"net/http"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestSnapshotNameConflictProblem(t *testing.T) {
	status, code, _, retryable := classifyError(ports.ErrSnapshotNameConflict)
	if status != http.StatusConflict || code != "snapshot_name_conflict" || retryable {
		t.Fatalf("problem=%d %s retryable=%v", status, code, retryable)
	}
}
