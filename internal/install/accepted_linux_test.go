//go:build linux

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
