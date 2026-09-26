//go:build linux

package executionimage

import (
	"errors"
	"fmt"
	"os"

	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"golang.org/x/sys/unix"
)

// CloneVerifiedRootfs reflinks the verified rootfs of a prepared image into an
// unnamed inode on the cache filesystem while the caller still holds the
// digest lock. The opened source must be the verified file and its path must
// still name that file after the clone, so no replacement slips between
// verification and staging. The clone has no name: it disappears with its last
// descriptor, and a crashed Runner leaves nothing to sweep.
func (manager *Manager) CloneVerifiedRootfs(prepared PreparedImage) (*os.File, error) {
	var verified runtimemanager.VerifiedExecutionImageArtifact
	for _, artifact := range prepared.Artifacts {
		if artifact.Label == "rootfs" {
			verified = artifact
		}
	}
	if verified.Path == "" {
		return nil, errors.New("SecondBox execution image rootfs identity is not recorded")
	}
	source, err := os.OpenFile(verified.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image rootfs open: %w", err)
	}
	defer source.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &opened); err != nil {
		return nil, fmt.Errorf("SecondBox execution image rootfs inspection: %w", err)
	}
	if uint64(opened.Dev) != verified.Device || opened.Ino != verified.Inode || opened.Size != verified.Size ||
		opened.Mtim.Nano() != verified.ModTimeUnixNano || opened.Ctim.Nano() != verified.ChangeUnixNano {
		return nil, errors.New("SecondBox execution image rootfs changed after verification")
	}
	clone, err := manager.unnamedCacheFile()
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlFileClone(int(clone.Fd()), int(source.Fd())); err != nil {
		return nil, errors.Join(fmt.Errorf("SecondBox execution image rootfs reflink: %w", err), clone.Close())
	}
	current, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(verified.Label, verified.Path)
	if err != nil || !sameVerifiedArtifactIdentity(verified, current) {
		return nil, errors.Join(errors.New("SecondBox execution image rootfs changed while it was staged"), err, clone.Close())
	}
	return clone, nil
}

// ProbeRootfsCloning proves at startup that the cache filesystem can reflink
// into an unnamed inode, so an incapable filesystem fails the Runner instead
// of the first selected-image start.
func (manager *Manager) ProbeRootfsCloning() error {
	source, err := manager.unnamedCacheFile()
	if err != nil {
		return err
	}
	defer source.Close()
	probe := []byte("secondbox-execution-image-reflink-probe\n")
	if _, err := source.Write(probe); err != nil {
		return fmt.Errorf("SecondBox execution image reflink probe write: %w", err)
	}
	clone, err := manager.unnamedCacheFile()
	if err != nil {
		return err
	}
	defer clone.Close()
	if err := unix.IoctlFileClone(int(clone.Fd()), int(source.Fd())); err != nil {
		return fmt.Errorf("SecondBox execution image cache root %q cannot reflink; use a Btrfs or XFS filesystem with reflink support: %w", manager.cacheRoot, err)
	}
	info, err := clone.Stat()
	if err != nil {
		return fmt.Errorf("SecondBox execution image reflink probe inspection: %w", err)
	}
	if info.Size() != int64(len(probe)) {
		return errors.New("SecondBox execution image reflink probe produced a different size")
	}
	return nil
}

func (manager *Manager) unnamedCacheFile() (*os.File, error) {
	descriptor, err := unix.Open(manager.cacheRoot, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image unnamed staging file in %q: %w", manager.cacheRoot, err)
	}
	return os.NewFile(uintptr(descriptor), "secondbox-execution-image-rootfs"), nil
}
