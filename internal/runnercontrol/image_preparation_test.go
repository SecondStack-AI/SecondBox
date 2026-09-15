package runnercontrol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestImagePreparationResultAuthorityAndReplay(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	_, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.lifecycle_effects
		(id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
		 command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
		 claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,created_at,updated_at)
		VALUES ('prepare-op','sandbox',1,'prepare_image','queued','','','runner-authorized',
		 'prepare-command','','',0,0,$1,'',$2,'','','{}','{}',$2,$2)`, now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	write := func(runner string, result *runnerv1.PrepareImageResult, at time.Time) error {
		tx, err := store.pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err := recordImagePreparationResult(t.Context(), tx, runner, result, at); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	result := &runnerv1.PrepareImageResult{OperationId: "prepare-op", ResolvedDigest: "sha256:" + strings.Repeat("a", 64), Manifest: []byte("signed manifest"), Signature: []byte("signature")}
	if err := write("runner-other", result, now); err == nil {
		t.Fatal("unassigned Runner supplied preparation evidence")
	}
	invalid := *result
	invalid.ResolvedDigest = "invalid" + strings.Repeat("a", 64)
	if err := write("runner-authorized", &invalid, now); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	if err := write("runner-authorized", result, now); err != nil {
		t.Fatal(err)
	}
	if err := write("runner-authorized", &runnerv1.PrepareImageResult{OperationId: "prepare-op", Failure: "late failure"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var state string
	var evidence []byte
	if err := store.pool.QueryRow(t.Context(), `SELECT state,evidence_json FROM secondbox.lifecycle_effects WHERE id='prepare-op'`).Scan(&state, &evidence); err != nil {
		t.Fatal(err)
	}
	var stored runnerv1.PrepareImageResult
	if err := json.Unmarshal(evidence, &stored); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || stored.ResolvedDigest != result.ResolvedDigest || stored.Failure != "" {
		t.Fatalf("terminal preparation was changed: %s %s", state, evidence)
	}
}
