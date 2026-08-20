//go:build darwin

package microsandbox

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformSocketpair() ([2]int, error) {
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

func configureHelperProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
