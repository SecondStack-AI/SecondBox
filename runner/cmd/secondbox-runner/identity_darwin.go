//go:build darwin

package main

import "fmt"

func validateRunnerExecutionIdentity(healthcheck bool, effectiveUID int) error {
	if !healthcheck && effectiveUID == 0 {
		return fmt.Errorf("SecondBox Darwin runner must use a dedicated unprivileged identity")
	}
	return nil
}
