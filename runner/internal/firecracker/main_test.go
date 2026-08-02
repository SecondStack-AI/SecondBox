package firecracker

import (
	"fmt"
	"os"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"
)

func TestMain(m *testing.M) {
	if handled, err := jailersupervisor.RunInvocation(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
