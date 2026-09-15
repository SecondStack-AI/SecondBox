// Package executionimage retrieves and verifies client-selected OCI execution bundles.
package executionimage

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

var (
	imageDigestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*/[A-Za-z0-9][A-Za-z0-9._/-]*(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127}|@sha256:[a-f0-9]{64})$`)
)

const selectedBundleDirectory = "secondbox-runner-microvm"

type PreparedImage struct {
	Directory          string
	RequestedReference string
	ResolvedDigest     string
}

type Manager struct {
	cacheRoot            string
	registryAllowlist    []string
	registryCertificates string
	publicKeyPath        string
	publicKeySHA256      string
	skopeoPath           string
}

func NewManager(cfg *config.Config) (*Manager, error) {
	if cfg == nil || !filepath.IsAbs(cfg.ExecutionImageCacheRoot) || len(cfg.ExecutionImageRegistryAllowlist) == 0 || !filepath.IsAbs(cfg.ExecutionImageRegistryCertificates) {
		return nil, errors.New("SecondBox execution image manager requires absolute cache and registry certificate roots plus a registry allowlist")
	}
	skopeoPath, err := exec.LookPath("skopeo")
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image manager requires skopeo: %w", err)
	}
	if err := os.MkdirAll(cfg.ExecutionImageCacheRoot, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image cache creation failed: %w", err)
	}
	if err := os.MkdirAll(cfg.ExecutionImageRegistryCertificates, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image registry certificate directory creation failed: %w", err)
	}
	return &Manager{
		cacheRoot:            cfg.ExecutionImageCacheRoot,
		registryAllowlist:    append([]string(nil), cfg.ExecutionImageRegistryAllowlist...),
		registryCertificates: cfg.ExecutionImageRegistryCertificates,
		publicKeyPath:        cfg.ExecutionImagePublicKeyPath,
		publicKeySHA256:      cfg.ExecutionImagePublicKeySHA256,
		skopeoPath:           skopeoPath,
	}, nil
}

func (manager *Manager) Prepare(
	ctx context.Context,
	operationID string,
	image *runnerprotocol.ExecutionImage,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (PreparedImage, error) {
	if image == nil || !imageReferencePattern.MatchString(image.Reference) {
		return PreparedImage{}, errors.New("SecondBox execution image reference is invalid")
	}
	registry := strings.SplitN(image.Reference, "/", 2)[0]
	if !slices.Contains(manager.registryAllowlist, registry) {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image registry %q is not allowed", registry)
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_RESOLVE); err != nil {
		return PreparedImage{}, err
	}
	authFile, cleanupAuth, err := manager.writeAuthFile(registry, image.PullCredentials)
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
	lock, err := manager.lockDigest(resolvedDigest)
	if err != nil {
		return PreparedImage{}, err
	}
	defer lock.Close()
	if _, err := os.Stat(cacheDirectory); err == nil {
		if err := config.VerifyMicroVMArtifactDirectory(cacheDirectory, manager.publicKeyPath, manager.publicKeySHA256); err != nil {
			return PreparedImage{}, fmt.Errorf("SecondBox cached execution image verification failed: %w", err)
		}
		return PreparedImage{Directory: cacheDirectory, RequestedReference: image.Reference, ResolvedDigest: resolvedDigest}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image cache inspection failed: %w", err)
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_DOWNLOAD); err != nil {
		return PreparedImage{}, err
	}
	workDirectory, err := os.MkdirTemp(manager.cacheRoot, ".execution-image-")
	if err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image staging creation failed: %w", err)
	}
	defer os.RemoveAll(workDirectory)
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
	command := exec.CommandContext(ctx, manager.skopeoPath, args...)
	if output, err := command.CombinedOutput(); err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image download failed: %w: %s", err, boundedOutput(output))
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_EXTRACT); err != nil {
		return PreparedImage{}, err
	}
	candidate := filepath.Join(workDirectory, "bundle")
	if err := extractDockerArchive(archivePath, candidate); err != nil {
		return PreparedImage{}, err
	}
	if err := config.VerifyMicroVMArtifactDirectory(candidate, manager.publicKeyPath, manager.publicKeySHA256); err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image signature verification failed: %w", err)
	}
	if err := os.Rename(candidate, cacheDirectory); err != nil {
		return PreparedImage{}, fmt.Errorf("SecondBox execution image cache publication failed: %w", err)
	}
	return PreparedImage{Directory: cacheDirectory, RequestedReference: image.Reference, ResolvedDigest: resolvedDigest}, nil
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
	defer os.Remove(temporaryPath)
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

func (manager *Manager) writeAuthFile(registry string, credentials *runnerprotocol.RegistryPullCredentials) (string, func(), error) {
	if credentials == nil {
		return "", func() {}, nil
	}
	if credentials.Token == "" {
		return "", nil, errors.New("SecondBox execution image pull token is empty")
	}
	directory, err := os.MkdirTemp(manager.cacheRoot, ".registry-auth-")
	if err != nil {
		return "", nil, fmt.Errorf("SecondBox execution image auth staging failed: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	auth := base64.StdEncoding.EncodeToString([]byte(credentials.Username + ":" + credentials.Token))
	document, err := json.Marshal(map[string]any{"auths": map[string]any{registry: map[string]string{"auth": auth}}})
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("SecondBox execution image auth encoding failed: %w", err)
	}
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("SecondBox execution image auth write failed: %w", err)
	}
	return path, cleanup, nil
}

func (manager *Manager) lockDigest(digest string) (*os.File, error) {
	lockDirectory := filepath.Join(manager.cacheRoot, ".locks")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image lock directory failed: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDirectory, strings.TrimPrefix(digest, "sha256:")+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image lock open failed: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("SecondBox execution image lock failed: %w", err)
	}
	return lock, nil
}

func extractDockerArchive(archivePath, target string) error {
	archiveDirectory, err := os.MkdirTemp(filepath.Dir(target), ".docker-archive-")
	if err != nil {
		return fmt.Errorf("SecondBox execution image archive staging failed: %w", err)
	}
	defer os.RemoveAll(archiveDirectory)
	if err := extractTarFile(archivePath, archiveDirectory, ""); err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(archiveDirectory, "manifest.json"))
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
	for _, layer := range manifests[0].Layers {
		cleanLayer := filepath.Clean(layer)
		if filepath.IsAbs(cleanLayer) || cleanLayer == "." || strings.HasPrefix(cleanLayer, ".."+string(filepath.Separator)) {
			return errors.New("SecondBox execution image archive layer path is unsafe")
		}
		if err := extractTarFile(filepath.Join(archiveDirectory, cleanLayer), target, selectedBundleDirectory+"/"); err != nil {
			return err
		}
	}
	return nil
}

func extractTarFile(path, target, requiredPrefix string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox execution image tar open failed: %w", err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
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
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		default:
			return fmt.Errorf("SecondBox execution image tar entry %q has unsupported type", header.Name)
		}
	}
}

func boundedOutput(output []byte) string {
	const maximum = 1024
	text := strings.TrimSpace(string(output))
	if len(text) > maximum {
		text = text[:maximum]
	}
	return text
}
