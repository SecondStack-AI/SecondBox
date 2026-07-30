package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

type runtimeInputs struct {
	baseURL          string
	platformToken    string
	runtimeDigest    string
	toolchainDigest  string
	sourceCommit     string
	goVersion        string
	artifactManifest string
}

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runMain(arguments []string) error {
	flags := flag.NewFlagSet("secondbox-scenario-lifecycle", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	mode := flags.String("mode", "", "required mode: validate, prepare, or run")
	configPath := flags.String("config", "", "absolute lifecycle configuration path")
	outputPath := flags.String("output", "", "absolute machine-readable result path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox lifecycle driver does not accept positional arguments")
	}
	if *mode != "validate" && *mode != "prepare" && *mode != "run" {
		return errors.New("SecondBox lifecycle driver --mode must be validate, prepare, or run")
	}
	config, err := readLifecycleConfig(*configPath)
	if err != nil {
		return err
	}
	if *mode == "validate" {
		for _, pattern := range config.Patterns {
			schedule, scheduleErr := buildArrivalSchedule(pattern)
			if scheduleErr != nil {
				return scheduleErr
			}
			fmt.Printf(
				"Validated pattern %s (%s): offered=%d window=%s\n",
				pattern.Name, pattern.Kind, schedule.offeredCount(), schedule.window(),
			)
		}
		fmt.Printf(
			"Validated lifecycle config: cycles=%v patterns=%d resident=%v\n",
			config.Cycles, len(config.Patterns), config.ResidentPopulations,
		)
		return nil
	}
	inputs, err := readRuntimeInputs(*mode)
	if err != nil {
		return err
	}
	clients, err := scenarioharness.NewClients(
		inputs.baseURL,
		inputs.platformToken,
		config.TenantRef,
		config.SubjectRef,
		time.Duration(config.RequestTimeoutMilliseconds)*time.Millisecond,
	)
	if err != nil {
		return err
	}
	driver := &lifecycleDriver{
		config: config, client: clients.Subject,
		runtimeDigest: inputs.runtimeDigest, toolchainDigest: inputs.toolchainDigest,
		bootStages: make(map[string][]time.Duration),
	}
	switch *mode {
	case "prepare":
		if strings.TrimSpace(*outputPath) != "" {
			return errors.New("SecondBox lifecycle prepare mode does not accept --output")
		}
		return driver.prepare(context.Background())
	case "run":
		if strings.TrimSpace(*outputPath) == "" {
			return errors.New("SecondBox lifecycle run mode requires --output")
		}
		return driver.runBenchmark(context.Background(), inputs, *outputPath)
	}
	return errors.New("SecondBox lifecycle mode dispatch failed")
}

func (driver *lifecycleDriver) runBenchmark(
	ctx context.Context,
	inputs runtimeInputs,
	outputPath string,
) error {
	if err := driver.waitForRunner(ctx); err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	var results []cellResult
	for _, cycle := range driver.config.Cycles {
		for _, pattern := range driver.config.Patterns {
			for _, resident := range driver.config.ResidentPopulations {
				result, err := driver.runCell(ctx, cycle, pattern, resident)
				if err != nil {
					return fmt.Errorf(
						"SecondBox lifecycle cell cycle=%s pattern=%s resident=%d failed: %w",
						cycle, pattern.Name, resident, err,
					)
				}
				results = append(results, result)
			}
		}
	}
	driver.mu.Lock()
	stages := make(map[string][]time.Duration, len(driver.bootStages))
	for stage, durations := range driver.bootStages {
		stages[stage] = append([]time.Duration(nil), durations...)
	}
	driver.mu.Unlock()
	bootStages, dominant := summarizeBootStages(stages)
	report := lifecycleReport{
		SchemaVersion: 1, StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		SourceCommit: inputs.sourceCommit, GoVersion: inputs.goVersion,
		ArtifactManifest: inputs.artifactManifest,
		ShedArrivals:     driver.shedArrivals.Load(),
		Results:          results,
		BootStages:       bootStages, DominantBootStage: dominant,
	}
	if err := writeLifecycleReport(outputPath, report); err != nil {
		return err
	}
	if err := writeHumanReport(os.Stdout, report); err != nil {
		return fmt.Errorf("SecondBox lifecycle human report failed: %w", err)
	}
	fmt.Printf("\nMachine-readable lifecycle report: %s\n", outputPath)
	return nil
}

func readRuntimeInputs(mode string) (runtimeInputs, error) {
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("SecondBox lifecycle driver requires %s", name)
		}
		return value, nil
	}
	var inputs runtimeInputs
	var err error
	if inputs.baseURL, err = required("SECONDBOX_LIVE_BASE_URL"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.platformToken, err = required("SECONDBOX_PLATFORM_TOKEN"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.runtimeDigest, err = required("SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.toolchainDigest, err = required("SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST"); err != nil {
		return runtimeInputs{}, err
	}
	if mode == "run" {
		if inputs.sourceCommit, err = required("SECONDBOX_SCENARIO_SOURCE_COMMIT"); err != nil {
			return runtimeInputs{}, err
		}
		if inputs.goVersion, err = required("SECONDBOX_SCENARIO_GO_VERSION"); err != nil {
			return runtimeInputs{}, err
		}
		if inputs.artifactManifest, err = required("SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST"); err != nil {
			return runtimeInputs{}, err
		}
	}
	return inputs, nil
}
