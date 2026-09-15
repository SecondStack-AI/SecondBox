// Package executionimage retrieves and verifies client-selected OCI execution bundles.
package executionimage

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

var (
	imageDigestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	cacheDirectoryPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*/[A-Za-z0-9][A-Za-z0-9._/-]*(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127}|@sha256:[a-f0-9]{64})$`)
)

const selectedBundleDirectory = "secondbox-runner-microvm"

type PreparedImage struct {
	Directory          string
	RequestedReference string
	ResolvedDigest     string
	Artifacts          []runtimemanager.VerifiedExecutionImageArtifact
	Release            func()
}

type CapacityReservation func(context.Context, uint64) (func() error, error)

var ErrCapacityAdmissionDenied = errors.New("SecondBox execution image capacity admission denied")

type Manager struct {
	fixedDirectory       string
	fixedPublicKeyPath   string
	fixedPublicKeySHA256 string
	cacheRoot            string
	registryAllowlist    []string
	registryCertificates string
	publicKeyPath        string
	publicKeySHA256      string
	skopeoPath           string
	maximumDownloadBytes int64
	maximumExpandedBytes int64
	maximumCacheBytes    int64
	coldPreparationGate  chan struct{}
	cacheMu              sync.RWMutex
	pinMu                sync.Mutex
	pins                 map[string]int
}

func NewManager(cfg *config.Config) (*Manager, error) {
	if cfg == nil || !filepath.IsAbs(cfg.ExecutionImageCacheRoot) || len(cfg.ExecutionImageRegistryAllowlist) == 0 || !filepath.IsAbs(cfg.ExecutionImageRegistryCertificates) || cfg.ExecutionImageMaximumDownloadBytes <= 0 || cfg.ExecutionImageMaximumExpandedBytes <= 0 || cfg.ExecutionImageMaximumCacheBytes < cfg.ExecutionImageMaximumExpandedBytes {
		return nil, errors.New("SecondBox execution image manager requires absolute cache and registry certificate roots, a registry allowlist, positive download and expansion limits, and a cache limit at least as large as the expansion limit")
	}
	if err := os.MkdirAll(cfg.ExecutionImageCacheRoot, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image cache creation failed: %w", err)
	}
	if err := os.MkdirAll(cfg.ExecutionImageRegistryCertificates, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image registry certificate directory creation failed: %w", err)
	}
	return &Manager{
		fixedDirectory:       filepath.Dir(cfg.MicroVMKernelPath),
		fixedPublicKeyPath:   cfg.MicroVMPublicKeyPath,
		fixedPublicKeySHA256: cfg.MicroVMPublicKeySHA256,
		cacheRoot:            cfg.ExecutionImageCacheRoot,
		registryAllowlist:    append([]string(nil), cfg.ExecutionImageRegistryAllowlist...),
		registryCertificates: cfg.ExecutionImageRegistryCertificates,
		publicKeyPath:        cfg.ExecutionImagePublicKeyPath,
		publicKeySHA256:      cfg.ExecutionImagePublicKeySHA256,
		maximumDownloadBytes: cfg.ExecutionImageMaximumDownloadBytes,
		maximumExpandedBytes: cfg.ExecutionImageMaximumExpandedBytes,
		maximumCacheBytes:    cfg.ExecutionImageMaximumCacheBytes,
		coldPreparationGate:  make(chan struct{}, 1),
		pins:                 make(map[string]int),
	}, nil
}

func (manager *Manager) Fetch(
	ctx context.Context,
	operationID string,
	image *runnerprotocol.ExecutionImage,
	authDocument []byte,
	progress func(runnerprotocol.AssignmentProgressStage) error,
	reserveCapacity CapacityReservation,
) (prepared PreparedImage, resultErr error) {
	if image == nil || !imageReferencePattern.MatchString(image.Reference) {
		return PreparedImage{}, errors.New("SecondBox execution image reference is invalid")
	}
	if reserveCapacity == nil {
		return PreparedImage{}, errors.New("SecondBox execution image capacity reservation is required")
	}
	registry := strings.SplitN(image.Reference, "/", 2)[0]
	if !slices.Contains(manager.registryAllowlist, registry) {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image registry %q is not allowed", registry)
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_RESOLVE); err != nil {
		return PreparedImage{}, err
	}
	authFile, cleanupAuth, err := manager.writeAuthFile(authDocument)
	if err != nil {
		return PreparedImage{}, err
	}
	defer cleanupAuth()
	inspectCertificateArgs := manager.registryCertificateArgs(registry, "--cert-dir")
	resolvedDigest, err := manager.resolveOperationDigest(ctx, operationID, image.Reference, authFile, inspectCertificateArgs)
	if err != nil {
		return PreparedImage{}, err
	}
	cacheDirectory := filepath.Join(manager.cacheRoot, strings.TrimPrefix(resolvedDigest, "sha256:"))
	lock, err := manager.lockDigest(ctx, resolvedDigest)
	if err != nil {
		return PreparedImage{}, err
	}
	defer lock.Close()
	defer func() {
		if resultErr != nil || prepared.Directory == "" {
			return
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			resultErr = errors.New("SecondBox image preparation requires a deadline")
		} else {
			resultErr = retainPreparedDirectory(prepared.Directory, deadline)
		}
		if resultErr != nil {
			prepared.Release()
			prepared = PreparedImage{}
		}
	}()
	manager.cacheMu.RLock()
	if _, err := os.Stat(cacheDirectory); err == nil {
		artifacts, err := manager.verifyAndCaptureArtifacts(ctx, cacheDirectory)
		if err != nil {
			manager.cacheMu.RUnlock()
			return PreparedImage{}, fmt.Errorf("SecondBox cached execution image verification failed: %w", err)
		}
		if err := os.Chtimes(cacheDirectory, time.Now(), time.Now()); err != nil {
			manager.cacheMu.RUnlock()
			return PreparedImage{}, fmt.Errorf("SecondBox execution image cache access-time update failed: %w", err)
		}
		prepared := manager.preparedImage(cacheDirectory, image.Reference, resolvedDigest, artifacts)
		manager.cacheMu.RUnlock()
		return prepared, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		manager.cacheMu.RUnlock()
		return PreparedImage{}, fmt.Errorf("SecondBox execution image cache inspection failed: %w", err)
	}
	manager.cacheMu.RUnlock()
	select {
	case manager.coldPreparationGate <- struct{}{}:
		defer func() { <-manager.coldPreparationGate }()
	case <-ctx.Done():
		return PreparedImage{}, context.Cause(ctx)
	}
	manager.cacheMu.RLock()
	_, cacheErr := os.Stat(cacheDirectory)
	manager.cacheMu.RUnlock()
	if cacheErr == nil {
		manager.cacheMu.RLock()
		artifacts, err := manager.verifyAndCaptureArtifacts(ctx, cacheDirectory)
		if err != nil {
			manager.cacheMu.RUnlock()
			return PreparedImage{}, fmt.Errorf("SecondBox cached execution image verification failed: %w", err)
		}
		prepared := manager.preparedImage(cacheDirectory, image.Reference, resolvedDigest, artifacts)
		manager.cacheMu.RUnlock()
		return prepared, nil
	}
	manager.cacheMu.Lock()
	if err := manager.ensureCacheCapacity(cacheDirectory); err != nil {
		manager.cacheMu.Unlock()
		return PreparedImage{}, err
	}
	manager.cacheMu.Unlock()
	preparationBytes, err := preparationCapacityBytes(manager.maximumDownloadBytes, manager.maximumExpandedBytes)
	if err != nil {
		return PreparedImage{}, err
	}
	releaseCapacity, err := manager.reservePreparationCapacity(ctx, cacheDirectory, preparationBytes, reserveCapacity)
	if err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image staging capacity reservation failed: %w", err)
	}
	defer func() {
		if releaseErr := releaseCapacity(); releaseErr != nil {
			if prepared.Release != nil {
				prepared.Release()
				prepared = PreparedImage{}
			}
			resultErr = errors.Join(resultErr, releaseErr)
		}
	}()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_DOWNLOAD); err != nil {
		return PreparedImage{}, err
	}
	workDirectory, err := os.MkdirTemp(manager.cacheRoot, ".execution-image-")
	if err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image staging creation failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDirectory) }()
	archivePath := filepath.Join(workDirectory, "image.tar")
	args := []string{"copy"}
	args = append(args, manager.registryCertificateArgs(registry, "--src-cert-dir")...)
	if authFile != "" {
		args = append(args, "--authfile", authFile)
	}
	repository := strings.SplitN(image.Reference, "@", 2)[0]
	if separator := strings.LastIndex(repository, ":"); separator > strings.LastIndex(repository, "/") {
		repository = repository[:separator]
	}
	args = append(args, "docker://"+repository+"@"+resolvedDigest, "docker-archive:"+archivePath)
	output, err := manager.downloadArchive(ctx, archivePath, args)
	if err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image download failed: %w: %s", err, boundedOutput(output))
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_EXTRACT); err != nil {
		return PreparedImage{}, err
	}
	candidate := filepath.Join(workDirectory, "bundle")
	if err := extractDockerArchive(ctx, archivePath, candidate, manager.maximumDownloadBytes, manager.maximumExpandedBytes); err != nil {
		return PreparedImage{}, err
	}
	artifacts, err := manager.verifyAndCaptureArtifacts(ctx, candidate)
	if err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image signature verification failed: %w", err)
	}
	if err := os.Rename(candidate, cacheDirectory); err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image cache publication failed: %w", err)
	}
	artifacts, err = relocateVerifiedArtifacts(cacheDirectory, artifacts)
	if err != nil {
		return PreparedImage{}, err
	}
	return manager.preparedImage(cacheDirectory, image.Reference, resolvedDigest, artifacts), nil
}

func (manager *Manager) reservePreparationCapacity(
	ctx context.Context,
	target string,
	requestedBytes uint64,
	reserve CapacityReservation,
) (func() error, error) {
	for {
		release, err := reserve(ctx, requestedBytes)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrCapacityAdmissionDenied) {
			return nil, err
		}
		manager.cacheMu.Lock()
		evicted, evictionErr := manager.evictOldestUnpinned(target)
		manager.cacheMu.Unlock()
		if evictionErr != nil {
			return nil, evictionErr
		}
		if !evicted {
			return nil, err
		}
	}
}

func (manager *Manager) downloadArchive(ctx context.Context, archivePath string, args []string) ([]byte, error) {
	command := exec.CommandContext(ctx, manager.skopeoPath, args...)
	// Skopeo's temporary layers belong in the reserved cache staging area.
	command.Env = append(os.Environ(), "TMPDIR="+filepath.Dir(archivePath))
	var output boundedLogWriter
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return output.Bytes(), err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				if err := manager.checkDownloadStaging(filepath.Dir(archivePath)); err != nil {
					return output.Bytes(), err
				}
				if info, statErr := os.Stat(archivePath); statErr != nil {
					return output.Bytes(), statErr
				} else if info.Size() > manager.maximumDownloadBytes {
					return output.Bytes(), fmt.Errorf("downloaded archive exceeds %d bytes", manager.maximumDownloadBytes)
				}
			}
			return output.Bytes(), err
		case <-ticker.C:
			if err := manager.checkDownloadStaging(filepath.Dir(archivePath)); err != nil {
				_ = command.Process.Kill()
				<-done
				return output.Bytes(), err
			}
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-done
			return output.Bytes(), context.Cause(ctx)
		}
	}
}

func (manager *Manager) checkDownloadStaging(directory string) error {
	remaining := uint64(manager.maximumDownloadBytes) * 2
	return filepath.WalkDir(directory, func(_ string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil // Skopeo can remove a temporary layer during inspection.
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Size() > manager.maximumDownloadBytes || uint64(info.Size()) > remaining {
			return errors.New("SecondBox image download staging exceeds its reserved size limit")
		}
		remaining -= uint64(info.Size())
		return nil
	})
}

type boundedLogWriter struct {
	mu   sync.Mutex
	data []byte
}

func (writer *boundedLogWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	const maximum = 64 << 10
	remaining := maximum - len(writer.data)
	if remaining > 0 {
		writer.data = append(writer.data, data[:min(len(data), remaining)]...)
	}
	return len(data), nil
}

func (writer *boundedLogWriter) Bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.data...)
}

type cacheEntry struct {
	path     string
	size     int64
	modified time.Time
}

func (manager *Manager) ensureCacheCapacity(target string) error {
	entries, total, err := manager.cacheEntries()
	if err != nil {
		return err
	}
	required, err := preparationCapacityBytes(manager.maximumDownloadBytes, manager.maximumExpandedBytes)
	if err != nil {
		return err
	}
	available, err := manager.availableCacheBytes()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	for _, entry := range entries {
		if total+manager.maximumExpandedBytes <= manager.maximumCacheBytes && available >= required {
			break
		}
		if entry.path == target {
			continue
		}
		manager.pinMu.Lock()
		pinned := manager.pins[entry.path] > 0
		manager.pinMu.Unlock()
		if pinned {
			continue
		}
		removed, err := manager.evictCacheEntry(entry.path)
		if err != nil {
			return err
		}
		if !removed {
			continue
		}
		total -= entry.size
		available, err = manager.availableCacheBytes()
		if err != nil {
			return err
		}
	}
	if total+manager.maximumExpandedBytes > manager.maximumCacheBytes {
		return fmt.Errorf("SecondBox execution image cache cannot reserve %d bytes within its %d-byte limit", manager.maximumExpandedBytes, manager.maximumCacheBytes)
	}
	if available < required {
		return fmt.Errorf("SecondBox execution image preparation requires %d free bytes, only %d are available", required, available)
	}
	return nil
}

func (manager *Manager) availableCacheBytes() (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(manager.cacheRoot, &stat); err != nil {
		return 0, fmt.Errorf("SecondBox execution image free-space inspection failed: %w", err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func (manager *Manager) evictOldestUnpinned(target string) (bool, error) {
	entries, _, err := manager.cacheEntries()
	if err != nil {
		return false, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	for _, entry := range entries {
		if entry.path == target {
			continue
		}
		manager.pinMu.Lock()
		pinned := manager.pins[entry.path] > 0
		manager.pinMu.Unlock()
		if pinned {
			continue
		}
		removed, err := manager.evictCacheEntry(entry.path)
		if err != nil {
			return false, err
		}
		if removed {
			return true, nil
		}
	}
	return false, nil
}

// Storage pressure requests one pass over expired, unlocked cache entries.
func (manager *Manager) reclaimUnusedImages(reference string) error {
	manager.cacheMu.Lock()
	defer manager.cacheMu.Unlock()
	entries, _, err := manager.cacheEntries()
	if err != nil {
		return err
	}
	_, digest, _ := strings.Cut(reference, "@")
	for _, entry := range entries {
		if filepath.Base(entry.path) == strings.TrimPrefix(digest, "sha256:") {
			continue
		}
		manager.pinMu.Lock()
		pinned := manager.pins[entry.path] > 0
		manager.pinMu.Unlock()
		if pinned {
			continue
		}
		if _, err := manager.evictCacheEntry(entry.path); err != nil {
			return err
		}
	}
	return nil
}

func (manager *Manager) evictCacheEntry(path string) (bool, error) {
	locks := filepath.Join(manager.cacheRoot, ".locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(locks, filepath.Base(path)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	until, err := preparedDirectoryDeadline(path)
	if err != nil {
		return false, err
	}
	if until.After(time.Now()) {
		return false, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("SecondBox execution image cache eviction failed: %w", err)
	}
	return true, nil
}

func preparationCapacityBytes(maximumDownloadBytes, maximumExpandedBytes int64) (uint64, error) {
	if maximumDownloadBytes <= 0 || maximumExpandedBytes <= 0 ||
		uint64(maximumDownloadBytes) > (^uint64(0)-uint64(maximumExpandedBytes))/2 {
		return 0, errors.New("SecondBox execution image preparation capacity is invalid")
	}
	return uint64(maximumDownloadBytes)*2 + uint64(maximumExpandedBytes), nil
}

func (manager *Manager) cacheEntries() ([]cacheEntry, int64, error) {
	directoryEntries, err := os.ReadDir(manager.cacheRoot)
	if err != nil {
		return nil, 0, fmt.Errorf("SecondBox execution image cache read failed: %w", err)
	}
	var entries []cacheEntry
	var total int64
	for _, directoryEntry := range directoryEntries {
		if !directoryEntry.IsDir() || !cacheDirectoryPattern.MatchString(directoryEntry.Name()) {
			continue
		}
		path := filepath.Join(manager.cacheRoot, directoryEntry.Name())
		var size int64
		if err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			return nil
		}); err != nil {
			return nil, 0, fmt.Errorf("SecondBox execution image cache accounting failed: %w", err)
		}
		info, err := directoryEntry.Info()
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, cacheEntry{path: path, size: size, modified: info.ModTime()})
		total += size
	}
	return entries, total, nil
}

func (manager *Manager) verifyAndCaptureArtifacts(ctx context.Context, directory string) ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
	artifacts := make([]runtimemanager.VerifiedExecutionImageArtifact, 0, 3)
	for _, artifact := range []struct{ label, name string }{{"kernel", "kernel"}, {"rootfs", "rootfs.ext4"}, {"shared image", "shared.img"}} {
		identity, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(artifact.label, filepath.Join(directory, artifact.name))
		if err != nil {
			return nil, fmt.Errorf("record execution image %s identity before verification: %w", artifact.label, err)
		}
		artifacts = append(artifacts, identity)
	}
	if err := config.VerifyMicroVMArtifactDirectory(ctx, directory, manager.publicKeyPath, manager.publicKeySHA256); err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		current, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(artifact.Label, artifact.Path)
		if err != nil {
			return nil, fmt.Errorf("record execution image %s identity after verification: %w", artifact.Label, err)
		}
		if !sameVerifiedArtifactIdentity(artifact, current) {
			return nil, fmt.Errorf("SecondBox execution image %s changed during signature verification", artifact.Label)
		}
	}
	return artifacts, nil
}

func relocateVerifiedArtifacts(directory string, artifacts []runtimemanager.VerifiedExecutionImageArtifact) ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
	relocated := make([]runtimemanager.VerifiedExecutionImageArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		current, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(artifact.Label, filepath.Join(directory, filepath.Base(artifact.Path)))
		if err != nil {
			return nil, fmt.Errorf("record published execution image %s identity: %w", artifact.Label, err)
		}
		if !sameVerifiedArtifactIdentity(artifact, current) {
			return nil, fmt.Errorf("SecondBox execution image %s changed during cache publication", artifact.Label)
		}
		relocated = append(relocated, current)
	}
	return relocated, nil
}

func sameVerifiedArtifactIdentity(left, right runtimemanager.VerifiedExecutionImageArtifact) bool {
	left.Path = ""
	right.Path = ""
	return left == right
}

func (manager *Manager) preparedImage(directory, reference, digest string, artifacts []runtimemanager.VerifiedExecutionImageArtifact) PreparedImage {
	manager.pinMu.Lock()
	if manager.pins == nil {
		manager.pins = make(map[string]int)
	}
	manager.pins[directory]++
	manager.pinMu.Unlock()
	var once sync.Once
	return PreparedImage{
		Directory: directory, RequestedReference: reference, ResolvedDigest: digest, Artifacts: artifacts,
		Release: func() {
			once.Do(func() {
				manager.pinMu.Lock()
				defer manager.pinMu.Unlock()
				manager.pins[directory]--
				if manager.pins[directory] == 0 {
					delete(manager.pins, directory)
				}
			})
		},
	}
}

type operationResolution struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

func (manager *Manager) resolveOperationDigest(ctx context.Context, operationID, reference, authFile string, certificateArgs []string) (string, error) {
	if operationID == "" {
		return "", errors.New("SecondBox execution image resolution requires an Operation ID")
	}
	resolutionDirectory := filepath.Join(manager.cacheRoot, ".resolutions")
	if err := os.MkdirAll(resolutionDirectory, 0o700); err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution directory failed: %w", err)
	}
	operationHash := sha256.Sum256([]byte(operationID))
	path := filepath.Join(resolutionDirectory, fmt.Sprintf("%x.json", operationHash[:]))
	if document, err := os.ReadFile(path); err == nil {
		var resolution operationResolution
		if json.Unmarshal(document, &resolution) != nil || resolution.Reference != reference || !imageDigestPattern.MatchString(resolution.Digest) {
			return "", errors.New("SecondBox persisted execution image resolution is invalid")
		}
		return resolution.Digest, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("SecondBox execution image resolution read failed: %w", err)
	}
	digest, err := manager.resolveDigest(ctx, reference, authFile, certificateArgs)
	if err != nil {
		return "", err
	}
	document, err := json.Marshal(operationResolution{Reference: reference, Digest: digest})
	if err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution encoding failed: %w", err)
	}
	temporary, err := os.CreateTemp(resolutionDirectory, ".resolution-")
	if err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution staging failed: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("SecondBox execution image resolution permissions failed: %w", err)
	}
	if _, err := temporary.Write(document); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("SecondBox execution image resolution write failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution close failed: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution publication failed: %w", err)
	}
	return digest, nil
}

func (manager *Manager) resolveDigest(ctx context.Context, reference, authFile string, certificateArgs []string) (string, error) {
	args := []string{"inspect", "--format", "{{.Digest}}"}
	args = append(args, certificateArgs...)
	if authFile != "" {
		args = append(args, "--authfile", authFile)
	}
	args = append(args, "docker://"+reference)
	output, err := exec.CommandContext(ctx, manager.skopeoPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("SecondBox execution image resolution failed: %w: %s", err, boundedOutput(output))
	}
	digest := strings.TrimSpace(string(output))
	if !imageDigestPattern.MatchString(digest) {
		return "", errors.New("SecondBox execution image registry returned an invalid manifest digest")
	}
	if separator := strings.LastIndex(reference, "@sha256:"); separator >= 0 && reference[separator+1:] != digest {
		return "", errors.New("SecondBox execution image digest does not match the requested reference")
	}
	return digest, nil
}

func (manager *Manager) registryCertificateArgs(registry, flag string) []string {
	certificateDirectory := filepath.Join(manager.registryCertificates, registry)
	if _, err := os.Stat(filepath.Join(certificateDirectory, "ca.crt")); err == nil {
		return []string{flag, certificateDirectory}
	}
	return nil
}

func (manager *Manager) writeAuthFile(document []byte) (string, func(), error) {
	if !json.Valid(document) {
		return "", nil, errors.New("SecondBox execution image authentication document is invalid")
	}
	directory, err := os.MkdirTemp(manager.cacheRoot, ".registry-auth-")
	if err != nil {
		return "", nil, fmt.Errorf("SecondBox execution image auth staging failed: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("SecondBox execution image auth write failed: %w", err)
	}
	return path, cleanup, nil
}

func (manager *Manager) lockDigest(ctx context.Context, digest string) (*os.File, error) {
	lockDirectory := filepath.Join(manager.cacheRoot, ".locks")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image lock directory failed: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDirectory, strings.TrimPrefix(digest, "sha256:")+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image lock open failed: %w", err)
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("SecondBox execution image lock failed: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, context.Cause(ctx)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func extractDockerArchive(ctx context.Context, archivePath, target string, maximumArchiveBytes, maximumExpandedBytes int64) error {
	archiveDirectory, err := os.MkdirTemp(filepath.Dir(target), ".docker-archive-")
	if err != nil {
		return fmt.Errorf("SecondBox execution image archive staging failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(archiveDirectory) }()
	archiveBudget := &byteBudget{maximum: maximumArchiveBytes}
	if err := extractTarFile(ctx, archivePath, archiveDirectory, "", archiveBudget); err != nil {
		return err
	}
	manifestBytes, err := config.ReadArtifactMetadata(filepath.Join(archiveDirectory, "manifest.json"), config.MaximumArtifactManifestBytes)
	if err != nil {
		return fmt.Errorf("SecondBox execution image archive manifest read failed: %w", err)
	}
	var manifests []struct {
		Layers []string `json:"Layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifests); err != nil || len(manifests) != 1 || len(manifests[0].Layers) == 0 {
		return errors.New("SecondBox execution image archive manifest is invalid")
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("SecondBox execution image bundle creation failed: %w", err)
	}
	expandedBudget := &byteBudget{maximum: maximumExpandedBytes}
	for _, layer := range manifests[0].Layers {
		cleanLayer := filepath.Clean(layer)
		if filepath.IsAbs(cleanLayer) || cleanLayer == "." || strings.HasPrefix(cleanLayer, ".."+string(filepath.Separator)) {
			return errors.New("SecondBox execution image archive layer path is unsafe")
		}
		layerPath := filepath.Join(archiveDirectory, cleanLayer)
		if err := applyLayerWhiteouts(ctx, layerPath, target, selectedBundleDirectory+"/"); err != nil {
			return err
		}
		if err := extractTarFile(ctx, layerPath, target, selectedBundleDirectory+"/", expandedBudget); err != nil {
			return err
		}
	}
	return nil
}

type byteBudget struct {
	used    int64
	maximum int64
}

func (budget *byteBudget) consume(size int64) error {
	if size < 0 || size > budget.maximum-budget.used {
		return fmt.Errorf("SecondBox execution image extraction exceeds %d bytes", budget.maximum)
	}
	budget.used += size
	return nil
}

func extractTarFile(ctx context.Context, path, target, requiredPrefix string, budget *byteBudget) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox execution image tar open failed: %w", err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("SecondBox execution image tar read failed: %w", err)
		}
		name := filepath.Clean(header.Name)
		if name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.New("SecondBox execution image tar path is unsafe")
		}
		if requiredPrefix != "" {
			if !strings.HasPrefix(name, requiredPrefix) {
				continue
			}
			name = strings.TrimPrefix(name, requiredPrefix)
			if name == "" {
				continue
			}
		}
		destination := filepath.Join(target, name)
		if !strings.HasPrefix(destination, filepath.Clean(target)+string(filepath.Separator)) {
			return errors.New("SecondBox execution image tar destination is unsafe")
		}
		base := filepath.Base(name)
		if requiredPrefix != "" && strings.HasPrefix(base, ".wh.") {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if info, err := os.Lstat(destination); err == nil && !info.IsDir() {
				if err := os.RemoveAll(destination); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := budget.consume(header.Size); err != nil {
				return err
			}
			if err := os.RemoveAll(destination); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, contextReader{ctx: ctx, reader: reader}, header.Size)
			closeErr := output.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		case tar.TypeLink, tar.TypeSymlink:
			if requiredPrefix != "" {
				return fmt.Errorf("SecondBox execution image layer entry %q cannot be a link", header.Name)
			}
			linkTarget := filepath.Clean(filepath.Join(filepath.Dir(destination), header.Linkname))
			if !strings.HasPrefix(linkTarget, filepath.Clean(target)+string(filepath.Separator)) {
				return errors.New("SecondBox execution image tar link target is unsafe")
			}
			info, err := os.Stat(linkTarget)
			if err != nil {
				return fmt.Errorf("SecondBox execution image tar link target is invalid: %w", err)
			}
			if !info.Mode().IsRegular() {
				return errors.New("SecondBox execution image tar link target is not a regular file")
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if err := os.Link(linkTarget, destination); err != nil {
				return fmt.Errorf("SecondBox execution image tar link materialization failed: %w", err)
			}
		default:
			return fmt.Errorf("SecondBox execution image tar entry %q has unsupported type", header.Name)
		}
	}
}

func applyLayerWhiteouts(ctx context.Context, path, target, requiredPrefix string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox execution image tar open failed: %w", err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("SecondBox execution image tar read failed: %w", err)
		}
		name := filepath.Clean(header.Name)
		if !strings.HasPrefix(name, requiredPrefix) {
			continue
		}
		name = strings.TrimPrefix(name, requiredPrefix)
		base := filepath.Base(name)
		if !strings.HasPrefix(base, ".wh.") {
			continue
		}
		destination := filepath.Join(target, name)
		if !strings.HasPrefix(destination, filepath.Clean(target)+string(filepath.Separator)) {
			return errors.New("SecondBox execution image whiteout destination is unsafe")
		}
		if base == ".wh..wh..opq" {
			entries, err := os.ReadDir(filepath.Dir(destination))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			for _, entry := range entries {
				if err := os.RemoveAll(filepath.Join(filepath.Dir(destination), entry.Name())); err != nil {
					return err
				}
			}
			continue
		}
		whiteoutName := strings.TrimPrefix(base, ".wh.")
		if whiteoutName == "" || whiteoutName == "." || whiteoutName == ".." {
			return errors.New("SecondBox execution image whiteout name is unsafe")
		}
		if err := os.RemoveAll(filepath.Join(filepath.Dir(destination), whiteoutName)); err != nil {
			return err
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := context.Cause(reader.ctx); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func boundedOutput(output []byte) string {
	const maximum = 1024
	text := strings.TrimSpace(string(output))
	if len(text) > maximum {
		text = text[:maximum]
	}
	return text
}
