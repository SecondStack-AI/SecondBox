//go:build linux

package microsandbox

import (
	"os/exec"
	"syscall"
)

func configureHelperProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
