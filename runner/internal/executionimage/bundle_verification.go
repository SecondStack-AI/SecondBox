package executionimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

// Verifying a signed bundle hashes its kernel, rootfs and shared image, which
// are gigabytes. A Sandbox start cannot afford that inside its start deadline,
// so each bundle directory is verified once and afterwards proven unchanged by
// the filesystem metadata of every file the verification read. Metadata drift
// forces one full re-verification. A Runner restart costs one verification per
// bundle it starts, the fixed release bundle included.
type bundleVerification struct {
	artifacts  []runtimemanager.VerifiedExecutionImageArtifact
	identities []runtimemanager.VerifiedExecutionImageArtifact
}

func (manager *Manager) verifiedBundleArtifacts(ctx context.Context, directory, publicKeyPath, publicKeySHA256 string) ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
	return manager.memoizedBundleVerification(directory, publicKeyPath, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return manager.verifyAndCaptureArtifacts(ctx, directory, publicKeyPath, publicKeySHA256)
	})
}

func (manager *Manager) memoizedBundleVerification(
	directory string,
	publicKeyPath string,
	verify func() ([]runtimemanager.VerifiedExecutionImageArtifact, error),
) ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
	if recorded, remembered := manager.rememberedBundleVerification(directory); remembered {
		unchanged, err := bundleIdentitiesUnchanged(recorded.identities)
		if err != nil {
			return nil, err
		}
		if unchanged {
			return slices.Clone(recorded.artifacts), nil
		}
	}
	before, err := captureBundleIdentities(directory, publicKeyPath)
	if err != nil {
		return nil, err
	}
	artifacts, err := verify()
	if err != nil {
		return nil, err
	}
	after, err := captureBundleIdentities(directory, publicKeyPath)
	if err != nil {
		return nil, err
	}
	for index := range before {
		if !sameVerifiedArtifactIdentity(before[index], after[index]) {
			return nil, fmt.Errorf("SecondBox execution image bundle %s changed during signature verification", before[index].Label)
		}
	}
	manager.rememberBundleVerification(directory, bundleVerification{artifacts: artifacts, identities: after})
	return artifacts, nil
}

func (manager *Manager) rememberedBundleVerification(directory string) (bundleVerification, bool) {
	manager.verificationMu.Lock()
	defer manager.verificationMu.Unlock()
	recorded, remembered := manager.verifications[directory]
	return recorded, remembered
}

func (manager *Manager) rememberBundleVerification(directory string, verification bundleVerification) {
	manager.verificationMu.Lock()
	defer manager.verificationMu.Unlock()
	if manager.verifications == nil {
		manager.verifications = make(map[string]bundleVerification)
	}
	for remembered := range manager.verifications {
		// An unreachable bundle directory can no longer start a Sandbox, so its
		// verification is dropped instead of retained for the process lifetime.
		if _, err := os.Stat(remembered); err != nil {
			delete(manager.verifications, remembered)
		}
	}
	manager.verifications[directory] = verification
}

func captureBundleIdentities(directory, publicKeyPath string) ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
	identities := make([]runtimemanager.VerifiedExecutionImageArtifact, 0, len(config.SignedArtifactFileNames)+1)
	for _, name := range config.SignedArtifactFileNames {
		identity, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(name, filepath.Join(directory, name))
		if err != nil {
			return nil, fmt.Errorf("record execution image bundle file %s identity: %w", name, err)
		}
		identities = append(identities, identity)
	}
	publicKey, err := runtimemanager.CaptureVerifiedExecutionImageArtifact("signing public key", publicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("record execution image signing public key identity: %w", err)
	}
	return append(identities, publicKey), nil
}

func bundleIdentitiesUnchanged(identities []runtimemanager.VerifiedExecutionImageArtifact) (bool, error) {
	for _, identity := range identities {
		current, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(identity.Label, identity.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("inspect execution image bundle file %s identity: %w", identity.Label, err)
		}
		if !sameVerifiedArtifactIdentity(identity, current) {
			return false, nil
		}
	}
	return true, nil
}
