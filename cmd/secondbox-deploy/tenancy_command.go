package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
)

func runBootstrapTenancy(ctx context.Context, arguments []string, renderer cliui.Renderer) (resultErr error) {
	if len(arguments) == 0 {
		return errors.New("SecondBox installer bootstrap-tenancy requires an operation directory")
	}
	flags := flag.NewFlagSet("bootstrap-tenancy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant-ref", "local", "Tenant reference")
	subject := flags.String("subject-ref", "local-operator", "Subject reference")
	application := flags.Bool("application", false, "return a new application bearer token once")
	check := flags.Bool("check", false, "verify prerequisites without changes")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox installer bootstrap-tenancy accepts one operation directory")
	}
	directory, err := filepath.Abs(arguments[0])
	if err != nil {
		return err
	}
	lock, err := install.AcquireLock(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	var plan install.InstallPlan
	var receipt install.InstallReceipt
	if *check {
		plan, receipt, err = install.ReadOperationReadOnly(directory, os.Getuid())
	} else {
		plan, receipt, err = install.RecoverOperation(directory, os.Getuid(), lock)
	}
	if err != nil {
		return err
	}
	if slices.Index(install.StageSequence, lastInstallStage(receipt)) < slices.Index(install.StageSequence, install.StageReadiness) {
		return errors.New("SecondBox installer bootstrap-tenancy requires completed readiness")
	}
	if receipt.Status != install.OperationRunning && receipt.Status != install.OperationFailed && receipt.Status != install.OperationSucceeded {
		return errors.New("SecondBox installer bootstrap-tenancy requires an installed operation")
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer bootstrap-tenancy requires the active update to complete")
	}
	result, err := install.BootstrapTenancy(ctx, plan, install.TenancyOptions{TenantRef: *tenant, SubjectRef: *subject, EgressContext: expectedInstallerComposeProject(plan), Application: *application, Check: *check}, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	if !*check {
		receipt.UpdatedAt = time.Now().UTC()
		receipt.TenancyBootstraps = append(receipt.TenancyBootstraps, install.StageRecord{Stage: install.StageTenancyBootstrap, CompletedAt: receipt.UpdatedAt, Evidence: result.Evidence})
		if err := install.SaveReceipt(directory, plan, receipt, os.Getuid()); err != nil {
			return err
		}
	}
	if result.BearerToken != "" {
		// Explicit credential issuance is the sole secret-bearing output. Never
		// pass it to summary presentation, diagnostics, or operation persistence.
		return json.NewEncoder(renderer.Output).Encode(struct {
			Evidence    map[string]string `json:"evidence"`
			BearerToken string            `json:"bearerToken"`
		}{result.Evidence, result.BearerToken})
	}
	if renderer.OutputMode == cliui.OutputJSON {
		return json.NewEncoder(renderer.Output).Encode(result)
	}
	pairs := []cliui.Pair{}
	keys := make([]string, 0, len(result.Evidence))
	for key := range result.Evidence {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		pairs = append(pairs, cliui.Pair{Key: key, Value: result.Evidence[key]})
	}
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox tenancy bootstrap", Status: cliui.StatusComplete, Pairs: pairs})
}
