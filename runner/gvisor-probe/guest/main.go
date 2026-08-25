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
		_, _ = fmt.Fprintln(os.Stderr, "usage: guest <hello|fail|stay|spin|hog> <marker-path>")
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
	case "spin":
		spin()
		return 0
	case "hog":
		hog()
		return 0
	default:
		_, _ = fmt.Fprintf(os.Stderr, "guest mode unknown: %s\n", mode)
		return 2
	}
}

// spinSeconds and spinWorkers are fixed so the host can bound the expected
// cgroup CPU usage: with a one-CPU quota, four busy workers for three wall
// seconds must accumulate roughly three CPU-seconds, not twelve.
const (
	spinSeconds = 3
	spinWorkers = 4
)

func spin() {
	stop := time.Now().Add(spinSeconds * time.Second)
	done := make(chan struct{}, spinWorkers)
	for i := 0; i < spinWorkers; i++ {
		go func() {
			counter := 0
			for time.Now().Before(stop) {
				counter++
			}
			_ = counter
			done <- struct{}{}
		}()
	}
	for i := 0; i < spinWorkers; i++ {
		<-done
	}
}

// hog touches memory far past the configured limit; under enforcement the
// sandbox must be killed before completion, so finishing means failure.
func hog() {
	const chunkBytes = 32 << 20
	const totalBytes = 1 << 30
	var retained [][]byte
	for allocated := 0; allocated < totalBytes; allocated += chunkBytes {
		chunk := make([]byte, chunkBytes)
		for page := 0; page < len(chunk); page += 4096 {
			chunk[page] = 1
		}
		retained = append(retained, chunk)
	}
	_ = retained
}
