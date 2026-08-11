//go:build linux

package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func acceptedFixture(t *testing.T) (string, InstallPlan, string) {
	t.Helper()
	directory := t.TempDir()
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePreflight, plan.CreatedAt, map[string]string{"hostFactsDigest": plan.HostFactsDigest}); err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePlanAccepted, plan.CreatedAt, map[string]string{"reviewed": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteAccepted(directory, plan, receipt); err != nil {
		t.Fatal(err)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	return directory, plan, digest
}

func TestReadAcceptedUsesNoFollowOwnershipModeAndDigestFence(t *testing.T) {
	directory, plan, digest := acceptedFixture(t)
	got, receipt, err := ReadAccepted(directory, digest, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationID != plan.OperationID || receipt.CompletedStages[1].Stage != StagePlanAccepted {
		t.Fatalf("accepted operation changed: %#v %#v", got, receipt)
	}
	if _, _, err := ReadAccepted(directory, "sha256:"+strings.Repeat("0", 64), os.Getuid()); err == nil {
		t.Fatal("wrong digest succeeded")
	}
	if _, _, err := ReadAccepted(directory, digest, os.Getuid()+1); err == nil {
		t.Fatal("wrong owner succeeded")
	}
}

func TestReadAcceptedRejectsSymlinkedPathComponentsAndHardlinks(t *testing.T) {
	directory, _, digest := acceptedFixture(t)
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "operation")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadAccepted(link, digest, os.Getuid()); err == nil {
		t.Fatal("symlinked operation directory succeeded")
	}
	planPath := filepath.Join(directory, "install-plan.json")
	if err := os.Link(planPath, filepath.Join(directory, "plan-hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadAccepted(directory, digest, os.Getuid()); err == nil {
		t.Fatal("multiply-linked plan succeeded")
	}
}

func TestReadAcceptedRejectsChangedReceiptBoundary(t *testing.T) {
	directory, plan, digest := acceptedFixture(t)
	receiptBytes, err := os.ReadFile(filepath.Join(directory, "install-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := DecodeReceipt(receiptBytes, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StageHostApply, plan.CreatedAt, nil); err != nil {
		t.Fatal(err)
	}
	encoded, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "install-receipt.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadAccepted(directory, digest, os.Getuid()); err == nil {
		t.Fatal("post-acceptance receipt succeeded at private apply boundary")
	}
	if _, completed, err := ReadHostApply(directory, digest, os.Getuid()); err != nil || completed.CompletedStages[len(completed.CompletedStages)-1].Stage != StageHostApply {
		t.Fatalf("completed host apply was unavailable for privileged replay: %#v, %v", completed.CompletedStages, err)
	}
}

func TestReadHostApplyRejectsPreAcceptanceReceipt(t *testing.T) {
	directory, plan, digest := acceptedFixture(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePreflight, plan.CreatedAt, nil); err != nil {
		t.Fatal(err)
	}
	encoded, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "install-receipt.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHostApply(directory, digest, os.Getuid()); err == nil {
		t.Fatal("pre-acceptance receipt reached private host apply")
	}
}

func TestSaveOperationCommitsActivatedReleaseAndReadRecoversIt(t *testing.T) {
	directory := t.TempDir()
	plan := validPlan(t)
	receipt := successfulReceipt(t, plan)
	if _, _, err := WriteAccepted(directory, plan, receipt); err != nil {
		t.Fatal(err)
	}
	started := plan.CreatedAt.Add(time.Hour)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, targetRelease(plan, "0.5.0"), started); err != nil {
		t.Fatal(err)
	}
	for index, stage := range UpdateStageSequence {
		if err := receipt.CompleteUpdateStage(stage, started.Add(time.Duration(index+1)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := receipt.ActivateUpdate(&plan, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := SaveOperation(directory, plan, receipt, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	readPlan, readReceipt, err := ReadOperation(directory, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if readPlan.Release.Version != "0.5.0" || len(readReceipt.Updates) != 1 || readReceipt.Updates[0].Status != UpdateSucceeded {
		t.Fatalf("committed operation = plan %#v receipt %#v", readPlan.Release, readReceipt.Updates)
	}
	for _, name := range []string{operationPlanStageName, operationReceiptStageName, operationCommitMarkerName} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatalf("commit residue %s: %v", name, err)
		}
	}

	planBytes, _ := Canonical(plan)
	receiptBytes, _ := Canonical(receipt)
	marker := operationCommitMarker{SchemaVersion: "secondbox.install.operation-commit/v1", OperationID: plan.OperationID, PlanDocumentDigest: Digest(planBytes), ReceiptDocumentDigest: Digest(receiptBytes)}
	markerBytes, _ := Canonical(marker)
	if err := os.WriteFile(filepath.Join(directory, operationPlanStageName), append(bytes.Clone(planBytes), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, operationReceiptStageName), append(bytes.Clone(receiptBytes), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, operationCommitMarkerName), append(markerBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadOperationReadOnly(directory, os.Getuid()); err == nil || !strings.Contains(err.Error(), "update --resume") {
		t.Fatalf("read-only operation unexpectedly recovered pending commit: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, operationCommitMarkerName)); err != nil {
		t.Fatalf("read-only operation changed pending commit: %v", err)
	}
	lock, err := AcquireLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecoverOperation(directory, os.Getuid(), lock); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverOperationDiscardsSafeMarkerlessStagedDocuments(t *testing.T) {
	directory, plan, _ := acceptedFixture(t)
	planBytes, err := Canonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, operationPlanStageName), append(planBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadOperation(directory, os.Getuid()); err == nil || !strings.Contains(err.Error(), "update --resume") {
		t.Fatalf("markerless stage did not fence read-only operation: %v", err)
	}
	lock, err := AcquireLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	recoveredPlan, _, recoverErr := RecoverOperation(directory, os.Getuid(), lock)
	closeErr := lock.Close()
	if recoverErr != nil || closeErr != nil {
		t.Fatalf("locked markerless recovery = %v, close = %v", recoverErr, closeErr)
	}
	if recoveredPlan.OperationID != plan.OperationID {
		t.Fatalf("recovered operation = %s, want %s", recoveredPlan.OperationID, plan.OperationID)
	}
	if _, err := os.Lstat(filepath.Join(directory, operationPlanStageName)); !os.IsNotExist(err) {
		t.Fatalf("markerless staged plan remains: %v", err)
	}
}

func TestRecoverOperationRequiresMatchingLiveLock(t *testing.T) {
	directory, _, _ := acceptedFixture(t)
	otherDirectory, _, _ := acceptedFixture(t)
	if _, _, err := RecoverOperation(directory, os.Getuid(), nil); err == nil || !strings.Contains(err.Error(), "matching operation lock") {
		t.Fatalf("nil recovery lock = %v", err)
	}
	lock, err := AcquireLock(otherDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecoverOperation(directory, os.Getuid(), lock); err == nil || !strings.Contains(err.Error(), "matching operation lock") {
		t.Fatalf("mismatched recovery lock = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecoverOperation(otherDirectory, os.Getuid(), lock); err == nil || !strings.Contains(err.Error(), "matching operation lock") {
		t.Fatalf("closed recovery lock = %v", err)
	}
}
