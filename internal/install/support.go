package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const maximumInstallerSupportFileBytes = 2 << 20

var supportNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)

// WriteSupportBundle writes bounded, redacted installer evidence. Callers may
// add only already-sanitized operational projections; secret files and
// Workspace contents are never opened for collection.
func WriteSupportBundle(output string, plan InstallPlan, receipt InstallReceipt, evidence map[string][]byte) error {
	if !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return installerError("support bundle output must be an absolute normalized path", nil)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return err
	}
	files := make(map[string][]byte, len(evidence)+4)
	for name, content := range evidence {
		if !supportNamePattern.MatchString(name) || len(content) > maximumInstallerSupportFileBytes {
			return installerError("support evidence name or size is invalid: "+name, nil)
		}
		files[name] = slices.Clone(content)
	}
	facts, err := Canonical(plan.HostFacts)
	if err != nil {
		return err
	}
	receiptBytes, err := Canonical(receipt)
	if err != nil {
		return err
	}
	files["preflight-redacted.json"] = append(facts, '\n')
	files["install-receipt.json"] = append(receiptBytes, '\n')
	files["plan-digest.txt"] = []byte(digest + "\n")
	if err := rejectSupportSecrets(plan, files); err != nil {
		return err
	}
	files["SHA256SUMS"] = supportChecksums(files)
	return writeSupportArchive(output, files)
}

func rejectSupportSecrets(plan InstallPlan, files map[string][]byte) error {
	secrets := [][]byte{}
	for _, target := range plan.SecretTargets {
		info, err := os.Lstat(target.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumInstallerSupportFileBytes {
			return installerError("support secret target is not a bounded protected file", err)
		}
		content, err := os.ReadFile(target.Path)
		if err != nil {
			return err
		}
		content = bytes.TrimSpace(content)
		if len(content) >= 8 {
			secrets = append(secrets, content)
		}
	}
	for name, content := range files {
		lower := strings.ToLower(string(content))
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "private key-----") || strings.Contains(lower, `"token"`) {
			return installerError("support evidence contains a secret-bearing marker: "+name, nil)
		}
		for _, secret := range secrets {
			if bytes.Contains(content, secret) {
				return installerError("support evidence contains installed secret material: "+name, nil)
			}
		}
	}
	return nil
}

func supportChecksums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var result strings.Builder
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		fmt.Fprintf(&result, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	}
	return []byte(result.String())
}

func writeSupportArchive(path string, files map[string][]byte) (resultErr error) {
	archive, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return installerError("create support bundle", err)
	}
	compressor := gzip.NewWriter(archive)
	writer := tar.NewWriter(compressor)
	defer func() {
		resultErr = errors.Join(resultErr, writer.Close(), compressor.Close(), archive.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(path))
		}
	}()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		content := files[name]
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			return err
		}
		if _, err := io.Copy(writer, bytes.NewReader(content)); err != nil {
			return err
		}
	}
	return nil
}
