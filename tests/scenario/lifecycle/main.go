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
			"Validated lifecycle config: measurements=%v patterns=%d resident=%v\n",
			config.Measurements, len(config.Patterns), config.ResidentPopulations,
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
	results, runErr := driver.runCells(ctx)
	report := lifecycleReport{
		SchemaVersion: 2, StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		SourceCommit: inputs.sourceCommit, GoVersion: inputs.goVersion,
		ArtifactManifest: inputs.artifactManifest,
		ShedArrivals:     driver.shedArrivals.Load(),
		Results:          results,
	}
	if runErr != nil {
		report.IncompleteReason = runErr.Error()
	}
	if err := writeLifecycleReport(outputPath, report); err != nil {
		return errors.Join(runErr, err)
	}
	if err := writeHumanReport(os.Stdout, report); err != nil {
		return errors.Join(
			runErr, fmt.Errorf("SecondBox lifecycle human report failed: %w", err),
		)
	}
	fmt.Printf("\nMachine-readable lifecycle report: %s\n", outputPath)
	return runErr
}

// cellRunner offers one cell. The driver supplies runCell; tests supply a
// function over prepared results so the ordering and accumulation can be
// exercised without a deployment.
type cellRunner func(
	ctx context.Context,
	measurement string,
	pattern arrivalPattern,
	resident int,
) (cellResult, error)

func (driver *lifecycleDriver) runCells(ctx context.Context) ([]cellResult, error) {
	return collectCells(ctx, driver.config, driver.runCell)
}

// collectCells offers every configured cell and stops at the first one that
// fails. The cells already measured are returned alongside the error so the
// caller can report them: a run that ends at step five still holds four valid
// measurements, and the partial cell itself carries whatever it observed before
// stopping.
func collectCells(
	ctx context.Context,
	config lifecycleConfig,
	run cellRunner,
) ([]cellResult, error) {
	var results []cellResult
	for _, measurement := range config.Measurements {
		for _, pattern := range config.Patterns {
			for _, resident := range config.ResidentPopulations {
				result, err := run(ctx, measurement, pattern, resident)
				// A cell that never reached its measurement window reports an
				// empty Measurement. Recording it would add a zero-valued row
				// that no reader can distinguish from a cell of pure zeroes.
				if result.Measurement != "" {
					results = append(results, result)
				}
				if err != nil {
					return results, fmt.Errorf(
						"SecondBox lifecycle cell measurement=%s pattern=%s resident=%d failed: %w",
						measurement, pattern.Name, resident, err,
					)
				}
			}
		}
	}
	return results, nil
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
