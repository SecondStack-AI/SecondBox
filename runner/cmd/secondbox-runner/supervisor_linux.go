//go:build linux

package main

import (
	"github.com/SecondStack-AI/SecondBox/runner/internal/gvisor"
	"github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"
)

func runPlatformSupervisorInvocation(arguments []string) (bool, error) {
	if len(arguments) > 0 && arguments[0] == gvisor.MountSupervisorInvocation {
		return true, gvisor.RunMountSupervisor(arguments[1:])
	}
	return jailersupervisor.RunInvocation(arguments)
}
