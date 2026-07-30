package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
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
	flags := flag.NewFlagSet("secondbox-scenario-stress", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	mode := flags.String("mode", "", "required mode: validate, prepare, or run")
	configPath := flags.String("config", "", "absolute stress configuration path")
	outputPath := flags.String("output", "", "absolute machine-readable result path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox stress driver does not accept positional arguments")
	}
	if *mode != "validate" && *mode != "prepare" && *mode != "run" {
		return errors.New("SecondBox stress driver --mode must be validate, prepare, or run")
	}
	config, err := readStressConfig(*configPath)
	if err != nil {
		return err
	}
	if *mode == "validate" {
		if strings.TrimSpace(*outputPath) != "" {
			return errors.New("SecondBox stress validate mode does not accept --output")
		}
		fmt.Printf(
			"Validated stress config: workloads=%d concurrency_levels=%d duration_seconds=%d\n",
			len(config.Workloads), len(config.ConcurrencyLevels), config.DurationSeconds,
		)
		return nil
	}
	inputs, err := readRuntimeInputs(*mode)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Timeout: time.Duration(config.RequestTimeoutMilliseconds) * time.Millisecond,
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		inputs.baseURL, inputs.platformToken, config.TenantRef, config.SubjectRef, httpClient,
	)
	if err != nil {
		return err
	}
	driver := &stressDriver{
		config: config, client: client,
		runtimeDigest: inputs.runtimeDigest, toolchainDigest: inputs.toolchainDigest,
		bootStages: make(map[string][]time.Duration),
	}
	switch *mode {
	case "prepare":
		if strings.TrimSpace(*outputPath) != "" {
			return errors.New("SecondBox stress prepare mode does not accept --output")
		}
		return driver.prepare(context.Background())
	case "run":
		if strings.TrimSpace(*outputPath) == "" {
			return errors.New("SecondBox stress run mode requires --output")
		}
		startedAt := time.Now().UTC()
		results, deploymentTiming, err := driver.run(context.Background())
		if err != nil {
			return err
		}
		markLatencyDegradation(results, config.LatencyDegradationRatio)
		bootStages, dominant := summarizeBootStages(driver.bootStages)
		report := stressReport{
			SchemaVersion: 1, StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			SourceCommit: inputs.sourceCommit, GoVersion: inputs.goVersion,
			ArtifactManifest: inputs.artifactManifest,
			Configuration: stressReportConfiguration{
				Workloads:               append([]string(nil), config.Workloads...),
				ConcurrencyLevels:       append([]int(nil), config.ConcurrencyLevels...),
				DurationSeconds:         config.DurationSeconds,
				TimingWindowSeconds:     config.TimingWindowSeconds,
				LatencyDegradationRatio: config.LatencyDegradationRatio,
				FileTransferBytes:       config.FileTransferBytes,
				StreamingOutputBytes:    config.StreamingOutputBytes,
			},
			ConfiguredBinding: config.configuredBinding(), Results: results,
			BootStages: bootStages, DominantBootStage: dominant,
			DeploymentTiming: &deploymentTiming,
		}
		if err := writeStressReport(*outputPath, report); err != nil {
			return err
		}
		if err := writeHumanReport(os.Stdout, report); err != nil {
			return fmt.Errorf("SecondBox stress human report failed: %w", err)
		}
		fmt.Printf("Machine-readable stress report: %s\n", *outputPath)
		if err := verifyStressResults(results, report.ConfiguredBinding); err != nil {
			return err
		}
		return nil
	}
	return errors.New("SecondBox stress mode dispatch failed")
}

func verifyStressResults(results []workloadResult, binding configuredLimit) error {
	for _, result := range results {
		if result.Failures > 0 {
			return fmt.Errorf(
				"SecondBox stress workload %s at concurrency %d recorded %d failures",
				result.Workload, result.Concurrency, result.Failures,
			)
		}
		if result.AdmissionRefusals > 0 && result.Concurrency < binding.Capacity {
			return fmt.Errorf(
				"SecondBox stress workload %s was refused below configured binding %s=%d",
				result.Workload, binding.Name, binding.Capacity,
			)
		}
	}
	return nil
}

func readRuntimeInputs(mode string) (runtimeInputs, error) {
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("SecondBox stress driver requires %s", name)
		}
		return value, nil
	}
	var inputs runtimeInputs
	var err error
	if inputs.baseURL, err = required("SECONDBOX_LIVE_BASE_URL"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.platformToken, err = required("SECONDBOX_LIVE_PLATFORM_TOKEN"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.runtimeDigest, err = required("SECONDBOX_STRESS_RUNTIME_BUNDLE_DIGEST"); err != nil {
		return runtimeInputs{}, err
	}
	if inputs.toolchainDigest, err = required("SECONDBOX_STRESS_TOOLCHAIN_BUNDLE_DIGEST"); err != nil {
		return runtimeInputs{}, err
	}
	if mode == "run" {
		if inputs.sourceCommit, err = required("SECONDBOX_STRESS_SOURCE_COMMIT"); err != nil {
			return runtimeInputs{}, err
		}
		if inputs.goVersion, err = required("SECONDBOX_STRESS_GO_VERSION"); err != nil {
			return runtimeInputs{}, err
		}
		if inputs.artifactManifest, err = required("SECONDBOX_STRESS_ARTIFACT_MANIFEST_DIGEST"); err != nil {
			return runtimeInputs{}, err
		}
	}
	return inputs, nil
}
