package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type transitionSamples struct {
	mu       sync.Mutex
	byName   map[string][]time.Duration
	refusals map[string]int64
	failures map[string]int64
}

func newTransitionSamples() *transitionSamples {
	return &transitionSamples{
		byName:   make(map[string][]time.Duration),
		refusals: make(map[string]int64),
		failures: make(map[string]int64),
	}
}

func (samples *transitionSamples) record(transition string, elapsed time.Duration) {
	samples.mu.Lock()
	defer samples.mu.Unlock()
	samples.byName[transition] = append(samples.byName[transition], elapsed)
}

func (samples *transitionSamples) recordError(err error) {
	refusal, code := classifyLifecycleError(err)
	samples.mu.Lock()
	defer samples.mu.Unlock()
	if refusal {
		samples.refusals[code]++
		return
	}
	samples.failures[code]++
}

type occupancySample struct {
	OffsetMilliseconds int64 `json:"offsetMilliseconds"`
	Ready              int64 `json:"ready"`
	InFlight           int64 `json:"inFlight"`
	Backlog            int64 `json:"backlog"`
}

// runCell executes one (cycle, pattern, resident population) combination.
//
// Arrivals are dispatched from a schedule computed before the window opens. When
// the system cannot keep up, in-flight cycles accumulate and the backlog series
// grows; the driver does not slow its offered rate in response. That is the
// property the previous closed-loop stress harness lacked.
func (driver *lifecycleDriver) runCell(
	ctx context.Context,
	cycle string,
	pattern arrivalPattern,
	resident int,
) (cellResult, error) {
	schedule, err := buildArrivalSchedule(pattern)
	if err != nil {
		return cellResult{}, err
	}
	fmt.Printf(
		"cycle=%s pattern=%s resident=%d offered=%d\n",
		cycle, pattern.Name, resident, schedule.offeredCount(),
	)

	residentHandles, err := driver.establishResident(ctx, cycle, pattern.Name, resident)
	if err != nil {
		driver.releaseResident(ctx, residentHandles, pattern.Name)
		return cellResult{}, err
	}
	defer driver.releaseResident(ctx, residentHandles, pattern.Name)

	// The warm cycle needs Sandboxes that already exist and are stopped, so the
	// measured edge is start-to-ready rather than create-to-ready. Provisioning
	// happens before the window opens and is not measured.
	// Warm arrivals check a Sandbox out of an exclusive pool for the duration of
	// their cycle. Sharing a handle between two concurrent arrivals would race
	// start against stop on the same Sandbox and produce meaningless timings.
	var warmPool []*secondboxclient.SandboxHandle
	var available chan *secondboxclient.SandboxHandle
	inFlightLimit := driver.config.MaximumInFlight
	if cycle == cycleWarm {
		warmPool, err = driver.provisionWarmPool(ctx, pattern.Name, schedule.offeredCount())
		if err != nil {
			driver.releaseWarmPool(ctx, warmPool, pattern.Name)
			return cellResult{}, err
		}
		defer driver.releaseWarmPool(ctx, warmPool, pattern.Name)
		available = make(chan *secondboxclient.SandboxHandle, len(warmPool))
		for _, handle := range warmPool {
			available <- handle
		}
		// Never offer more concurrent warm cycles than there are Sandboxes to
		// run them on, so a checkout always succeeds.
		if len(warmPool) < inFlightLimit {
			inFlightLimit = len(warmPool)
		}
	}

	samples := newTransitionSamples()
	var occupancy []occupancySample
	var occupancyMu sync.Mutex

	windowContext, cancelWindow := context.WithCancel(ctx)
	var sampler sync.WaitGroup
	sampler.Add(1)
	startedAt := time.Now()
	go func() {
		defer sampler.Done()
		ticker := time.NewTicker(
			time.Duration(driver.config.OccupancySampleMilliseconds) * time.Millisecond,
		)
		defer ticker.Stop()
		for {
			select {
			case <-windowContext.Done():
				return
			case now := <-ticker.C:
				occupancyMu.Lock()
				occupancy = append(occupancy, occupancySample{
					OffsetMilliseconds: now.Sub(startedAt).Milliseconds(),
					Ready:              driver.readyCount.Load(),
					InFlight:           driver.inFlight.Load(),
					Backlog:            driver.inFlight.Load(),
				})
				occupancyMu.Unlock()
			}
		}
	}()

	var cycles sync.WaitGroup
	completed := int64(0)
	var completedMu sync.Mutex
	for index, offset := range schedule.offsets {
		if wait := time.Until(startedAt.Add(offset)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				cancelWindow()
				sampler.Wait()
				return cellResult{}, ctx.Err()
			case <-timer.C:
			}
		}
		if driver.inFlight.Load() >= int64(inFlightLimit) {
			// Shedding is itself a saturation observation. It is counted
			// separately so it can never be mistaken for latency.
			driver.shedArrivals.Add(1)
			continue
		}
		var warm *secondboxclient.SandboxHandle
		if cycle == cycleWarm {
			select {
			case warm = <-available:
			default:
				driver.shedArrivals.Add(1)
				continue
			}
		}
		cycles.Add(1)
		driver.inFlight.Add(1)
		go func(arrival int, handle *secondboxclient.SandboxHandle) {
			defer cycles.Done()
			defer driver.inFlight.Add(-1)
			if handle != nil {
				defer func() { available <- handle }()
			}
			if driver.runOneCycle(ctx, cycle, pattern.Name, arrival, handle, samples) {
				completedMu.Lock()
				completed++
				completedMu.Unlock()
			}
		}(index, warm)
	}
	cycles.Wait()
	elapsed := time.Since(startedAt)
	cancelWindow()
	sampler.Wait()

	occupancyMu.Lock()
	collected := append([]occupancySample(nil), occupancy...)
	occupancyMu.Unlock()

	return buildCellResult(
		cycle, pattern, resident, schedule, samples, collected, completed, elapsed,
	), nil
}

// runOneCycle executes a single arrival and reports whether it completed every
// transition. Both edges are timed; a failure on either edge records the typed
// problem and abandons the remainder of that cycle.
func (driver *lifecycleDriver) runOneCycle(
	ctx context.Context,
	cycle string,
	patternName string,
	arrival int,
	warm *secondboxclient.SandboxHandle,
	samples *transitionSamples,
) bool {
	key := fmt.Sprintf("lifecycle-%s-%s-%d", cycle, patternName, arrival)
	cycleContext, cancel := driver.operationContext(ctx)
	defer cancel()

	if cycle == cycleWarm {
		if warm == nil {
			samples.recordError(fmt.Errorf("SecondBox lifecycle warm pool exhausted for %s", key))
			return false
		}
		started, err := driver.startSandbox(cycleContext, warm, key+"-start")
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(transitionStartReady, started)
		stopped, err := driver.stopSandbox(cycleContext, warm, key+"-stop")
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(transitionStopStopped, stopped)
		return true
	}

	handle, created, err := driver.createSandbox(cycleContext, key+"-create")
	if err != nil {
		samples.recordError(err)
		if handle != nil {
			if _, deleteErr := driver.deleteSandbox(
				cycleContext, handle, key+"-failed-delete", false,
			); deleteErr != nil {
				samples.recordError(deleteErr)
			}
		}
		return false
	}
	samples.record(transitionCreateReady, created)
	deleted, err := driver.deleteSandbox(cycleContext, handle, key+"-delete", true)
	if err != nil {
		samples.recordError(err)
		return false
	}
	samples.record(transitionDeleteGone, deleted)
	return true
}

// establishResident creates long-lived Sandboxes that stay ready for the whole
// cell, so arrival latency can be measured against a realistic standing
// population rather than an idle deployment.
func (driver *lifecycleDriver) establishResident(
	ctx context.Context,
	cycle string,
	patternName string,
	resident int,
) ([]*secondboxclient.SandboxHandle, error) {
	handles := make([]*secondboxclient.SandboxHandle, 0, resident)
	for index := 0; index < resident; index++ {
		key := fmt.Sprintf("lifecycle-resident-%s-%s-%d", cycle, patternName, index)
		setupContext, cancel := driver.operationContext(ctx)
		handle, _, err := driver.createSandbox(setupContext, key)
		cancel()
		if err != nil {
			if handle != nil {
				handles = append(handles, handle)
			}
			return handles, fmt.Errorf(
				"SecondBox lifecycle could not establish resident population of %d: %w", resident, err,
			)
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func (driver *lifecycleDriver) releaseResident(
	ctx context.Context,
	handles []*secondboxclient.SandboxHandle,
	patternName string,
) {
	for index, handle := range handles {
		cleanupContext, cancel := driver.operationContext(ctx)
		if _, err := driver.deleteSandbox(
			cleanupContext, handle, fmt.Sprintf("lifecycle-resident-%s-%d-cleanup", patternName, index), true,
		); err != nil {
			fmt.Printf("resident cleanup failed for %s index %d: %v\n", patternName, index, err)
		}
		cancel()
	}
}

// provisionWarmPool creates Sandboxes and stops them so that each measured
// arrival exercises start-to-ready against a retained Workspace.
func (driver *lifecycleDriver) provisionWarmPool(
	ctx context.Context,
	patternName string,
	arrivals int,
) ([]*secondboxclient.SandboxHandle, error) {
	size := arrivals
	if size > driver.config.MaximumInFlight {
		size = driver.config.MaximumInFlight
	}
	if size < 1 {
		size = 1
	}
	handles := make([]*secondboxclient.SandboxHandle, 0, size)
	for index := 0; index < size; index++ {
		key := fmt.Sprintf("lifecycle-warm-%s-%d", patternName, index)
		setupContext, cancel := driver.operationContext(ctx)
		handle, _, err := driver.createSandbox(setupContext, key)
		if err != nil {
			cancel()
			if handle != nil {
				handles = append(handles, handle)
			}
			return handles, fmt.Errorf("SecondBox lifecycle warm pool provisioning failed: %w", err)
		}
		if _, err := driver.stopSandbox(setupContext, handle, key+"-initial-stop"); err != nil {
			cancel()
			handles = append(handles, handle)
			return handles, fmt.Errorf("SecondBox lifecycle warm pool initial stop failed: %w", err)
		}
		cancel()
		handles = append(handles, handle)
	}
	return handles, nil
}

func (driver *lifecycleDriver) releaseWarmPool(
	ctx context.Context,
	handles []*secondboxclient.SandboxHandle,
	patternName string,
) {
	for index, handle := range handles {
		cleanupContext, cancel := driver.operationContext(ctx)
		if _, err := driver.deleteSandbox(
			cleanupContext, handle, fmt.Sprintf("lifecycle-warm-%s-%d-cleanup", patternName, index), false,
		); err != nil {
			fmt.Printf("warm pool cleanup failed for %s index %d: %v\n", patternName, index, err)
		}
		cancel()
	}
}

func buildCellResult(
	cycle string,
	pattern arrivalPattern,
	resident int,
	schedule arrivalSchedule,
	samples *transitionSamples,
	occupancy []occupancySample,
	completed int64,
	elapsed time.Duration,
) cellResult {
	samples.mu.Lock()
	defer samples.mu.Unlock()
	transitions := make(map[string]transitionSummary, len(samples.byName))
	for name, durations := range samples.byName {
		sorted := append([]time.Duration(nil), durations...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		transitions[name] = transitionSummary{
			Samples:             len(sorted),
			P50Milliseconds:     percentile(sorted, 0.50).Milliseconds(),
			P95Milliseconds:     percentile(sorted, 0.95).Milliseconds(),
			P99Milliseconds:     percentile(sorted, 0.99).Milliseconds(),
			MaximumMilliseconds: percentile(sorted, 1.0).Milliseconds(),
		}
	}
	refusals := make(map[string]int64, len(samples.refusals))
	for code, count := range samples.refusals {
		refusals[code] = count
	}
	failures := make(map[string]int64, len(samples.failures))
	for code, count := range samples.failures {
		failures[code] = count
	}
	offeredWindow := schedule.window()
	if offeredWindow <= 0 {
		offeredWindow = elapsed
	}
	var offeredRate, completedRate float64
	if offeredWindow > 0 {
		offeredRate = float64(schedule.offeredCount()) / offeredWindow.Seconds()
	}
	if elapsed > 0 {
		completedRate = float64(completed) / elapsed.Seconds()
	}
	peakBacklog := int64(0)
	for _, sample := range occupancy {
		if sample.Backlog > peakBacklog {
			peakBacklog = sample.Backlog
		}
	}
	return cellResult{
		Cycle: cycle, Pattern: pattern.Name, PatternKind: pattern.Kind,
		ResidentPopulation:     resident,
		OfferedArrivals:        schedule.offeredCount(),
		CompletedCycles:        completed,
		OfferedRatePerSecond:   offeredRate,
		CompletedRatePerSecond: completedRate,
		ElapsedMilliseconds:    elapsed.Milliseconds(),
		PeakBacklog:            peakBacklog,
		Transitions:            transitions,
		Refusals:               refusals,
		Failures:               failures,
		Occupancy:              occupancy,
	}
}
