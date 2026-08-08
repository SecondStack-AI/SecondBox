package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func WriteAccepted(directory string, plan InstallPlan, receipt InstallReceipt) (string, string, error) {
	if err := plan.Validate(); err != nil {
		return "", "", err
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return "", "", err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", installerError("operation directory must be an existing non-symbolic-link directory", err)
	}
	planPath := filepath.Join(directory, "install-plan.json")
	receiptPath := filepath.Join(directory, "install-receipt.json")
	planBytes, err := Canonical(plan)
	if err != nil {
		return "", "", err
	}
	receiptBytes, err := Canonical(receipt)
	if err != nil {
		return "", "", err
	}
	if err := writeCreateOnly(planPath, append(planBytes, '\n')); err != nil {
		return "", "", err
	}
	if err := writeCreateOnly(receiptPath, append(receiptBytes, '\n')); err != nil {
		return "", "", errors.Join(err, os.Remove(planPath))
	}
	return planPath, receiptPath, nil
}

func writeCreateOnly(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return installerError("create "+path, err)
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(installerError("write "+path, err), os.Remove(path))
	}
	return nil
}

func ReadPlan(path string) (InstallPlan, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return InstallPlan{}, nil, installerError("plan must be a mode-0600 non-symbolic-link regular file", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return InstallPlan{}, nil, fmt.Errorf("SecondBox installer read plan: %w", err)
	}
	plan, err := DecodePlan(content)
	return plan, content, err
}
