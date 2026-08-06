package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
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

// failureTotal counts genuine failures only. Refusals are excluded: a deployment
// declining to admit an arrival is the measurement, not a fault, and a rail that
// counted refusals would abort exactly when the run got interesting.
func (samples *transitionSamples) failureTotal() int64 {
	samples.mu.Lock()
	defer samples.mu.Unlock()
	total := int64(0)
	for _, count := range samples.failures {
		total += count
	}
	return total
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
	OffsetMilliseconds   int64    `json:"offsetMilliseconds"`
	Ready                int64    `json:"ready"`
	OutstandingArrivals  int64    `json:"outstandingArrivals"`
	OfferedRatePerSecond *float64 `json:"offeredRatePerSecond"`
}

type cellResources struct {
	mu      sync.Mutex
	handles []*secondboxclient.SandboxHandle
}

func (resources *cellResources) add(handle *secondboxclient.SandboxHandle) {
	if handle == nil {
		return
	}
	resources.mu.Lock()
	defer resources.mu.Unlock()
	resources.handles = append(resources.handles, handle)
}

func (resources *cellResources) snapshot() []*secondboxclient.SandboxHandle {
	resources.mu.Lock()
	defer resources.mu.Unlock()
	return append([]*secondboxclient.SandboxHandle(nil), resources.handles...)
}

// runCell executes one (measurement, pattern, resident population) combination.
//
// Arrivals are dispatched from a schedule computed before the window opens. When
// the system cannot keep up, outstanding arrivals accumulate; the driver does
// not slow its offered rate in response. That is the
// property the previous closed-loop stress harness lacked.
func (driver *lifecycleDriver) runCell(
	ctx context.Context,
	measurement string,
	pattern arrivalPattern,
	resident int,
) (result cellResult, returnErr error) {
	schedule, err := buildArrivalSchedule(pattern)
	if err != nil {
		return cellResult{}, err
	}
	fmt.Printf(
		"measurement=%s pattern=%s resident=%d offered=%d\n",
		measurement, pattern.Name, resident, schedule.offeredCount(),
	)

	residentHandles, err := driver.establishResident(
		ctx, measurement, pattern.Name, resident,
	)
	if err != nil {
		cleanupErr := driver.releaseResident(
			context.WithoutCancel(ctx), residentHandles, pattern.Name,
		)
		return cellResult{}, errors.Join(err, cleanupErr)
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			driver.releaseResident(
				context.WithoutCancel(ctx), residentHandles, pattern.Name,
			),
		)
	}()

	resources := &cellResources{}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			driver.releaseCellResources(
				context.WithoutCancel(ctx), resources, measurement, pattern.Name,
			),
		)
	}()
	available, err := driver.prepareMeasurementPool(
		ctx, measurement, pattern.Name, schedule.offeredCount(), resources,
	)
	if err != nil {
		return cellResult{}, err
	}

	samples := newTransitionSamples()
	timings := newStartupTimingSamples()
	var occupancy []occupancySample
	var occupancyMu sync.Mutex

	windowContext, cancelWindow := context.WithCancel(ctx)
	var sampler sync.WaitGroup
	sampler.Add(1)
	startedAt := time.Now()
	// Rails stop the run; they never cancel ctx. The arrival goroutines parent
	// their operations on ctx, and classifyLifecycleError reads context.Canceled
	// as a client error, so cancelling would manufacture the very failures the
	// distress knee is meant to detect.
	var railTripped atomic.Value
	memoryLowWater := int64(-1)
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
				offset := now.Sub(startedAt)
				occupancyMu.Lock()
				occupancy = append(occupancy, occupancySample{
					OffsetMilliseconds:   offset.Milliseconds(),
					Ready:                driver.readyCount.Load(),
					OutstandingArrivals:  driver.inFlight.Load(),
					OfferedRatePerSecond: offeredRateAt(pattern, offset),
				})
				occupancyMu.Unlock()
				memory, err := readHostMemory()
				if err != nil {
					continue
				}
				occupancyMu.Lock()
				if memoryLowWater < 0 || memory.AvailableMiB < memoryLowWater {
					memoryLowWater = memory.AvailableMiB
				}
				occupancyMu.Unlock()
				if rail := driver.config.rails().evaluate(railObservation{
					availableMiB: memory.AvailableMiB,
					failures:     samples.failureTotal(),
					elapsed:      offset,
				}); rail != "" {
					railTripped.CompareAndSwap(nil, rail)
				}
			}
		}
	}()

	var arrivals sync.WaitGroup
	completed := int64(0)
	var completedMu sync.Mutex
	var peakOutstanding atomic.Int64
	// Shedding is attributed to the cell as well as the run. A run-global total
	// cannot say which step shed, so it cannot distinguish saturation of the
	// deployment from saturation of maximumInFlight.
	var shed atomic.Int64

	// finish drains the cell and builds its result. Both the normal exit and the
	// cancelled exit go through it so a cell that stopped early still reports the
	// arrivals it did measure.
	finish := func() cellResult {
		arrivals.Wait()
		elapsed := time.Since(startedAt)
		cancelWindow()
		sampler.Wait()
		occupancyMu.Lock()
		collected := append([]occupancySample(nil), occupancy...)
		lowWater := memoryLowWater
		occupancyMu.Unlock()
		rail, _ := railTripped.Load().(string)
		return buildCellResult(cellObservation{
			measurement: measurement, pattern: pattern, resident: resident,
			schedule: schedule, samples: samples, timings: timings,
			occupancy: collected, completed: completed, shed: shed.Load(),
			peakOutstanding: peakOutstanding.Load(), elapsed: elapsed,
			memoryLowWaterMiB: lowWater, abortedAtRail: rail,
		})
	}

	for index, offset := range schedule.offsets {
		// A burst schedule sets every offset to zero, so this loop dispatches in
		// microseconds against a sampler ticking in hundreds of milliseconds.
		// The check cannot shrink the cell it fires in — rails act between cells.
		// It is here for schedules that do span time.
		if rail, tripped := railTripped.Load().(string); tripped && rail != "" {
			return finish(), fmt.Errorf(
				"SecondBox lifecycle host rail %s stopped the run", rail,
			)
		}
		if wait := time.Until(startedAt.Add(offset)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return finish(), ctx.Err()
			case <-timer.C:
			}
		}
		if driver.inFlight.Load() >= int64(driver.config.MaximumInFlight) {
			// Shedding is itself a saturation observation. It is counted
			// separately so it can never be mistaken for latency.
			driver.shedArrivals.Add(1)
			shed.Add(1)
			continue
		}
		var handle *secondboxclient.SandboxHandle
		if available != nil {
			select {
			case handle = <-available:
			default:
				driver.shedArrivals.Add(1)
				shed.Add(1)
				continue
			}
		}
		arrivals.Add(1)
		recordPeak(&peakOutstanding, driver.inFlight.Add(1))
		go func(arrival int, handle *secondboxclient.SandboxHandle) {
			defer arrivals.Done()
			defer driver.inFlight.Add(-1)
			if driver.runOneMeasurement(
				ctx, measurement, pattern.Name, arrival, handle, resources, samples, timings,
			) {
				completedMu.Lock()
				completed++
				completedMu.Unlock()
			}
		}(index, handle)
	}
	result = finish()
	if result.AbortedAtRail != "" {
		return result, fmt.Errorf(
			"SecondBox lifecycle host rail %s stopped the run", result.AbortedAtRail,
		)
	}
	return result, nil
}

func recordPeak(peak *atomic.Int64, candidate int64) {
	for current := peak.Load(); candidate > current; current = peak.Load() {
		if peak.CompareAndSwap(current, candidate) {
			return
		}
	}
}

// runOneMeasurement executes exactly one measured transition. Its prerequisite
// state and cleanup belong to the cell setup/cleanup, never to arrival latency.
func (driver *lifecycleDriver) runOneMeasurement(
	ctx context.Context,
	measurement string,
	patternName string,
	arrival int,
	handle *secondboxclient.SandboxHandle,
	resources *cellResources,
	samples *transitionSamples,
	timings *startupTimingSamples,
) bool {
	key := fmt.Sprintf("lifecycle-%s-%s-%d", measurement, patternName, arrival)
	operationContext, cancel := driver.operationContext(ctx)
	defer cancel()

	switch measurement {
	case measurementCreateReady:
		createdHandle, elapsed, err := driver.createSandbox(
			operationContext, key+"-create", timings,
		)
		resources.add(createdHandle)
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(measurement, elapsed)
		return true
	case measurementStartReady:
		if handle == nil {
			samples.recordError(fmt.Errorf("SecondBox lifecycle start pool exhausted for %s", key))
			return false
		}
		elapsed, err := driver.startSandbox(operationContext, handle, key+"-start", timings)
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(measurement, elapsed)
		return true
	case measurementStopStopped:
		if handle == nil {
			samples.recordError(fmt.Errorf("SecondBox lifecycle stop pool exhausted for %s", key))
			return false
		}
		elapsed, err := driver.stopSandbox(operationContext, handle, key+"-stop", timings)
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(measurement, elapsed)
		return true
	case measurementDeleteGone:
		if handle == nil {
			samples.recordError(fmt.Errorf("SecondBox lifecycle delete pool exhausted for %s", key))
			return false
		}
		elapsed, err := driver.deleteSandbox(operationContext, handle, key+"-delete", timings)
		if err != nil {
			samples.recordError(err)
			return false
		}
		samples.record(measurement, elapsed)
		return true
	default:
		samples.recordError(fmt.Errorf(
			"SecondBox lifecycle unsupported measurement %q", measurement,
		))
		return false
	}
}

// establishResident creates long-lived Sandboxes that stay ready for the whole
// cell, so arrival latency can be measured against a realistic standing
// population rather than an idle deployment.
func (driver *lifecycleDriver) establishResident(
	ctx context.Context,
	measurement string,
	patternName string,
	resident int,
) ([]*secondboxclient.SandboxHandle, error) {
	handles := make([]*secondboxclient.SandboxHandle, 0, resident)
	for index := 0; index < resident; index++ {
		key := fmt.Sprintf("lifecycle-resident-%s-%s-%d", measurement, patternName, index)
		setupContext, cancel := driver.operationContext(ctx)
		handle, _, err := driver.createSandbox(setupContext, key, nil)
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
) error {
	var cleanupErrors []error
	for index, handle := range handles {
		cleanupContext, cancel := driver.operationContext(ctx)
		if _, err := driver.deleteSandbox(
			cleanupContext, handle,
			fmt.Sprintf("lifecycle-resident-%s-%d-cleanup", patternName, index), nil,
		); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"SecondBox lifecycle resident cleanup failed for %s index %d: %w",
				patternName, index, err,
			))
		}
		cancel()
	}
	return errors.Join(cleanupErrors...)
}

// prepareMeasurementPool establishes the exact prerequisite state before the
// arrival window. Each non-create arrival owns one Sandbox and never reuses it
// inside the measured cell.
func (driver *lifecycleDriver) prepareMeasurementPool(
	ctx context.Context,
	measurement string,
	patternName string,
	arrivals int,
	resources *cellResources,
) (chan *secondboxclient.SandboxHandle, error) {
	if measurement == measurementCreateReady {
		return nil, nil
	}
	available := make(chan *secondboxclient.SandboxHandle, arrivals)
	for index := 0; index < arrivals; index++ {
		key := fmt.Sprintf("lifecycle-%s-pool-%s-%d", measurement, patternName, index)
		setupContext, cancel := driver.operationContext(ctx)
		handle, _, err := driver.createSandbox(setupContext, key+"-create", nil)
		resources.add(handle)
		if err != nil {
			cancel()
			return nil, fmt.Errorf(
				"SecondBox lifecycle %s pool provisioning failed: %w", measurement, err,
			)
		}
		if measurement == measurementStartReady {
			if _, err := driver.stopSandbox(
				setupContext, handle, key+"-initial-stop", nil,
			); err != nil {
				cancel()
				return nil, fmt.Errorf(
					"SecondBox lifecycle start pool initial stop failed: %w", err,
				)
			}
		}
		cancel()
		available <- handle
	}
	return available, nil
}

func (driver *lifecycleDriver) releaseCellResources(
	ctx context.Context,
	resources *cellResources,
	measurement string,
	patternName string,
) error {
	var cleanupErrors []error
	for index, handle := range resources.snapshot() {
		cleanupContext, cancel := driver.operationContext(ctx)
		if _, err := driver.deleteSandbox(
			cleanupContext,
			handle,
			fmt.Sprintf("lifecycle-%s-%s-%d-cleanup", measurement, patternName, index),
			nil,
		); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"SecondBox lifecycle measurement cleanup failed for %s/%s index %d: %w",
				measurement, patternName, index, err,
			))
		}
		cancel()
	}
	return errors.Join(cleanupErrors...)
}

// cellObservation is everything one cell measured. It is a struct rather than a
// parameter list because the list had already reached ten positional arguments,
// where a transposed pair is a silent defect rather than a compile error.
type cellObservation struct {
	measurement     string
	pattern         arrivalPattern
	resident        int
	schedule        arrivalSchedule
	samples         *transitionSamples
	timings         *startupTimingSamples
	occupancy         []occupancySample
	completed         int64
	shed              int64
	peakOutstanding   int64
	elapsed           time.Duration
	memoryLowWaterMiB int64
	abortedAtRail     string
}

func buildCellResult(observation cellObservation) cellResult {
	measurement := observation.measurement
	pattern := observation.pattern
	schedule := observation.schedule
	samples := observation.samples
	timings := observation.timings
	completed := observation.completed
	elapsed := observation.elapsed
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
	var completedRate float64
	if elapsed > 0 {
		completedRate = float64(completed) / elapsed.Seconds()
	}
	timings.mu.Lock()
	stages := make(map[string][]time.Duration, len(timings.bootStages))
	for stage, durations := range timings.bootStages {
		stages[stage] = append([]time.Duration(nil), durations...)
	}
	spans := make(map[string][]time.Duration, len(timings.startupSpans))
	for span, durations := range timings.startupSpans {
		spans[span] = append([]time.Duration(nil), durations...)
	}
	timings.mu.Unlock()
	bootStages, dominantBootStage := summarizeBootStages(stages)
	var latency *transitionSummary
	if summary, exists := transitions[measurement]; exists {
		latency = &summary
	}
	return cellResult{
		Measurement:               measurement,
		Pattern:                   pattern.Name,
		PatternKind:               pattern.Kind,
		ResidentPopulation:        observation.resident,
		OfferedArrivals:           schedule.offeredCount(),
		CompletedArrivals:         completed,
		ShedArrivals:              observation.shed,
		OfferedRatePerSecond:      schedule.offeredRatePerSecond(),
		CompletionRatePerSecond:   completedRate,
		ArrivalWindowMilliseconds: schedule.rateWindow.Milliseconds(),
		ElapsedMilliseconds:       elapsed.Milliseconds(),
		PeakOutstandingArrivals:   observation.peakOutstanding,
		Latency:                   latency,
		BootStages:                bootStages,
		DominantBootStage:         dominantBootStage,
		StartupSpans:              summarizeStartupSpans(spans),
		Refusals:                  refusals,
		Failures:                  failures,
		MemAvailableLowWaterMiB:   observation.memoryLowWaterMiB,
		AbortedAtRail:             observation.abortedAtRail,
		Occupancy:                 observation.occupancy,
	}
}
