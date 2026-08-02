package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Rail names, in the order they are evaluated. The order is fixed so a run that
// trips two rails on the same tick always reports the same one.
const (
	railAvailableMemory = "availableMemoryFloorMiB"
	railStepFailures    = "stepFailureCeiling"
	railWallClock       = "maximumWallClockSeconds"
)

// hostMemory is the host's memory as the kernel reports it, in MiB.
type hostMemory struct {
	AvailableMiB int64
}

// parseMeminfo reads MemAvailable from /proc/meminfo content. Parsing is split
// from reading so it can be tested without a live /proc. MemAvailable is the
// kernel's own estimate of what a new allocation can obtain, which is the
// question a rail is asking; MemFree is not, because it excludes reclaimable
// page cache.
//
// Note that tmpfs is counted as cache but cannot be reclaimed under pressure,
// only swapped, so a host with a large /tmp reports more headroom than it has.
func parseMeminfo(reader io.Reader) (hostMemory, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), ":")
		if !found || name != "MemAvailable" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return hostMemory{}, fmt.Errorf(
				"SecondBox lifecycle meminfo MemAvailable is not kB-denominated: %q",
				strings.TrimSpace(value),
			)
		}
		kilobytes, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return hostMemory{}, fmt.Errorf(
				"SecondBox lifecycle meminfo MemAvailable is not a number: %w", err,
			)
		}
		return hostMemory{AvailableMiB: kilobytes / 1024}, nil
	}
	if err := scanner.Err(); err != nil {
		return hostMemory{}, fmt.Errorf("SecondBox lifecycle meminfo read failed: %w", err)
	}
	return hostMemory{}, fmt.Errorf("SecondBox lifecycle meminfo declares no MemAvailable")
}

func readHostMemory() (hostMemory, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return hostMemory{}, fmt.Errorf("SecondBox lifecycle meminfo open failed: %w", err)
	}
	defer file.Close()
	return parseMeminfo(file)
}

// rails returns the configured rails. Validation requires the block, so a nil
// one only reaches here from a caller that built a config in memory; it reads as
// disabled rather than panicking mid-run.
func (config lifecycleConfig) rails() hostRailConfig {
	if config.HostRails == nil {
		return hostRailConfig{}
	}
	return *config.HostRails
}

// railObservation is what the rails are evaluated against.
type railObservation struct {
	availableMiB int64
	failures     int64
	elapsed      time.Duration
}

// evaluate returns the name of the first rail the observation trips, or an empty
// string. Rails are a safety trip, never feedback: tripping one ends the run, it
// never reduces the load being offered. Modulating offered load in response to
// observed strain would make the benchmark closed loop, and a closed-loop
// benchmark stops offering work exactly when the queue starts growing.
func (rails hostRailConfig) evaluate(observation railObservation) string {
	if !rails.Enabled {
		return ""
	}
	if observation.availableMiB < rails.AvailableMemoryFloorMiB {
		return railAvailableMemory
	}
	if observation.failures > rails.StepFailureCeiling {
		return railStepFailures
	}
	if observation.elapsed >= time.Duration(rails.MaximumWallClockSeconds)*time.Second {
		return railWallClock
	}
	return ""
}
