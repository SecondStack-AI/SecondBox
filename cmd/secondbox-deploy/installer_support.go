package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
)

func runInstallSupport(ctx context.Context, arguments []string, renderer cliui.Renderer) (resultErr error) {
	if len(arguments) != 3 || arguments[1] != "--output" {
		return errors.New("SecondBox installer support: expected DIRECTORY --output ABSOLUTE_ARCHIVE")
	}
	directory, err := filepath.Abs(arguments[0])
	if err != nil {
		return err
	}
	output := arguments[2]
	lock, err := install.AcquireLock(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err := install.ReadOperation(directory, os.Getuid())
	if err != nil {
		return err
	}
	evidence := map[string][]byte{}
	inspection, inspectErr := deployconfig.Inspect(installerPlannedPath(plan, "manifest"))
	evidence["manifest-inspection.json"] = boundedEvidence(inspection)
	evidence["manifest-inspection.status"] = evidenceStatus(inspectErr)

	manifestPath := installerPlannedPath(plan, "manifest")
	collectComposeEvidence(ctx, manifestPath, evidence)
	if plan.Storage.MountUnitPath != "" {
		collectHostCommandEvidence(ctx, evidence, "systemd-mount", "systemctl", "show", "--no-pager", "--", filepath.Base(plan.Storage.MountUnitPath))
	}
	collectHostCommandEvidence(ctx, evidence, "workspace-findmnt", "findmnt", "--json", "--output", "TARGET,SOURCE,FSTYPE,OPTIONS", "--target", plan.Storage.WorkspacePath)
	collectHostCommandEvidence(ctx, evidence, "workspace-df", "df", "--block-size=1", "--output=source,fstype,size,used,avail,target", plan.Storage.WorkspacePath)
	collectInstalledCLIEvidence(ctx, plan, evidence)
	collectControlPlaneHealth(ctx, plan, evidence)

	if err := install.WriteSupportBundle(output, plan, receipt, evidence); err != nil {
		return err
	}
	return writeDeployReceipt(renderer, "Installer support bundle created", []cliui.Pair{{Key: "Archive", Value: output}, {Key: "Operation", Value: plan.OperationID}}, output+"\n")
}

func collectComposeEvidence(ctx context.Context, manifestPath string, evidence map[string][]byte) {
	for _, item := range []struct {
		name    string
		command []string
	}{{"compose-status", []string{"ps", "--format", "json"}}, {"runner-log-tail", []string{"logs", "--no-color", "--tail", "200", "runner"}}} {
		arguments, err := deployconfig.ComposeDiagnosticArguments(manifestPath, item.command...)
		if err != nil {
			evidence[item.name+".status"] = evidenceStatus(err)
			continue
		}
		var output bytes.Buffer
		executor := deployconfig.SystemComposeExecutor{Output: &output, Diagnostic: &output}
		err = executor.Run(ctx, arguments)
		evidence[item.name+".txt"] = boundedEvidence(output.Bytes())
		evidence[item.name+".status"] = evidenceStatus(err)
	}
}

func collectHostCommandEvidence(ctx context.Context, evidence map[string][]byte, label, name string, arguments ...string) {
	commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, arguments...)
	command.Env = installerCommandEnvironment("")
	output := boundedCommandBuffer{maximum: int(maximumInstallerEvidenceBytes())}
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	evidence[label+".txt"] = boundedEvidence(output.Bytes())
	evidence[label+".status"] = evidenceStatus(err)
}

func collectInstalledCLIEvidence(ctx context.Context, plan install.InstallPlan, evidence map[string][]byte) {
	command, output, diagnostic := installedCLICommand(ctx, plan, "--output", "json", "runners", "list", "--query", "limit=100")
	err := command.Run()
	evidence["runner-health.json"] = boundedEvidence(output.Bytes())
	evidence["runner-health.stderr"] = boundedEvidence(diagnostic.Bytes())
	evidence["runner-health.status"] = evidenceStatus(err)
}

func collectControlPlaneHealth(ctx context.Context, plan install.InstallPlan, evidence map[string][]byte) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+plan.Network.APIAddress+"/healthz", nil)
	if err != nil {
		evidence["control-plane-health.status"] = evidenceStatus(err)
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		evidence["control-plane-health.status"] = evidenceStatus(err)
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumInstallerEvidenceBytes()+1))
	closeErr := response.Body.Close()
	evidence["control-plane-health.body"] = boundedEvidence(body)
	evidence["control-plane-health.status"] = []byte(fmt.Sprintf("http=%d error=%s\n", response.StatusCode, cliui.Sanitize(errorText(errors.Join(readErr, closeErr)))))
}

func maximumInstallerEvidenceBytes() int64 { return 2 << 20 }

func boundedEvidence(content []byte) []byte {
	if len(content) > int(maximumInstallerEvidenceBytes()) {
		content = content[:maximumInstallerEvidenceBytes()]
	}
	return bytes.ToValidUTF8(content, []byte("?"))
}

func evidenceStatus(err error) []byte {
	if err == nil {
		return []byte("ok\n")
	}
	return []byte("error=" + cliui.Sanitize(errorText(err)) + "\n")
}

func errorText(err error) string {
	if err == nil {
		return "none"
	}
	return strings.ReplaceAll(err.Error(), "\n", " ")
}
