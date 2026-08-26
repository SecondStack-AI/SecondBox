//go:build linux

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

	"github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"golang.org/x/sys/unix"
)

type linuxDriver struct {
	formatterKind     FormatterKind
	formatExecutable  string
	helperExecutable  string
	setUUIDExecutable string
}

func newPlatformDriver(formatterKind FormatterKind, helperExecutable string) (platformDriver, error) {
	return newLinuxDriver(formatterKind, helperExecutable)
}

func newLinuxDriver(formatterKind FormatterKind, helperExecutable string) (platformDriver, error) {
	setUUIDExecutable, err := exec.LookPath("tune2fs")
	if err != nil {
		return nil, fmt.Errorf("SecondBox WorkspaceStore tune2fs is required: %w", err)
	}
	driver := linuxDriver{formatterKind: formatterKind, setUUIDExecutable: setUUIDExecutable}
	switch formatterKind {
	case FormatterMke2fs:
		driver.formatExecutable, err = exec.LookPath("mke2fs")
		if err != nil {
			return nil, fmt.Errorf("SecondBox WorkspaceStore mke2fs is required: %w", err)
		}
	case FormatterMicrosandboxHelper:
		if !filepath.IsAbs(helperExecutable) || filepath.Clean(helperExecutable) != helperExecutable {
			return nil, fmt.Errorf("SecondBox WorkspaceStore Microsandbox helper executable must be a clean absolute path")
		}
		info, statErr := os.Stat(helperExecutable)
		if statErr != nil {
			return nil, fmt.Errorf("SecondBox WorkspaceStore inspect Microsandbox helper executable: %w", statErr)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("SecondBox WorkspaceStore Microsandbox helper is not executable")
		}
		for _, dependency := range []string{"mke2fs", "e2fsck"} {
			if _, dependencyErr := exec.LookPath(dependency); dependencyErr != nil {
				return nil, fmt.Errorf("SecondBox WorkspaceStore %s is required by the Microsandbox helper: %w", dependency, dependencyErr)
			}
		}
		driver.helperExecutable = helperExecutable
	default:
		return nil, fmt.Errorf("SecondBox WorkspaceStore formatter kind %q is invalid", formatterKind)
	}
	return driver, nil
}

func (linuxDriver) Clone(destination *os.File, source *os.File) error {
	return unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
}

func (driver linuxDriver) Format(ctx context.Context, workspace *os.File, capacity int64, uuid string) error {
	if driver.formatterKind == FormatterMke2fs {
		command := exec.CommandContext(
			ctx,
			driver.formatExecutable,
			"-t", "ext4",
			"-F",
			"-q",
			"-L", workspaceFilesystemLabel,
			"-U", uuid,
			"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard",
			"/proc/self/fd/3",
		)
		command.ExtraFiles = []*os.File{workspace}
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf(
				"SecondBox WorkspaceStore ext4 format failed: %w: %s",
				err,
				strings.TrimSpace(string(output)),
			)
		}
		return nil
	}
	decoded, err := decodeUUID(uuid)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore decode helper format UUID: %w", err)
	}
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
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
	return nil
}

func (driver linuxDriver) SetUUID(ctx context.Context, workspace *os.File, uuid string) error {
	command := exec.CommandContext(ctx, driver.setUUIDExecutable, "-U", uuid, "/proc/self/fd/3")
	command.ExtraFiles = []*os.File{workspace}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore ext4 UUID rewrite failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (linuxDriver) OpenAttachment(path string) (*os.File, error) {
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

func (linuxDriver) LinkDescriptor(file *os.File, destination string) error {
	return platformLinkDescriptor(file, destination)
}

func platformLinkDescriptor(file *os.File, destination string) error {
	if file == nil || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return fmt.Errorf("SecondBox WorkspaceStore descriptor link target is invalid")
	}
	_ = os.Remove(destination)
	if err := unix.Linkat(
		unix.AT_FDCWD,
		"/proc/self/fd/"+strconv.Itoa(int(file.Fd())),
		unix.AT_FDCWD,
		destination,
		unix.AT_SYMLINK_FOLLOW,
	); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore link attachment descriptor: %w", err)
	}
	return nil
}

func (linuxDriver) ResetSparse(file *os.File, capacity int64) error {
	if file == nil || capacity < legacyMinimumExt4Bytes {
		return ErrStorageIncompatible
	}
	if err := unix.Fallocate(
		int(file.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		0,
		capacity,
	); err != nil {
		return fmt.Errorf("%w: SecondBox WorkspaceStore sparse reset failed: %w", ErrStorageIncompatible, err)
	}
	return nil
}

func (linuxDriver) CompactSparse(*os.File, int64) error { return nil }

func (linuxDriver) TryLock(file *os.File) error     { return platformTryLock(file) }
func (linuxDriver) SyncDirectory(path string) error { return platformSyncDirectory(path) }
func (linuxDriver) ChildDescriptorPath(descriptor int) string {
	return platformChildDescriptorPath(descriptor)
}

func platformTryLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func platformSyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open directory for fsync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore fsync directory: %w", err)
	}
	return nil
}

func platformChildDescriptorPath(descriptor int) string {
	if descriptor < 3 {
		return ""
	}
	return "/proc/self/fd/" + strconv.Itoa(descriptor)
}
