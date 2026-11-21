package executionimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"golang.org/x/sys/unix"
)

// Verification records let a restarted Runner admit a bundle it verified
// before without hashing its gigabytes again. They live in the Runner's own
// verification root, outside the cache the unprivileged image fetcher owns,
// and the Runner trusts only a root and record files it owns with owner-only
// access, opened without following links.
const (
	verificationRecordSchemaVersion = 1
	maximumVerificationRecordBytes  = 64 << 10
)

// validateVerificationRoot keeps the record root out of the fetcher's cache.
// The fetcher's own Manager has no record root.
func validateVerificationRoot(cacheRoot, verificationRoot string) error {
	if verificationRoot == "" {
		return nil
	}
	if !filepath.IsAbs(verificationRoot) || filepath.Clean(verificationRoot) != verificationRoot {
		return errors.New("SecondBox execution image verification record root must be a clean absolute path")
	}
	if pathsOverlap(cacheRoot, verificationRoot) {
		return errors.New("SecondBox execution image verification record root must be outside the execution image cache")
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	return left == right ||
		strings.HasPrefix(left, right+string(filepath.Separator)) ||
		strings.HasPrefix(right, left+string(filepath.Separator))
}

type verificationRecord struct {
	SchemaVersion int                                             `json:"schemaVersion"`
	Directory     string                                          `json:"directory"`
	Artifacts     []runtimemanager.VerifiedExecutionImageArtifact `json:"artifacts"`
	Identities    []runtimemanager.VerifiedExecutionImageArtifact `json:"identities"`
}

func verificationRecordName(directory string) string {
	sum := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(sum[:]) + ".json"
}

// openVerificationRecords opens the record root, creating it when asked. It
// returns nil without error when the root is absent and not created.
func (manager *Manager) openVerificationRecords(create bool) (*os.File, error) {
	if manager.verificationRoot == "" {
		return nil, errors.New("SecondBox execution image verification requires a verification record root")
	}
	path := manager.verificationRoot
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		if !create {
			return nil, nil
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("SecondBox execution image verification record root creation failed: %w", err)
		}
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image verification record root open failed: %w", err)
	}
	directory := os.NewFile(uintptr(fd), path)
	if err := requireRunnerPrivate(fd, unix.S_IFDIR); err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("SecondBox execution image verification record root %s: %w", path, err)
	}
	return directory, nil
}

func requireRunnerPrivate(fd int, fileType uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != fileType {
		return errors.New("has an unexpected file type")
	}
	if stat.Uid != uint32(os.Geteuid()) || mode&0o077 != 0 {
		return errors.New("is not owned by the Runner with owner-only access")
	}
	return nil
}

// loadVerificationRecord returns the recorded verification of directory, or
// false when the Runner has none in the current schema.
func (manager *Manager) loadVerificationRecord(directory string) (bundleVerification, bool, error) {
	manager.recordMu.Lock()
	defer manager.recordMu.Unlock()
	records, err := manager.openVerificationRecords(false)
	if err != nil || records == nil {
		return bundleVerification{}, false, err
	}
	defer records.Close()
	name := verificationRecordName(directory)
	fd, err := unix.Openat(int(records.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return bundleVerification{}, false, nil
	}
	if err != nil {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record open failed: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := requireRunnerPrivate(fd, unix.S_IFREG); err != nil {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record %s: %w", name, err)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumVerificationRecordBytes+1))
	if err != nil {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record read failed: %w", err)
	}
	if len(content) > maximumVerificationRecordBytes {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record %s is too large", name)
	}
	var record verificationRecord
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record %s is invalid: %w", name, err)
	}
	if record.SchemaVersion != verificationRecordSchemaVersion {
		return bundleVerification{}, false, nil
	}
	if err := validateVerificationRecord(directory, record); err != nil {
		return bundleVerification{}, false, fmt.Errorf("SecondBox execution image verification record %s: %w", name, err)
	}
	return bundleVerification{artifacts: record.Artifacts, identities: record.Identities}, true, nil
}

// validateVerificationRecord requires the record to name exactly the files a
// bundle verification of directory reads, so its identities are the ones
// checked before the record admits a launch.
func validateVerificationRecord(directory string, record verificationRecord) error {
	if record.Directory != directory {
		return errors.New("names another bundle directory")
	}
	if len(record.Identities) != len(config.BundleArtifactFileNames) {
		return errors.New("does not cover every bundle file")
	}
	for index, name := range config.BundleArtifactFileNames {
		if record.Identities[index].Label != name || record.Identities[index].Path != filepath.Join(directory, name) {
			return fmt.Errorf("bundle file %s identity is misplaced", name)
		}
	}
	if len(record.Artifacts) == 0 {
		return errors.New("has no verified artifacts")
	}
	for _, artifact := range record.Artifacts {
		if filepath.Dir(artifact.Path) != directory || !slices.Contains(config.BundleArtifactFileNames, filepath.Base(artifact.Path)) {
			return fmt.Errorf("verified artifact %s is outside the bundle", artifact.Label)
		}
	}
	return nil
}

// storeVerificationRecord atomically replaces the record of directory and
// removes records whose bundle directory no longer exists.
func (manager *Manager) storeVerificationRecord(directory string, verification bundleVerification) error {
	content, err := json.Marshal(verificationRecord{
		SchemaVersion: verificationRecordSchemaVersion,
		Directory:     directory,
		Artifacts:     verification.artifacts,
		Identities:    verification.identities,
	})
	if err != nil {
		return err
	}
	manager.recordMu.Lock()
	defer manager.recordMu.Unlock()
	records, err := manager.openVerificationRecords(true)
	if err != nil {
		return err
	}
	defer records.Close()
	recordsFD := int(records.Fd())
	if err := pruneVerificationRecords(records); err != nil {
		return err
	}
	name := verificationRecordName(directory)
	temporary := ".tmp-" + name
	if err := unix.Unlinkat(recordsFD, temporary, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("SecondBox execution image verification record cleanup failed: %w", err)
	}
	fd, err := unix.Openat(recordsFD, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox execution image verification record creation failed: %w", err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("SecondBox execution image verification record write failed: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("SecondBox execution image verification record sync failed: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("SecondBox execution image verification record close failed: %w", err)
	}
	if err := unix.Renameat(recordsFD, temporary, recordsFD, name); err != nil {
		return fmt.Errorf("SecondBox execution image verification record publication failed: %w", err)
	}
	if err := records.Sync(); err != nil {
		return fmt.Errorf("SecondBox execution image verification record directory sync failed: %w", err)
	}
	return nil
}

func pruneVerificationRecords(records *os.File) error {
	names, err := records.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("SecondBox execution image verification record listing failed: %w", err)
	}
	recordsFD := int(records.Fd())
	for _, name := range names {
		if strings.HasPrefix(name, ".tmp-") {
			continue
		}
		fd, err := unix.Openat(recordsFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("SecondBox execution image verification record %s open failed: %w", name, err)
		}
		file := os.NewFile(uintptr(fd), name)
		var record verificationRecord
		decodeErr := json.NewDecoder(io.LimitReader(file, maximumVerificationRecordBytes)).Decode(&record)
		_ = file.Close()
		if decodeErr != nil {
			return fmt.Errorf("SecondBox execution image verification record %s is invalid: %w", name, decodeErr)
		}
		if _, err := os.Stat(record.Directory); errors.Is(err, os.ErrNotExist) {
			if err := unix.Unlinkat(recordsFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("SecondBox execution image verification record %s removal failed: %w", name, err)
			}
		} else if err != nil {
			return fmt.Errorf("SecondBox execution image verification record %s bundle inspection failed: %w", name, err)
		}
	}
	return nil
}
