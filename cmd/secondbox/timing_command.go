package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func runTimingCommand(
	ctx context.Context,
	rawURL string,
	token string,
	tenantRef string,
	subjectRef string,
	command string,
	args []string,
	output io.Writer,
	httpClient *http.Client,
) error {
	if rawURL == "" {
		return errors.New("SecondBox timings requires --url" + sessionSourceHint)
	}
	if token == "" {
		return errors.New("SecondBox timings requires --token" + sessionSourceHint)
	}
	if tenantRef == "" || subjectRef == "" {
		return errors.New(
			"SecondBox timings requires --tenant-ref and --subject-ref" + sessionSourceHint,
		)
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		rawURL, token, tenantRef, subjectRef, httpClient,
	)
	if err != nil {
		return err
	}
	switch command {
	case "sandbox":
		return runSandboxTiming(ctx, client, args, output)
	case "operation":
		return runOperationTiming(ctx, client, args, output)
	case "summary":
		return runDeploymentTiming(ctx, client, args, output)
	default:
		return fmt.Errorf("SecondBox timings command is invalid: %s", command)
	}
}

func runSandboxTiming(
	ctx context.Context,
	client *secondboxclient.Client,
	args []string,
	output io.Writer,
) error {
	flags := flag.NewFlagSet("timings sandbox", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sandboxID := flags.String("sandbox-id", "", "Sandbox identifier")
	limit := flags.Int("limit", 0, "maximum recent Operations and Execs, each")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox timings sandbox options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return errors.New("SecondBox timings sandbox received unexpected arguments")
	}
	if *sandboxID == "" {
		return errors.New("SecondBox timings sandbox requires --sandbox-id")
	}
	if *limit < 1 || *limit > 200 {
		return errors.New("SecondBox timings sandbox --limit must be from 1 through 200")
	}
	query := make(url.Values)
	query.Set("limit", strconv.Itoa(*limit))
	var timing secondboxclient.SandboxTiming
	if err := client.RequestJSON(ctx, "getSandboxTiming", secondboxclient.CallOptions{
		PathParameters:  map[string]string{"sandboxId": *sandboxID},
		QueryParameters: query,
	}, &timing); err != nil {
		return err
	}
	return writeSandboxTiming(output, timing)
}

func runOperationTiming(
	ctx context.Context,
	client *secondboxclient.Client,
	args []string,
	output io.Writer,
) error {
	flags := flag.NewFlagSet("timings operation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operationID := flags.String("operation-id", "", "Operation identifier")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox timings operation options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return errors.New("SecondBox timings operation received unexpected arguments")
	}
	if *operationID == "" {
		return errors.New("SecondBox timings operation requires --operation-id")
	}
	var timing secondboxclient.OperationTiming
	if err := client.RequestJSON(ctx, "getOperationTiming", secondboxclient.CallOptions{
		PathParameters: map[string]string{"operationId": *operationID},
	}, &timing); err != nil {
		return err
	}
	return writeOperationTiming(output, timing)
}

func runDeploymentTiming(
	ctx context.Context,
	client *secondboxclient.Client,
	args []string,
	output io.Writer,
) error {
	flags := flag.NewFlagSet("timings summary", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	windowText := flags.String("window", "", "aggregation window from 1m through 1h")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox timings summary options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return errors.New("SecondBox timings summary received unexpected arguments")
	}
	if *windowText == "" {
		return errors.New("SecondBox timings summary requires --window")
	}
	window, err := time.ParseDuration(*windowText)
	if err != nil || window < time.Minute || window > time.Hour ||
		window%time.Second != 0 {
		return errors.New(
			"SecondBox timings summary --window must be whole seconds from 1m through 1h",
		)
	}
	query := make(url.Values)
	query.Set("windowSeconds", strconv.FormatInt(int64(window/time.Second), 10))
	var summary secondboxclient.DeploymentTimingSummary
	if err := client.RequestJSON(ctx, "getDeploymentTiming", secondboxclient.CallOptions{
		QueryParameters: query,
	}, &summary); err != nil {
		return err
	}
	return writeDeploymentTiming(output, summary)
}

func writeSandboxTiming(output io.Writer, timing secondboxclient.SandboxTiming) error {
	if _, err := fmt.Fprintf(output, "Sandbox: %s\n\n", timing.SandboxID); err != nil {
		return fmt.Errorf("SecondBox timings write Sandbox heading: %w", err)
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "OPERATION\tKIND\tSTATE\tQUEUE\tEXECUTION\tTOTAL"); err != nil {
		return fmt.Errorf("SecondBox timings write Operation heading: %w", err)
	}
	for _, operation := range timing.Operations {
		if _, err := fmt.Fprintf(
			table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			operation.OperationID, operation.Kind, operation.State,
			formatMilliseconds(operation.QueueMilliseconds),
			formatMilliseconds(operation.ExecutionMilliseconds),
			formatMilliseconds(operation.TotalMilliseconds),
		); err != nil {
			return fmt.Errorf("SecondBox timings write Operation row: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("SecondBox timings flush Operation table: %w", err)
	}
	for _, operation := range timing.Operations {
		if err := writeBootTimings(output, operation.OperationID, operation.Boots); err != nil {
			return err
		}
	}
	table = tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "\nEXEC\tMODE\tOUTCOME\tELAPSED\tCOMPLETED"); err != nil {
		return fmt.Errorf("SecondBox timings write Exec heading: %w", err)
	}
	for _, exec := range timing.Execs {
		if _, err := fmt.Fprintf(
			table, "%s\t%s\t%s\t%dms\t%s\n",
			exec.SessionID, exec.Mode, exec.Outcome, exec.ElapsedMilliseconds,
			exec.CompletedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("SecondBox timings write Exec row: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("SecondBox timings flush Exec table: %w", err)
	}
	return nil
}

func writeOperationTiming(output io.Writer, operation secondboxclient.OperationTiming) error {
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "OPERATION\tSANDBOX\tKIND\tSTATE\tQUEUE\tEXECUTION\tTOTAL"); err != nil {
		return fmt.Errorf("SecondBox timings write Operation heading: %w", err)
	}
	if _, err := fmt.Fprintf(
		table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		operation.OperationID, operation.SandboxID, operation.Kind, operation.State,
		formatMilliseconds(operation.QueueMilliseconds),
		formatMilliseconds(operation.ExecutionMilliseconds),
		formatMilliseconds(operation.TotalMilliseconds),
	); err != nil {
		return fmt.Errorf("SecondBox timings write Operation row: %w", err)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("SecondBox timings flush Operation table: %w", err)
	}
	return writeBootTimings(output, operation.OperationID, operation.Boots)
}

func writeBootTimings(
	output io.Writer,
	operationID string,
	boots []secondboxclient.BootTiming,
) error {
	for _, boot := range boots {
		if _, err := fmt.Fprintf(
			output,
			"\nBoot: operation=%s generation=%d total=%dms completed=%t\n",
			operationID, boot.Generation, boot.DurationMilliseconds, boot.Completed,
		); err != nil {
			return fmt.Errorf("SecondBox timings write boot heading: %w", err)
		}
		table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "STAGE\tELAPSED\tCUMULATIVE\tOBSERVED\tRECEIVED"); err != nil {
			return fmt.Errorf("SecondBox timings write boot-stage heading: %w", err)
		}
		for _, stage := range boot.Stages {
			if _, err := fmt.Fprintf(
				table, "%s\t%dms\t%dms\t%s\t%s\n",
				stage.Stage, stage.ElapsedMilliseconds, stage.CumulativeMilliseconds,
				stage.ObservedAt.UTC().Format(time.RFC3339Nano),
				stage.ReceivedAt.UTC().Format(time.RFC3339Nano),
			); err != nil {
				return fmt.Errorf("SecondBox timings write boot-stage row: %w", err)
			}
		}
		if err := table.Flush(); err != nil {
			return fmt.Errorf("SecondBox timings flush boot-stage table: %w", err)
		}
	}
	return nil
}

func writeDeploymentTiming(
	output io.Writer,
	summary secondboxclient.DeploymentTimingSummary,
) error {
	if _, err := fmt.Fprintf(
		output, "Window: %ds  Observed: %s\n",
		summary.WindowSeconds, summary.ObservedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("SecondBox timings write summary heading: %w", err)
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "SIGNAL\tCOUNT\tP50\tP95\tP99"); err != nil {
		return fmt.Errorf("SecondBox timings write summary table heading: %w", err)
	}
	for _, signal := range []struct {
		name     string
		duration secondboxclient.DurationPercentiles
	}{
		{name: "boot", duration: summary.Boot},
		{name: "exec", duration: summary.Exec},
		{name: "api", duration: summary.API},
	} {
		if err := writePercentileRow(table, signal.name, signal.duration); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("SecondBox timings flush summary table: %w", err)
	}
	if summary.DominantBootStage != nil {
		if _, err := fmt.Fprintf(
			output, "Dominant boot stage: %s (p95 %s)\n",
			summary.DominantBootStage.Stage,
			formatMilliseconds(summary.DominantBootStage.Duration.P95Milliseconds),
		); err != nil {
			return fmt.Errorf("SecondBox timings write dominant boot stage: %w", err)
		}
	}
	if err := writeAggregateSections(output, summary); err != nil {
		return err
	}
	return nil
}

func writeAggregateSections(
	output io.Writer,
	summary secondboxclient.DeploymentTimingSummary,
) error {
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "\nBOOT_STAGE\tCOUNT\tP50\tP95\tP99"); err != nil {
		return fmt.Errorf("SecondBox timings write boot aggregate heading: %w", err)
	}
	for _, stage := range summary.BootStages {
		if err := writePercentileRow(table, stage.Stage, stage.Duration); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(table, "\nEXEC_SERIES\tCOUNT\tP50\tP95\tP99"); err != nil {
		return fmt.Errorf("SecondBox timings write Exec aggregate heading: %w", err)
	}
	for _, exec := range summary.ExecSeries {
		if err := writePercentileRow(
			table, exec.Mode+"/"+exec.Outcome, exec.Duration,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(table, "\nAPI_ROUTE\tCOUNT\tP50\tP95\tP99"); err != nil {
		return fmt.Errorf("SecondBox timings write API aggregate heading: %w", err)
	}
	for _, api := range summary.APISeries {
		if err := writePercentileRow(
			table, api.Route+" "+api.StatusClass, api.Duration,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(table, "\nOPERATION\tQUEUE_P95\tEXECUTION_P95\tTOTAL_P95\tCOUNT"); err != nil {
		return fmt.Errorf("SecondBox timings write Operation aggregate heading: %w", err)
	}
	for _, operation := range summary.Operations {
		if _, err := fmt.Fprintf(
			table, "%s/%s\t%s\t%s\t%s\t%d\n",
			operation.Kind, operation.State,
			formatMilliseconds(operation.Queue.P95Milliseconds),
			formatMilliseconds(operation.Execution.P95Milliseconds),
			formatMilliseconds(operation.Total.P95Milliseconds),
			operation.Total.Count,
		); err != nil {
			return fmt.Errorf("SecondBox timings write Operation aggregate row: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("SecondBox timings flush aggregate sections: %w", err)
	}
	return nil
}

func writePercentileRow(
	output io.Writer,
	name string,
	duration secondboxclient.DurationPercentiles,
) error {
	if _, err := fmt.Fprintf(
		output, "%s\t%d\t%s\t%s\t%s\n",
		name, duration.Count,
		formatMilliseconds(duration.P50Milliseconds),
		formatMilliseconds(duration.P95Milliseconds),
		formatMilliseconds(duration.P99Milliseconds),
	); err != nil {
		return fmt.Errorf("SecondBox timings write percentile row: %w", err)
	}
	return nil
}

func formatMilliseconds(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10) + "ms"
}
