package firecracker

import "github.com/SecondStack-AI/SecondBox/runner/internal/config"

func buildFirecrackerConfig(cfg *config.Config, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP string) firecrackerConfig {
	return buildFirecrackerConfigWithPolicy(cfg, kernelPath, rootfsPath, workspacePath, sharedImagePath, vsockUDS, tapName, guestIP, nil)
}
