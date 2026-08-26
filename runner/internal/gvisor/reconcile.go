//go:build linux

package gvisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// StaleLoopEvidence is one bounded record of a reconciled leftover device.
type StaleLoopEvidence struct {
	Device      string
	BackingFile string
}

// ReconcileStaleLoops detaches loop devices whose backing file lives under
// the runner's workspace root. Attachment mounts live in supervisor-private
// namespaces and vanish with them, and loop devices are armed with autoclear,
// so leftovers indicate an abnormal termination; startup must clear them
// before readiness. The sysfs root is a parameter only so the scan logic is
// testable against a synthetic tree.
func ReconcileStaleLoops(workspaceRoot string) ([]StaleLoopEvidence, error) {
	return reconcileStaleLoops("/sys/block", "/dev", workspaceRoot)
}

func reconcileStaleLoops(sysfsBlockRoot, deviceRoot, workspaceRoot string) ([]StaleLoopEvidence, error) {
	candidates, err := scanStaleLoops(sysfsBlockRoot, deviceRoot, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var reconciled []StaleLoopEvidence
	var joined error
	for _, candidate := range candidates {
		if err := detachLoopDevice(candidate.Device); err != nil {
			joined = errors.Join(joined, fmt.Errorf("detach stale %s: %w", candidate.Device, err))
			continue
		}
		reconciled = append(reconciled, candidate)
	}
	return reconciled, joined
}

// scanStaleLoops selects the loop devices backed by files under the
// workspace root without touching them.
func scanStaleLoops(sysfsBlockRoot, deviceRoot, workspaceRoot string) ([]StaleLoopEvidence, error) {
	if workspaceRoot == "" || !filepath.IsAbs(workspaceRoot) {
		return nil, errors.New("SecondBox gVisor loop reconciliation requires an absolute workspace root")
	}
	entries, err := os.ReadDir(sysfsBlockRoot)
	if err != nil {
		return nil, fmt.Errorf("scan block devices: %w", err)
	}
	prefix := filepath.Clean(workspaceRoot) + string(filepath.Separator)
	var candidates []StaleLoopEvidence
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "loop") {
			continue
		}
		backingPath := filepath.Join(sysfsBlockRoot, entry.Name(), "loop", "backing_file")
		content, err := os.ReadFile(backingPath)
		if err != nil {
			continue // Not a bound loop device.
		}
		backing := strings.TrimSpace(string(content))
		backing = strings.TrimSuffix(backing, " (deleted)")
		if !strings.HasPrefix(backing, prefix) {
			continue
		}
		candidates = append(candidates, StaleLoopEvidence{
			Device:      filepath.Join(deviceRoot, entry.Name()),
			BackingFile: backing,
		})
	}
	return candidates, nil
}

func detachLoopDevice(devicePath string) error {
	device, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer device.Close()
	if err := unix.IoctlSetInt(int(device.Fd()), unix.LOOP_CLR_FD, 0); err != nil &&
		!errors.Is(err, unix.ENXIO) {
		return err
	}
	return nil
}

// reconcileStaleRuntimeDirectories removes per-Instance runtime state left by
// an earlier runner generation. The runtime directory is exclusively this
// runner's, and every attachment those directories described was already
// reconciled, so nothing under it can be live before compute launches.
func reconcileStaleRuntimeDirectories(runtimeDir string) error {
	entries, err := os.ReadDir(runtimeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read stale runtime directories: %w", err)
	}
	var joined error
	for _, entry := range entries {
		joined = errors.Join(joined, os.RemoveAll(filepath.Join(runtimeDir, entry.Name())))
	}
	if joined != nil {
		return fmt.Errorf("remove stale runtime directories: %w", joined)
	}
	return nil
}
