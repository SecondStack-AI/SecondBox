//go:build darwin

package workspacestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"golang.org/x/sys/unix"
)

type darwinDriver struct {
	helperExecutable  string
	setUUIDExecutable string
}

const darwinDescriptorPathBufferBytes = 4096

const (
	darwinSparseBlockBytes = 4096
	darwinSparseScanBytes  = 1 << 20
)

type darwinPunchHole struct {
	Flags    uint32
	Reserved uint32
	Offset   int64
	Length   int64
}

func newPlatformDriver(formatterKind FormatterKind, helperExecutable string) (platformDriver, error) {
	if formatterKind != FormatterMicrosandboxHelper {
		return nil, fmt.Errorf(
			"SecondBox Darwin WorkspaceStore requires formatter kind %q",
			FormatterMicrosandboxHelper,
		)
	}
	if !filepath.IsAbs(helperExecutable) || filepath.Clean(helperExecutable) != helperExecutable {
		return nil, fmt.Errorf("SecondBox WorkspaceStore Microsandbox helper executable must be a clean absolute path")
	}
	info, err := os.Stat(helperExecutable)
	if err != nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore inspect Microsandbox helper executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("SecondBox WorkspaceStore Microsandbox helper is not executable")
	}
	for _, dependency := range []string{"mke2fs", "e2fsck"} {
		if _, err := exec.LookPath(dependency); err != nil {
			return nil, fmt.Errorf(
				"SecondBox WorkspaceStore %s is required by the Microsandbox helper: %w",
				dependency,
				err,
			)
		}
	}
	setUUIDExecutable, err := exec.LookPath("tune2fs")
	if err != nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore tune2fs is required: %w", err)
	}
	return darwinDriver{
		helperExecutable:  helperExecutable,
		setUUIDExecutable: setUUIDExecutable,
	}, nil
}

func (darwinDriver) Clone(destination *os.File, source *os.File) error {
	if destination == nil || source == nil ||
		!filepath.IsAbs(destination.Name()) || filepath.Clean(destination.Name()) != destination.Name() {
		return fmt.Errorf("%w: SecondBox WorkspaceStore APFS clone descriptors are invalid", ErrStorageIncompatible)
	}
	destinationPath := destination.Name()
	if err := os.Remove(destinationPath); err != nil {
		return fmt.Errorf("%w: remove APFS clone placeholder: %v", ErrStorageIncompatible, err)
	}
	if err := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, destinationPath, 0); err != nil {
		return fmt.Errorf("%w: APFS clonefile failed: %v", ErrStorageIncompatible, err)
	}
	// clonefile preserves the source mode. Templates and Snapshots are
	// intentionally read-only, but the newly cloned child must be writable so
	// WorkspaceStore can apply its UUID fence before setting the requested final
	// mode. The caller performs that final chmod after Clone returns.
	if err := os.Chmod(destinationPath, writableImageMode); err != nil {
		return fmt.Errorf("%w: make APFS clone writable: %v", ErrStorageIncompatible, err)
	}
	clone, err := os.OpenFile(destinationPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: reopen APFS clone: %v", ErrStorageIncompatible, err)
	}
	defer clone.Close()
	if err := unix.Dup2(int(clone.Fd()), int(destination.Fd())); err != nil {
		return fmt.Errorf("%w: adopt APFS clone descriptor: %v", ErrStorageIncompatible, err)
	}
	return nil
}

func (driver darwinDriver) Format(
	ctx context.Context,
	workspace *os.File,
	capacity int64,
	uuid string,
) error {
	decoded, err := decodeUUID(uuid)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore decode helper format UUID: %w", err)
	}
	sockets, err := darwinSocketpair()
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore create helper control socket: %w", err)
	}
	parent := os.NewFile(uintptr(sockets[0]), "microsandbox-helper-parent")
	child := os.NewFile(uintptr(sockets[1]), "microsandbox-helper-child")
	defer parent.Close()
	defer child.Close()
	command := exec.CommandContext(ctx, driver.helperExecutable, "format-workspace")
	command.ExtraFiles = []*os.File{child, workspace}
	command.Stdin = nil
	command.Stdout = nil
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore start Microsandbox helper formatter: %w", err)
	}
	_ = child.Close()
	request := &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version,
		RequestId:       1,
		Message: &microsandboxprotocol.Envelope_FormatWorkspace{FormatWorkspace: &microsandboxprotocol.FormatWorkspaceRequest{
			LogicalCapacityBytes: uint64(capacity),
			Label:                workspaceFilesystemLabel,
			WorkspaceUuid:        decoded,
		}},
	}
	writeErr := microsandboxprotocol.WriteFrame(parent, request)
	response, readErr := microsandboxprotocol.ReadFrame(parent)
	waitErr := command.Wait()
	if writeErr != nil || readErr != nil || waitErr != nil {
		return fmt.Errorf(
			"SecondBox WorkspaceStore Microsandbox helper format failed: %w: %s",
			errors.Join(writeErr, readErr, waitErr),
			strings.TrimSpace(stderr.String()),
		)
	}
	terminal := response.GetTerminal()
	if response.ProtocolVersion != microsandboxprotocol.Version || response.RequestId != 1 ||
		terminal == nil || !terminal.Success {
		return fmt.Errorf("SecondBox WorkspaceStore Microsandbox helper rejected format request")
	}
	if err := compactDarwinSparseFile(workspace, capacity); err != nil {
		return err
	}
	return nil
}

// compactDarwinSparseFile restores sparse allocation after the Homebrew
// e2fsprogs formatter has written zero-filled ext4 regions to APFS. Punching an
// all-zero, filesystem-block-aligned extent preserves the exact image bytes.
func compactDarwinSparseFile(file *os.File, capacity int64) error {
	if file == nil || capacity < minimumExt4Bytes || capacity%darwinSparseBlockBytes != 0 {
		return fmt.Errorf("%w: SecondBox WorkspaceStore APFS sparse compaction input is invalid", ErrStorageIncompatible)
	}
	buffer := make([]byte, darwinSparseScanBytes)
	zeroStart := int64(-1)
	flushZeros := func(end int64) error {
		if zeroStart < 0 {
			return nil
		}
		hole := darwinPunchHole{Offset: zeroStart, Length: end - zeroStart}
		_, _, errno := unix.Syscall(
			unix.SYS_FCNTL,
			file.Fd(),
			uintptr(unix.F_PUNCHHOLE),
			uintptr(unsafe.Pointer(&hole)),
		)
		zeroStart = -1
		if errno != 0 {
			return fmt.Errorf("%w: SecondBox WorkspaceStore APFS punch hole: %v", ErrStorageIncompatible, errno)
		}
		return nil
	}
	for offset := int64(0); offset < capacity; {
		length := int64(len(buffer))
		if remaining := capacity - offset; remaining < length {
			length = remaining
		}
		chunk := buffer[:length]
		if _, err := file.ReadAt(chunk, offset); err != nil {
			return fmt.Errorf("SecondBox WorkspaceStore scan formatted APFS image: %w", err)
		}
		for blockOffset := int64(0); blockOffset < length; blockOffset += darwinSparseBlockBytes {
			blockEnd := blockOffset + darwinSparseBlockBytes
			absoluteOffset := offset + blockOffset
			if allZero(chunk[blockOffset:blockEnd]) {
				if zeroStart < 0 {
					zeroStart = absoluteOffset
				}
				continue
			}
			if err := flushZeros(absoluteOffset); err != nil {
				return err
			}
		}
		offset += length
	}
	if err := flushZeros(capacity); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync compacted APFS image: %w", err)
	}
	return nil
}

func (driver darwinDriver) SetUUID(ctx context.Context, workspace *os.File, uuid string) error {
	command := exec.CommandContext(ctx, driver.setUUIDExecutable, "-U", uuid, "/dev/fd/3")
	command.ExtraFiles = []*os.File{workspace}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"SecondBox WorkspaceStore ext4 UUID rewrite failed: %w: %s",
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return nil
}

func (darwinDriver) OpenAttachment(path string) (*os.File, error) {
	return platformOpenAttachment(path)
}

func platformOpenAttachment(path string) (*os.File, error) {
	source, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	descriptor, err := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, 10)
	closeErr := source.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		_ = unix.Close(descriptor)
		return nil, closeErr
	}
	return os.NewFile(uintptr(descriptor), "workspace"), nil
}

func (darwinDriver) LinkDescriptor(file *os.File, destination string) error {
	return platformLinkDescriptor(file, destination)
}

func platformLinkDescriptor(file *os.File, destination string) error {
	if file == nil || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return fmt.Errorf("SecondBox WorkspaceStore descriptor link target is invalid")
	}
	sourcePath, err := darwinDescriptorPath(file)
	if err != nil {
		return err
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore inspect descriptor path: %w", err)
	}
	descriptorInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore inspect attachment descriptor: %w", err)
	}
	if !os.SameFile(sourceInfo, descriptorInfo) {
		return fmt.Errorf("SecondBox WorkspaceStore descriptor path identity changed")
	}
	_ = os.Remove(destination)
	if err := os.Link(sourcePath, destination); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore link attachment descriptor: %w", err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil || !os.SameFile(destinationInfo, descriptorInfo) {
		_ = os.Remove(destination)
		return fmt.Errorf("SecondBox WorkspaceStore linked descriptor identity changed")
	}
	return nil
}

func darwinDescriptorPath(file *os.File) (string, error) {
	buffer := make([]byte, darwinDescriptorPathBufferBytes)
	_, _, errno := unix.Syscall(
		unix.SYS_FCNTL,
		file.Fd(),
		uintptr(unix.F_GETPATH),
		uintptr(unsafe.Pointer(&buffer[0])),
	)
	if errno != 0 {
		return "", fmt.Errorf("SecondBox WorkspaceStore resolve attachment descriptor: %w", errno)
	}
	end := bytes.IndexByte(buffer, 0)
	if end <= 0 {
		return "", fmt.Errorf("SecondBox WorkspaceStore attachment descriptor path is empty")
	}
	path := string(buffer[:end])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("SecondBox WorkspaceStore attachment descriptor path is invalid")
	}
	return path, nil
}

func darwinSocketpair() ([2]int, error) {
	descriptors, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return [2]int{}, err
	}
	for _, descriptor := range descriptors {
		if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			return [2]int{}, errors.Join(err, unix.Close(descriptors[0]), unix.Close(descriptors[1]))
		}
	}
	return descriptors, nil
}

func (darwinDriver) ResetSparse(file *os.File, capacity int64) error {
	if file == nil || capacity < minimumExt4Bytes {
		return ErrStorageIncompatible
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("%w: SecondBox WorkspaceStore sparse reset failed: %v", ErrStorageIncompatible, err)
	}
	if err := file.Truncate(capacity); err != nil {
		return fmt.Errorf("%w: SecondBox WorkspaceStore sparse resize failed: %v", ErrStorageIncompatible, err)
	}
	return nil
}

func (darwinDriver) CompactSparse(file *os.File, capacity int64) error {
	return compactDarwinSparseFile(file, capacity)
}

func (darwinDriver) TryLock(file *os.File) error     { return platformTryLock(file) }
func (darwinDriver) Unlock(file *os.File) error      { return platformUnlock(file) }
func (darwinDriver) SyncDirectory(path string) error { return platformSyncDirectory(path) }
func (darwinDriver) ChildDescriptorPath(descriptor int) string {
	return platformChildDescriptorPath(descriptor)
}

func platformTryLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func platformUnlock(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }

func platformSyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open directory for fsync: %w", err)
	}
	defer directory.Close()
	if err := unix.Fsync(int(directory.Fd())); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync directory: %w", err)
	}
	if _, err := unix.FcntlInt(directory.Fd(), unix.F_FULLFSYNC, 0); err != nil &&
		!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("SecondBox WorkspaceStore full-fsync directory: %w", err)
	}
	return nil
}

func platformChildDescriptorPath(descriptor int) string {
	if descriptor < 3 {
		return ""
	}
	return "/dev/fd/" + strconv.Itoa(descriptor)
}
