//go:build linux

// Package gvisor implements the SecondBox gVisor compute backend: runsc
// sandboxes supervised as local child processes, with the raw-ext4 Workspace
// image attached through a loop device inside a supervisor-private mount
// namespace and served to the sandbox by the gofer.
package gvisor

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// MountSupervisorInvocation is the hidden argv verb the runner binary
// dispatches to the supervisor child before normal startup.
const MountSupervisorInvocation = "gvisor-mount-supervisor"

// Inherited descriptor positions in the supervisor child.
const (
	workspaceDescriptorIndex = 3
	controlDescriptorIndex   = 4
	statusDescriptorIndex    = 5
)

// Control bytes the parent writes; closing the pipe acts as forced kill.
const (
	controlTerminate = 't'
	controlKill      = 'k'
)

const (
	workspaceMountFlags   = unix.MS_NOSUID | unix.MS_NODEV
	statusLineByteBound   = 4096
	ext4SuperblockOffset  = 1024
	ext4UUIDWithinSB      = 0x68
	ext4UUIDReadOffset    = ext4SuperblockOffset + ext4UUIDWithinSB
	ext4MagicWithinSB     = 0x38
	ext4MagicLittleEndian = 0xef53
)

// MountSupervisorPlan is everything the parent-side backend passes to one
// supervisor child. The workspace image travels as an inherited descriptor,
// never as a path.
type MountSupervisorPlan struct {
	Mountpoint    string
	ExpectedUUID  string
	CapacityBytes int64
	// Hold keeps the attachment mounted with no compute; the runsc fields
	// are then unused. This is the attachment-suite test mode.
	Hold        bool
	RunscPath   string
	StateRoot   string
	BundleDir   string
	ContainerID string
	RunscGlobal []string
}

func (plan MountSupervisorPlan) arguments() []string {
	arguments := []string{
		MountSupervisorInvocation,
		"-mountpoint", plan.Mountpoint,
		"-expected-uuid", plan.ExpectedUUID,
		"-capacity-bytes", strconv.FormatInt(plan.CapacityBytes, 10),
	}
	if plan.Hold {
		return append(arguments, "-hold")
	}
	arguments = append(arguments,
		"-runsc", plan.RunscPath,
		"-state-root", plan.StateRoot,
		"-bundle", plan.BundleDir,
		"-container-id", plan.ContainerID,
	)
	for _, global := range plan.RunscGlobal {
		arguments = append(arguments, "-runsc-global", global)
	}
	return arguments
}

func (plan MountSupervisorPlan) validate() error {
	if plan.Mountpoint == "" || plan.ExpectedUUID == "" || plan.CapacityBytes <= 0 {
		return errors.New("SecondBox gVisor mount supervisor requires mountpoint, UUID, and capacity")
	}
	if plan.Hold {
		return nil
	}
	if plan.RunscPath == "" || plan.StateRoot == "" || plan.BundleDir == "" || plan.ContainerID == "" {
		return errors.New("SecondBox gVisor mount supervisor requires the complete runsc launch identity")
	}
	return nil
}

// SupervisorHandles is the parent's side of one running supervisor.
type SupervisorHandles struct {
	Command *exec.Cmd
	// Control terminates the sandbox: one byte commands, close for kill.
	Control *os.File
	// Status delivers bounded supervisor lines; read with ReadStatusLine.
	Status *bufio.Reader

	statusFile *os.File
}

// CloseParentSide releases the parent's pipe ends after the supervisor exits.
func (handles *SupervisorHandles) CloseParentSide() error {
	var joined error
	if handles.Control != nil {
		joined = errors.Join(joined, ignoreClosed(handles.Control.Close()))
	}
	if handles.statusFile != nil {
		joined = errors.Join(joined, ignoreClosed(handles.statusFile.Close()))
	}
	return joined
}

func ignoreClosed(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

// StartMountSupervisor launches the runner's own binary as the supervisor
// child in a fresh mount namespace, passing the already-open workspace image
// as an inherited descriptor. The parent keeps supervising: the child dies
// with the parent, and the whole runsc tree dies with the child.
func StartMountSupervisor(
	selfExecutable string,
	plan MountSupervisorPlan,
	workspaceImage *os.File,
	writerLock *os.File,
) (*SupervisorHandles, error) {
	if err := plan.validate(); err != nil {
		return nil, err
	}
	if workspaceImage == nil || writerLock == nil {
		return nil, errors.New("SecondBox gVisor mount supervisor requires the workspace and writer-lock descriptors")
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, controlRead.Close(), controlWrite.Close())
	}
	command := exec.Command(selfExecutable, plan.arguments()...)
	// The writer-lock duplicate shares the attachment's open-file
	// description, so the exclusive Workspace fence survives a runner crash
	// for as long as the supervisor or its sandbox is alive and possibly
	// writing: the kernel releases the flock only when the last inherited
	// descriptor closes. The supervisor never uses the descriptor; holding
	// it is the point.
	command.ExtraFiles = []*os.File{workspaceImage, controlRead, statusWrite, writerLock}
	command.SysProcAttr = &syscall.SysProcAttr{
		Unshareflags: syscall.CLONE_NEWNS,
		Pdeathsig:    syscall.SIGKILL,
		Setpgid:      true,
	}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start SecondBox gVisor mount supervisor: %w", err),
			controlRead.Close(), controlWrite.Close(), statusRead.Close(), statusWrite.Close(),
		)
	}
	// The child inherited its copies; release the parent's duplicates.
	_ = controlRead.Close()
	_ = statusWrite.Close()
	return &SupervisorHandles{
		Command:    command,
		Control:    controlWrite,
		Status:     bufio.NewReaderSize(statusRead, statusLineByteBound),
		statusFile: statusRead,
	}, nil
}

// SupervisorStatus is one parsed bounded status line.
type SupervisorStatus struct {
	Kind   string
	Fields map[string]string
}

// ReadStatusLine reads and parses the next bounded status line.
func (handles *SupervisorHandles) ReadStatusLine() (SupervisorStatus, error) {
	line, err := handles.Status.ReadString('\n')
	if err != nil {
		return SupervisorStatus{}, err
	}
	return parseSupervisorStatus(strings.TrimSpace(line))
}

func parseSupervisorStatus(line string) (SupervisorStatus, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || len(line) > statusLineByteBound {
		return SupervisorStatus{}, fmt.Errorf("SecondBox gVisor supervisor status line is invalid: %q", line)
	}
	status := SupervisorStatus{Kind: fields[0], Fields: map[string]string{}}
	for _, field := range fields[1:] {
		key, value, found := strings.Cut(field, "=")
		if !found || key == "" {
			return SupervisorStatus{}, fmt.Errorf("SecondBox gVisor supervisor status field is invalid: %q", field)
		}
		status.Fields[key] = value
	}
	return status, nil
}

func emitStatus(status *os.File, kind string, pairs ...string) {
	line := kind
	if len(pairs) > 0 {
		line += " " + strings.Join(pairs, " ")
	}
	_, _ = fmt.Fprintf(status, "%s\n", line)
}

// parseMountSupervisorPlan decodes the child argv back into a validated plan.
func parseMountSupervisorPlan(arguments []string) (MountSupervisorPlan, error) {
	flags := flag.NewFlagSet(MountSupervisorInvocation, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var plan MountSupervisorPlan
	flags.StringVar(&plan.Mountpoint, "mountpoint", "", "workspace mountpoint")
	flags.StringVar(&plan.ExpectedUUID, "expected-uuid", "", "expected ext4 UUID, 32 hex digits")
	flags.Int64Var(&plan.CapacityBytes, "capacity-bytes", 0, "declared image capacity")
	flags.BoolVar(&plan.Hold, "hold", false, "hold the attachment without compute")
	flags.StringVar(&plan.RunscPath, "runsc", "", "pinned runsc binary")
	flags.StringVar(&plan.StateRoot, "state-root", "", "runsc state root")
	flags.StringVar(&plan.BundleDir, "bundle", "", "OCI bundle directory")
	flags.StringVar(&plan.ContainerID, "container-id", "", "container ID")
	runscGlobal := multiFlag{}
	flags.Var(&runscGlobal, "runsc-global", "additional runsc global flag (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return MountSupervisorPlan{}, err
	}
	plan.RunscGlobal = runscGlobal
	if err := plan.validate(); err != nil {
		return MountSupervisorPlan{}, err
	}
	return plan, nil
}

// RunMountSupervisor is the child-side entry point, invoked by the runner
// binary's argv dispatch inside the freshly unshared mount namespace.
func RunMountSupervisor(arguments []string) error {
	plan, err := parseMountSupervisorPlan(arguments)
	if err != nil {
		return err
	}

	workspace := os.NewFile(workspaceDescriptorIndex, "secondbox-gvisor-workspace")
	control := os.NewFile(controlDescriptorIndex, "secondbox-gvisor-control")
	status := os.NewFile(statusDescriptorIndex, "secondbox-gvisor-status")
	if workspace == nil || control == nil || status == nil {
		return errors.New("SecondBox gVisor mount supervisor descriptors are missing")
	}

	if err := runSupervisedAttachment(plan, workspace, control, status); err != nil {
		emitStatus(status, "terminal", "outcome=supervisor-failure", "detail="+boundToken(err.Error()))
		return err
	}
	return nil
}

func runSupervisedAttachment(
	plan MountSupervisorPlan,
	workspace, control, status *os.File,
) error {
	// The namespace was unshared at exec; making the view recursively
	// private keeps every attachment mount invisible to the host table.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	if err := verifyImageIdentity(workspace, plan.ExpectedUUID, plan.CapacityBytes); err != nil {
		return err
	}
	loopDevice, loopFile, err := attachLoopDescriptor(workspace)
	if err != nil {
		return err
	}
	defer loopFile.Close()
	if err := os.MkdirAll(plan.Mountpoint, 0o700); err != nil {
		return fmt.Errorf("create mountpoint: %w", err)
	}
	if err := unix.Mount(loopDevice, plan.Mountpoint, "ext4", workspaceMountFlags, ""); err != nil {
		return fmt.Errorf("mount workspace image: %w", err)
	}
	// The image identity must be unchanged by mounting (journal replay
	// rewrites data, never the filesystem identity).
	if err := verifyImageIdentity(workspace, plan.ExpectedUUID, plan.CapacityBytes); err != nil {
		return errors.Join(err, detachMount(plan.Mountpoint, loopFile))
	}
	if err := probeWorkspaceReadWrite(plan.Mountpoint); err != nil {
		return errors.Join(err, detachMount(plan.Mountpoint, loopFile))
	}
	emitStatus(status, "ready",
		"loop_device="+loopDevice, "pid="+strconv.Itoa(os.Getpid()), "rw_probe=ok")

	var computeErr error
	if plan.Hold {
		computeErr = holdUntilControl(control)
	} else {
		computeErr = superviseRunsc(plan, control, status)
	}
	detachErr := detachMount(plan.Mountpoint, loopFile)
	if computeErr != nil || detachErr != nil {
		return errors.Join(computeErr, detachErr)
	}
	emitStatus(status, "detached", "loop_device="+loopDevice)
	return nil
}

// probeWorkspaceReadWrite proves the mounted Workspace accepts durable
// writes before readiness, then removes its probe file.
func probeWorkspaceReadWrite(mountpoint string) error {
	probePath := mountpoint + "/.secondbox-attachment-probe"
	probe, err := os.OpenFile(probePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("workspace read/write probe open: %w", err)
	}
	if _, err := probe.WriteString("secondbox-gvisor-attachment-probe\n"); err != nil {
		_ = probe.Close()
		return fmt.Errorf("workspace read/write probe write: %w", err)
	}
	if err := probe.Sync(); err != nil {
		_ = probe.Close()
		return fmt.Errorf("workspace read/write probe sync: %w", err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("workspace read/write probe close: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("workspace read/write probe remove: %w", err)
	}
	return nil
}

// detachMount is the strict release order: flush the mounted filesystem,
// unmount, then clear the loop device. The workspace descriptor itself stays
// open until the parent's ComputeAttachment closes after supervisor exit.
func detachMount(mountpoint string, loopFile *os.File) error {
	mountFd, err := unix.Open(mountpoint, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open mountpoint for flush: %w", err)
	}
	syncErr := unix.Syncfs(mountFd)
	_ = unix.Close(mountFd)
	if syncErr != nil {
		return fmt.Errorf("syncfs workspace: %w", syncErr)
	}
	if err := unix.Unmount(mountpoint, 0); err != nil {
		return fmt.Errorf("unmount workspace: %w", err)
	}
	if err := unix.IoctlSetInt(int(loopFile.Fd()), unix.LOOP_CLR_FD, 0); err != nil &&
		!errors.Is(err, unix.ENXIO) {
		return fmt.Errorf("clear loop device: %w", err)
	}
	return nil
}

func holdUntilControl(control *os.File) error {
	buffer := make([]byte, 1)
	for {
		read, err := control.Read(buffer)
		if err != nil {
			return nil // EOF or parent loss: tear down.
		}
		if read == 1 && (buffer[0] == controlTerminate || buffer[0] == controlKill) {
			return nil
		}
	}
}

func superviseRunsc(plan MountSupervisorPlan, control, status *os.File) error {
	arguments := append([]string{"--root", plan.StateRoot}, plan.RunscGlobal...)
	arguments = append(arguments, "run", "-bundle", plan.BundleDir, plan.ContainerID)
	command := exec.Command(plan.RunscPath, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start runsc: %w", err)
	}

	go func() {
		buffer := make([]byte, 1)
		for {
			read, err := control.Read(buffer)
			if err != nil {
				// Parent loss or close: force the sandbox down.
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				return
			}
			if read != 1 {
				continue
			}
			switch buffer[0] {
			case controlTerminate:
				_ = command.Process.Signal(syscall.SIGTERM)
			case controlKill:
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			}
		}
	}()

	waitErr := command.Wait()
	outcome, code := classifyRunscExit(command, waitErr)
	emitStatus(status, "compute-exit", "outcome="+outcome, "code="+strconv.Itoa(code))
	return nil
}

func classifyRunscExit(command *exec.Cmd, waitErr error) (string, int) {
	if waitErr == nil {
		return "exit", 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok && waitStatus.Signaled() {
			return "signal", int(waitStatus.Signal())
		}
		return "exit", exitErr.ExitCode()
	}
	return "wait-failure", -1
}

// verifyImageIdentity checks the declared capacity and deterministic ext4
// identity through the inherited descriptor.
func verifyImageIdentity(image *os.File, expectedUUID string, capacityBytes int64) error {
	info, err := image.Stat()
	if err != nil {
		return fmt.Errorf("stat workspace image: %w", err)
	}
	if info.Size() != capacityBytes {
		return fmt.Errorf("workspace image size %d differs from declared capacity %d",
			info.Size(), capacityBytes)
	}
	magic := make([]byte, 2)
	if _, err := image.ReadAt(magic, ext4SuperblockOffset+ext4MagicWithinSB); err != nil {
		return fmt.Errorf("read ext4 magic: %w", err)
	}
	if uint16(magic[0])|uint16(magic[1])<<8 != ext4MagicLittleEndian {
		return errors.New("workspace image is not an ext4 filesystem")
	}
	uuid := make([]byte, 16)
	if _, err := image.ReadAt(uuid, ext4UUIDReadOffset); err != nil {
		return fmt.Errorf("read ext4 UUID: %w", err)
	}
	actual := fmt.Sprintf("%x", uuid)
	expected := strings.ToLower(strings.ReplaceAll(expectedUUID, "-", ""))
	if actual != expected {
		return fmt.Errorf("workspace image UUID %s differs from declared identity %s", actual, expected)
	}
	return nil
}

// attachLoopDescriptor creates a loop device from the inherited descriptor
// with autoclear armed, so a crashed supervisor cannot leak the device once
// every holder is gone.
func attachLoopDescriptor(image *os.File) (string, *os.File, error) {
	controlDevice, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open loop control: %w", err)
	}
	defer controlDevice.Close()
	// LOOP_CTL_GET_FREE reports a free index without reserving it, so two
	// concurrent starts can race to the same device and the loser binds
	// EBUSY while other devices remain available. Losing that race is
	// ordinary, so re-request a fresh index a bounded number of times.
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		index, err := unix.IoctlRetInt(int(controlDevice.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", nil, fmt.Errorf("acquire free loop device: %w", err)
		}
		devicePath := fmt.Sprintf("/dev/loop%d", index)
		loopFile, err := os.OpenFile(devicePath, os.O_RDWR, 0)
		if err != nil {
			return "", nil, fmt.Errorf("open %s: %w", devicePath, err)
		}
		if err := unix.IoctlSetInt(int(loopFile.Fd()), unix.LOOP_SET_FD, int(image.Fd())); err != nil {
			_ = loopFile.Close()
			if errors.Is(err, unix.EBUSY) {
				lastErr = fmt.Errorf("bind workspace descriptor to %s: %w", devicePath, err)
				continue
			}
			return "", nil, fmt.Errorf("bind workspace descriptor to %s: %w", devicePath, err)
		}
		var info unix.LoopInfo64
		info.Flags = unix.LO_FLAGS_AUTOCLEAR
		if err := unix.IoctlLoopSetStatus64(int(loopFile.Fd()), &info); err != nil {
			_ = unix.IoctlSetInt(int(loopFile.Fd()), unix.LOOP_CLR_FD, 0)
			_ = loopFile.Close()
			return "", nil, fmt.Errorf("arm loop autoclear on %s: %w", devicePath, err)
		}
		return devicePath, loopFile, nil
	}
	return "", nil, fmt.Errorf("acquire free loop device: every attempt lost its reservation race: %w", lastErr)
}

func boundToken(value string) string {
	replaced := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '=':
			return '_'
		}
		return r
	}, value)
	if len(replaced) > 200 {
		return replaced[:200]
	}
	return replaced
}

type multiFlag []string

func (values *multiFlag) String() string { return strings.Join(*values, ",") }

func (values *multiFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func unixKill(pid int) error {
	return unix.Kill(pid, unix.SIGKILL)
}
