//go:build linux

// secondbox-prepare-gvisor-flat-root creates and validates the mount targets
// required by SecondBox before a gVisor flat root receives its digest identity.
package main

import (
	"fmt"
	"os"

	"github.com/SecondStack-AI/SecondBox/runner/internal/gvisor"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: secondbox-prepare-gvisor-flat-root CLEAN_ABSOLUTE_FLAT_ROOT")
		os.Exit(2)
	}
	if err := gvisor.PrepareFlatRoot(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
