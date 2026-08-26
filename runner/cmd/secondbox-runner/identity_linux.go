//go:build linux

package main

import "fmt"

func validateRunnerExecutionIdentity(healthcheck bool, effectiveUID int) error {
	if !healthcheck && effectiveUID != 0 {
		return fmt.Errorf("SecondBox Linux runner must run as root to own local compute host resources")
	}
	return nil
}
