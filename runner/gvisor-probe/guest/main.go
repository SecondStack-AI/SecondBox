// Command secondbox-gvisor-probe-guest is the static container process the
// gVisor probe boots inside each sandbox. It writes a marker through a bind
// mount so the host side can observe boot, then behaves per mode.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const failExitCode = 7

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) >= 2 && os.Args[1] == "netcheck" {
		return netcheck(os.Args[2:])
	}
	if len(os.Args) != 3 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: guest <hello|fail|stay|spin|hog|fill|print|cat|netcheck> <argument>")
		return 2
	}
	mode := os.Args[1]
	argument := os.Args[2]
	// print and cat are exec-operation targets; they use stdio rather than a
	// marker mount and must not touch the filesystem.
	switch mode {
	case "print":
		fmt.Println(argument)
		return 0
	case "cat":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "guest cat failed: %v\n", err)
			return 1
		}
		return 0
	}
	markerPath := argument
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
	case "fill":
		return fill(markerPath)
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

// netcheck performs the network-policy probes. Each argument has the form
// "label|get|http://host:port/" or "label|dial|host:port"; the outcome for
// each label is written to stdout as "label=ok:<first-body-token>" or
// "label=blocked". A blocked probe is any resolution, connection, or request
// failure within the bounded timeout.
func netcheck(checks []string) int {
	if len(checks) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "guest netcheck requires at least one check")
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, check := range checks {
		parts := strings.SplitN(check, "|", 3)
		if len(parts) != 3 {
			_, _ = fmt.Fprintf(os.Stderr, "guest netcheck check %q is malformed\n", check)
			return 2
		}
		label, kind, target := parts[0], parts[1], parts[2]
		outcome := "blocked"
		switch kind {
		case "get":
			response, err := client.Get(target)
			if err == nil {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 256))
				_ = response.Body.Close()
				if readErr == nil && response.StatusCode == http.StatusOK {
					fields := strings.Fields(string(body))
					token := ""
					if len(fields) > 0 {
						token = fields[0]
					}
					outcome = "ok:" + token
				}
			}
		case "dial":
			connection, err := net.DialTimeout("tcp", target, 3*time.Second)
			if err == nil {
				_ = connection.Close()
				outcome = "ok:"
			}
		default:
			_, _ = fmt.Fprintf(os.Stderr, "guest netcheck kind %q is unknown\n", kind)
			return 2
		}
		fmt.Printf("%s=%s\n", label, outcome)
	}
	return 0
}

// fill writes to the workspace until the filesystem refuses, then records the
// outcome through the marker mount, which lives on a different filesystem so
// the record itself cannot hit the exhausted capacity.
func fill(markerPath string) int {
	const chunkBytes = 1 << 20
	chunk := make([]byte, chunkBytes)
	for i := range chunk {
		chunk[i] = 0xA5
	}
	output, err := os.OpenFile("/workspace/fill.dat", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "guest fill open failed: %v\n", err)
		return 1
	}
	written := 0
	var writeErr error
	for {
		n, err := output.Write(chunk)
		written += n
		if err != nil {
			writeErr = err
			break
		}
		if err := output.Sync(); err != nil {
			writeErr = err
			break
		}
	}
	_ = output.Close()
	outcome := "unexpected"
	if errors.Is(writeErr, syscall.ENOSPC) {
		outcome = "enospc"
	}
	record := fmt.Sprintf("outcome=%s bytes=%d error=%v\n", outcome, written, writeErr)
	if err := os.WriteFile(markerPath, []byte(record), 0o600); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "guest fill record failed: %v\n", err)
		return 1
	}
	if outcome != "enospc" {
		return 1
	}
	return 0
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
