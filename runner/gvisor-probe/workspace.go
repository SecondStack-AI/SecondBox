package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// proofWorkspace proves the host-attachment path the backend design depends
// on: an already-open raw-ext4 descriptor attached by loop device through
// /proc/self/fd, mounted by the host kernel in a probe-private mount
// namespace, served into the sandbox through the gofer at /workspace, and
// detached cleanly with the image identity intact. The mount work runs in a
// re-executed child holding an unshared mount namespace, mirroring the
// runner-private namespace the backend will own.
func proofWorkspace(env *probeEnv) error {
	if env.rootless {
		// Development-only escape: loop devices and ext4 mounts need root.
		// The qualification recipe never passes -rootless.
		emit(env.stdout, "workspace", "skipped", "reason=rootless_development_mode")
		return nil
	}
	base := filepath.Join(env.workDir, "workspace")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(self,
		"-runsc", env.runscPath,
		"-guest", env.guestPath,
		"-internal-workspace-work", base,
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		Unshareflags: syscall.CLONE_NEWNS,
	}
	command.Stdout = env.stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("workspace child: %w", err)
	}
	return nil
}

const (
	workspaceImageBytes = 256 << 20
	enospcImageBytes    = 64 << 20
	// A fixed UUID makes identity verification explicit; WorkspaceStore
	// derives real UUIDs deterministically per Workspace.
	workspaceUUID = "31e40cd4-5f5a-4b54-a06e-0123456789ab"
)

// runWorkspaceChild executes inside the unshared mount namespace.
func runWorkspaceChild(runscPath, guestPath, base string) error {
	// Mount propagation from the host is typically shared; making the view
	// recursively private keeps every probe mount invisible to the host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	env := &probeEnv{
		runscPath: runscPath,
		guestPath: guestPath,
		workDir:   base,
		stdout:    os.Stdout,
	}
	if err := subproofDescriptorMount(env, base); err != nil {
		return fmt.Errorf("descriptor-mount: %w", err)
	}
	if err := subproofENOSPC(env, base); err != nil {
		return fmt.Errorf("enospc: %w", err)
	}
	return nil
}

func subproofDescriptorMount(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "mount")
	if err != nil {
		return err
	}
	imagePath := filepath.Join(base, "mount", "workspace.img")
	image, err := createExt4Image(imagePath, workspaceImageBytes, workspaceUUID)
	if err != nil {
		return err
	}
	defer image.Close()

	identityBefore, err := imageIdentity(image)
	if err != nil {
		return err
	}
	if identityBefore.uuid != strings.ReplaceAll(workspaceUUID, "-", "") {
		return fmt.Errorf("formatted UUID %s does not match requested %s",
			identityBefore.uuid, workspaceUUID)
	}

	attachment, err := attachLoop(image)
	if err != nil {
		return err
	}
	mountPoint := filepath.Join(base, "mount", "mnt")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	if err := syscall.Mount(attachment, mountPoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", attachment, err)
	}

	// The sandbox writes its marker directly into the mounted Workspace.
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"hello", "/workspace/ws-marker"},
		Binds:     []bindMount{{Source: mountPoint, Destination: "/workspace", ReadOnly: false}},
	}); err != nil {
		return err
	}
	runCommand := env.runscRun(area, "run")
	defer reapArea(env, area, nil)
	if err := runCommand.Run(); err != nil {
		return fmt.Errorf("runsc run: %w", err)
	}

	if err := detachClean(mountPoint, attachment); err != nil {
		return err
	}

	identityAfter, err := imageIdentity(image)
	if err != nil {
		return err
	}
	if identityAfter != identityBefore {
		return fmt.Errorf("image identity changed: before=%+v after=%+v",
			identityBefore, identityAfter)
	}
	reopened, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("reopen image: %w", err)
	}
	defer reopened.Close()
	reopenedIdentity, err := imageIdentity(reopened)
	if err != nil {
		return err
	}
	if reopenedIdentity != identityBefore {
		return fmt.Errorf("reopened image identity changed: %+v vs %+v",
			reopenedIdentity, identityBefore)
	}

	// Re-attach and re-mount read-only to prove the marker is durable in the
	// image rather than in page cache or a shadow copy.
	reattached, err := attachLoop(image)
	if err != nil {
		return err
	}
	if err := syscall.Mount(reattached, mountPoint, "ext4", syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("read-only remount: %w", err)
	}
	marker, err := os.ReadFile(filepath.Join(mountPoint, "ws-marker"))
	if err != nil {
		return fmt.Errorf("marker missing after detach: %w", err)
	}
	if err := detachClean(mountPoint, reattached); err != nil {
		return err
	}
	if !strings.Contains(string(marker), "hello") {
		return fmt.Errorf("marker content unexpected: %q", marker)
	}

	emit(env.stdout, "workspace-descriptor-mount", "passed",
		"image_bytes="+strconv.Itoa(workspaceImageBytes),
		"uuid="+identityAfter.uuid,
		"inode_stable=true",
		"marker=durable")
	return nil
}

func subproofENOSPC(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "enospc")
	if err != nil {
		return err
	}
	imagePath := filepath.Join(base, "enospc", "workspace.img")
	image, err := createExt4Image(imagePath, enospcImageBytes, workspaceUUID)
	if err != nil {
		return err
	}
	defer image.Close()

	attachment, err := attachLoop(image)
	if err != nil {
		return err
	}
	mountPoint := filepath.Join(base, "enospc", "mnt")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	if err := syscall.Mount(attachment, mountPoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", attachment, err)
	}

	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"fill", guestMarkerPath("fill-record")},
		Binds: []bindMount{
			{Source: mountPoint, Destination: "/workspace", ReadOnly: false},
			{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false},
		},
	}); err != nil {
		return err
	}
	runCommand := env.runscRun(area, "run")
	defer reapArea(env, area, nil)
	if err := runCommand.Run(); err != nil {
		return fmt.Errorf("fill sandbox failed instead of observing ENOSPC: %w", err)
	}
	record, err := os.ReadFile(filepath.Join(area.markerDir, "fill-record"))
	if err != nil {
		return fmt.Errorf("fill record missing: %w", err)
	}
	if !strings.Contains(string(record), "outcome=enospc") {
		return fmt.Errorf("fill record unexpected: %q", record)
	}

	if err := detachClean(mountPoint, attachment); err != nil {
		return err
	}
	check := exec.Command("e2fsck", "-fn", imagePath)
	output, checkErr := check.CombinedOutput()
	if checkErr != nil {
		return fmt.Errorf("e2fsck after ENOSPC: %v: %s", checkErr, bytes.TrimSpace(output))
	}
	emit(env.stdout, "workspace-enospc", "passed",
		"image_bytes="+strconv.Itoa(enospcImageBytes),
		"guest_outcome=enospc",
		"e2fsck=clean")
	return nil
}

type identity struct {
	device uint64
	inode  uint64
	uuid   string
}

// imageIdentity reads the device, inode, and on-disk ext4 UUID through the
// held descriptor, matching how the backend must verify attachment identity.
func imageIdentity(image *os.File) (identity, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(image.Fd()), &stat); err != nil {
		return identity{}, fmt.Errorf("fstat image: %w", err)
	}
	uuid, err := readExt4UUID(image)
	if err != nil {
		return identity{}, err
	}
	return identity{device: stat.Dev, inode: stat.Ino, uuid: uuid}, nil
}

// ext4 superblock starts at byte 1024; the filesystem UUID lives at offset
// 0x68 within it.
const ext4UUIDOffset = 1024 + 0x68

func readExt4UUID(image *os.File) (string, error) {
	uuid := make([]byte, 16)
	if _, err := image.ReadAt(uuid, ext4UUIDOffset); err != nil {
		return "", fmt.Errorf("read ext4 UUID: %w", err)
	}
	return fmt.Sprintf("%x", uuid), nil
}

func createExt4Image(path string, sizeBytes int64, uuid string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	image, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := image.Truncate(sizeBytes); err != nil {
		_ = image.Close()
		return nil, fmt.Errorf("size image: %w", err)
	}
	format := exec.Command("mkfs.ext4", "-F", "-q", "-U", uuid, path)
	if output, err := format.CombinedOutput(); err != nil {
		_ = image.Close()
		return nil, fmt.Errorf("mkfs.ext4: %v: %s", err, bytes.TrimSpace(output))
	}
	return image, nil
}

// attachLoop creates a loop device from the held descriptor through the
// /proc/self/fd path of a child, never through the image's host pathname.
func attachLoop(image *os.File) (string, error) {
	command := exec.Command("losetup", "--find", "--show", "/proc/self/fd/3")
	command.ExtraFiles = []*os.File{image}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("losetup attach: %w", err)
	}
	device := strings.TrimSpace(string(output))
	if !strings.HasPrefix(device, "/dev/loop") {
		return "", fmt.Errorf("losetup returned %q", device)
	}
	return device, nil
}

// detachClean is the strict release order the backend must keep: flush the
// mounted filesystem, unmount, then release the loop device. The image
// descriptor itself stays open and is closed by the caller last.
func detachClean(mountPoint, loopDevice string) error {
	mountFd, err := syscall.Open(mountPoint, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open mount point: %w", err)
	}
	syncErr := unix.Syncfs(mountFd)
	_ = syscall.Close(mountFd)
	if syncErr != nil {
		return fmt.Errorf("syncfs: %w", syncErr)
	}
	if err := syscall.Unmount(mountPoint, 0); err != nil {
		return fmt.Errorf("umount: %w", err)
	}
	if output, err := exec.Command("losetup", "--detach", loopDevice).CombinedOutput(); err != nil {
		return fmt.Errorf("losetup detach: %v: %s", err, bytes.TrimSpace(output))
	}
	return nil
}
