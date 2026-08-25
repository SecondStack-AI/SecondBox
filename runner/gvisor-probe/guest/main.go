// Command secondbox-gvisor-probe-guest is the static container process the
// gVisor probe boots inside each sandbox. It writes a marker through a bind
// mount so the host side can observe boot, then behaves per mode.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const failExitCode = 7

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 3 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: guest <hello|fail|stay> <marker-path>")
		return 2
	}
	mode := os.Args[1]
	markerPath := os.Args[2]
	if err := os.WriteFile(markerPath, []byte("secondbox-gvisor-probe-guest "+mode+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "guest marker write failed: %v\n", err)
		return 1
	}
	switch mode {
	case "hello":
		return 0
	case "fail":
		return failExitCode
	case "stay":
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-stop:
			return 0
		case <-time.After(10 * time.Minute):
			_, _ = fmt.Fprintln(os.Stderr, "guest stay mode expired without a stop signal")
			return 1
		}
	default:
		_, _ = fmt.Fprintf(os.Stderr, "guest mode unknown: %s\n", mode)
		return 2
	}
}
