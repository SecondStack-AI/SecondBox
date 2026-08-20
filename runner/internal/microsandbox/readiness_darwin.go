//go:build darwin

package microsandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

func platformReadiness(ctx context.Context, config validatedConfig) error {
	if runtime.GOARCH != "arm64" {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness requires arm64")
	}
	hypervisor, err := unix.SysctlUint32("kern.hv_support")
	if err != nil || hypervisor != 1 {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness Hypervisor.framework: value=%d error=%v", hypervisor, err)
	}
	bundleRoot := filepath.Dir(filepath.Dir(config.HelperExecutable))
	expected := map[string]string{
		"helper":    filepath.Join(bundleRoot, "bin", "secondbox-microsandbox-helper"),
		"agentd":    filepath.Join(bundleRoot, "bin", "agentd"),
		"libkrunfw": filepath.Join(bundleRoot, "lib", "libkrunfw.5.dylib"),
	}
	for name, path := range map[string]string{
		"helper": config.HelperExecutable, "agentd": config.AgentdPath, "libkrunfw": config.LibkrunfwPath,
	} {
		actualPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("SecondBox Microsandbox Darwin readiness resolve %s: %w", name, err)
		}
		expectedPath, err := filepath.EvalSymlinks(expected[name])
		if err != nil || actualPath != expectedPath {
			return fmt.Errorf("SecondBox Microsandbox Darwin readiness %s is outside the installed runtime bundle", name)
		}
	}
	for _, path := range []string{config.HelperExecutable, config.LibkrunfwPath} {
		command := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", path)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf(
				"SecondBox Microsandbox Darwin readiness code signature: %w: %s",
				err,
				strings.TrimSpace(string(output)),
			)
		}
	}
	entitlementOutput, err := exec.CommandContext(
		ctx, "/usr/bin/codesign", "-d", "--entitlements", ":-", config.HelperExecutable,
	).CombinedOutput()
	if err != nil ||
		!bytes.Contains(entitlementOutput, []byte("<key>com.apple.security.hypervisor</key>")) ||
		!bytes.Contains(entitlementOutput, []byte("<key>com.apple.security.cs.disable-library-validation</key>")) {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness helper entitlement is incomplete: %w", err)
	}
	if !slices.Contains(config.manifest.AgentFeatures, "tcp") ||
		!slices.Contains(config.manifest.AgentFeatures, "exec-streaming") {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness network or stream feature is absent")
	}
	runtimeProbe, err := os.MkdirTemp("/tmp", "sbx-ready-")
	if err != nil {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness create short runtime path: %w", err)
	}
	if canonical, err := filepath.EvalSymlinks(runtimeProbe); err != nil ||
		!strings.HasPrefix(canonical, "/private/tmp/") && !strings.HasPrefix(canonical, "/tmp/") {
		_ = os.Remove(runtimeProbe)
		return fmt.Errorf(
			"SecondBox Microsandbox Darwin readiness short runtime path identity: path=%q error=%v",
			canonical,
			err,
		)
	}
	if err := os.Remove(runtimeProbe); err != nil {
		return fmt.Errorf("SecondBox Microsandbox Darwin readiness cleanup: %w", err)
	}
	return nil
}
