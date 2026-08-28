//go:build linux

package gvisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type flatRootTarget struct {
	path      string
	directory bool
}

var requiredFlatRootTargets = []flatRootTarget{
	{path: strings.TrimPrefix(guestAgentPath, "/")},
	{path: strings.TrimPrefix(guestWorkspacePath, "/"), directory: true},
	{path: strings.TrimPrefix(guestSocketDirectory, "/"), directory: true},
	{path: strings.TrimPrefix(guestRuntimePrivatePath, "/"), directory: true},
	{path: "etc/resolv.conf"},
	{path: "proc", directory: true},
	{path: "tmp", directory: true},
}

// PrepareFlatRoot materializes the immutable OCI mount-destination contract
// before an operator calculates and pins its flat-root digest. It creates only
// absent directories and placeholder files and never replaces existing nodes.
func PrepareFlatRoot(root string) error {
	if err := inspectFlatRoot(root); err != nil {
		return err
	}
	for _, target := range requiredFlatRootTargets {
		if err := inspectFlatRootTarget(root, target, false); err != nil {
			return err
		}
	}
	for _, target := range requiredFlatRootTargets {
		if err := materializeFlatRootTarget(root, target); err != nil {
			return err
		}
	}
	return ValidateFlatRoot(root)
}

// ValidateFlatRoot checks the prepared contract without modifying the pinned
// flat root. Runner readiness calls this before verifying its digest.
func ValidateFlatRoot(root string) error {
	if err := inspectFlatRoot(root); err != nil {
		return err
	}
	for _, target := range requiredFlatRootTargets {
		if err := inspectFlatRootTarget(root, target, true); err != nil {
			return err
		}
	}
	return nil
}

func inspectFlatRoot(root string) error {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("SecondBox gVisor flat root path must be clean and absolute")
	}
	if root == string(filepath.Separator) {
		return fmt.Errorf("SecondBox gVisor flat root path must not be the filesystem root")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("SecondBox gVisor flat root inspect: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("SecondBox gVisor flat root must not be a symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("SecondBox gVisor flat root must be a directory")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("SecondBox gVisor flat root resolve: %w", err)
	}
	if resolved != root {
		return fmt.Errorf("SecondBox gVisor flat root path must not contain symlink ancestors")
	}
	return nil
}

func inspectFlatRootTarget(root string, target flatRootTarget, require bool) error {
	components := strings.Split(filepath.ToSlash(target.path), "/")
	current := root
	for index, component := range components {
		current = filepath.Join(current, component)
		leaf := index == len(components)-1
		wantDirectory := !leaf || target.directory
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if require {
				return fmt.Errorf("SecondBox gVisor flat root requires %s", "/"+filepath.ToSlash(target.path))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("SecondBox gVisor flat root inspect %s: %w", "/"+filepath.ToSlash(target.path), err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SecondBox gVisor flat root path %s must not contain symlinks", "/"+filepath.ToSlash(target.path))
		}
		if wantDirectory && !info.IsDir() {
			return fmt.Errorf("SecondBox gVisor flat root path %s must be a directory", "/"+filepath.ToSlash(target.path))
		}
		if !wantDirectory && !info.Mode().IsRegular() {
			return fmt.Errorf("SecondBox gVisor flat root path %s must be a regular file", "/"+filepath.ToSlash(target.path))
		}
	}
	return nil
}

func materializeFlatRootTarget(root string, target flatRootTarget) error {
	components := strings.Split(filepath.ToSlash(target.path), "/")
	current := root
	for index, component := range components {
		current = filepath.Join(current, component)
		leaf := index == len(components)-1
		wantDirectory := !leaf || target.directory
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if wantDirectory {
				err = os.Mkdir(current, 0o755)
			} else {
				var file *os.File
				file, err = os.OpenFile(current, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err == nil {
					err = file.Close()
				}
			}
			if err != nil {
				return fmt.Errorf("SecondBox gVisor flat root create %s: %w", "/"+filepath.ToSlash(target.path), err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("SecondBox gVisor flat root inspect %s: %w", "/"+filepath.ToSlash(target.path), err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SecondBox gVisor flat root path %s must not contain symlinks", "/"+filepath.ToSlash(target.path))
		}
		if wantDirectory && !info.IsDir() {
			return fmt.Errorf("SecondBox gVisor flat root path %s must be a directory", "/"+filepath.ToSlash(target.path))
		}
		if !wantDirectory && !info.Mode().IsRegular() {
			return fmt.Errorf("SecondBox gVisor flat root path %s must be a regular file", "/"+filepath.ToSlash(target.path))
		}
	}
	return nil
}
