//go:build linux

package main

import "github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"

func runPlatformSupervisorInvocation(arguments []string) (bool, error) {
	return jailersupervisor.RunInvocation(arguments)
}
