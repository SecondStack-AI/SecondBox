//go:build linux

package microsandbox

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformSocketpair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func configureHelperProcess(command *exec.Cmd) {
	// SIGTERM, which the helper ignores, instead of SIGKILL: parent loss must
	// reach the helper as its lifecycle-pipe EOF so the coordinated
	// stop-the-VMM-then-flush path can run instead of dying mid-write. The
	// inherited writer lock keeps the Workspace fenced until that path exits.
	command.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
		Setpgid:   true,
	}
}
