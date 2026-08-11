package deployconfig

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type ComposeExecutor interface {
	Run(context.Context, []string) error
}

// RunCompose renders the validated deployment and delegates one exact Compose
// action through a narrow executor. Resource application remains the existing
// idempotent engine and runs only after Compose startup succeeds.
func RunCompose(ctx context.Context, manifestPath, action string, executor ComposeExecutor, httpClient *http.Client) error {
	return runCompose(ctx, manifestPath, action, executor, httpClient, true)
}

// RunComposeForAcceptedInstaller uses the accepted installer's independently
// revalidated root-host evidence while retaining all ordinary manifest checks.
func RunComposeForAcceptedInstaller(ctx context.Context, manifestPath, action string, executor ComposeExecutor, httpClient *http.Client) error {
	return runCompose(ctx, manifestPath, action, executor, httpClient, false)
}

func runCompose(ctx context.Context, manifestPath, action string, executor ComposeExecutor, httpClient *http.Client, validateSameHost bool) error {
	if action != "config" && action != "prepare" && action != "up" && action != "down" && action != "stop-control-plane" {
		return manifestError("compose action must be config, prepare, up, down, or stop-control-plane", nil)
	}
	if executor == nil {
		return manifestError("Compose executor is required", nil)
	}
	absolute, err := filepath.Abs(manifestPath)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(filepath.Dir(absolute), ".secondbox.generated.env")
	var resolved ResolvedDeployment
	if validateSameHost {
		resolved, err = Render(absolute, environmentPath)
	} else {
		resolved, err = renderForAcceptedInstaller(absolute, environmentPath)
	}
	if err != nil {
		return err
	}
	arguments := []string{"compose", "--project-name", resolved.ComposeProject(), "--env-file", environmentPath}
	for _, file := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", file)
	}
	switch action {
	case "config":
		arguments = append(arguments, "config", "--quiet")
	case "prepare":
		if resolved.Manifest.Deployment.Mode != "development" {
			return manifestError("compose prepare requires development mode", nil)
		}
		waitSeconds := fmt.Sprint(*resolved.Manifest.Deployment.DevelopmentWaitSeconds)
		return executor.Run(ctx, composeUpArgumentsInternal(arguments, "--detach", "--wait", "--wait-timeout", waitSeconds, "postgres"))
	case "up":
		arguments = composeUpArgumentsInternal(arguments, "--detach")
	case "down":
		arguments = append(slices.Clone(arguments), "down", "--remove-orphans")
	case "stop-control-plane":
		arguments = append(slices.Clone(arguments), "stop", "control-plane")
	}
	if err := executor.Run(ctx, arguments); err != nil {
		return err
	}
	if action == "up" {
		if httpClient == nil {
			return manifestError("Compose startup requires a resource-apply HTTP client", nil)
		}
		_, err := ApplyStandardResources(ctx, resolved, httpClient)
		return err
	}
	return nil
}

// PurgeComposeVolumes removes the exact validated deployment's containers,
// networks, and named volumes. It is intentionally separate from ordinary
// Compose down because uninstall preserves the bundled database and object
// while the typed permanent-purge workflow must remove it.
func PurgeComposeVolumes(ctx context.Context, manifestPath string, executor ComposeExecutor) error {
	return purgeComposeVolumes(ctx, manifestPath, executor, true)
}

func PurgeComposeVolumesForAcceptedInstaller(ctx context.Context, manifestPath string, executor ComposeExecutor) error {
	return purgeComposeVolumes(ctx, manifestPath, executor, false)
}

func purgeComposeVolumes(ctx context.Context, manifestPath string, executor ComposeExecutor, validateSameHost bool) error {
	if executor == nil {
		return manifestError("Compose executor is required", nil)
	}
	absolute, err := filepath.Abs(manifestPath)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(filepath.Dir(absolute), ".secondbox.generated.env")
	var resolved ResolvedDeployment
	if validateSameHost {
		resolved, err = Render(absolute, environmentPath)
	} else {
		resolved, err = renderForAcceptedInstaller(absolute, environmentPath)
	}
	if err != nil {
		return err
	}
	arguments := []string{"compose", "--project-name", resolved.ComposeProject(), "--env-file", environmentPath}
	for _, file := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", file)
	}
	return executor.Run(ctx, append(arguments, "down", "--remove-orphans", "--volumes"))
}

func composeUpArgumentsInternal(arguments []string, options ...string) []string {
	result := append(slices.Clone(arguments), "up", "--remove-orphans")
	return append(result, options...)
}

// ComposeDiagnosticArguments returns the exact existing deployment transport
// for bounded read-only Docker Compose inspection without rerendering it.
func ComposeDiagnosticArguments(manifestPath string, command ...string) ([]string, error) {
	return composeDiagnosticArguments(manifestPath, true, command...)
}

func ComposeDiagnosticArgumentsForAcceptedInstaller(manifestPath string, command ...string) ([]string, error) {
	return composeDiagnosticArguments(manifestPath, false, command...)
}

func composeDiagnosticArguments(manifestPath string, validateSameHost bool, command ...string) ([]string, error) {
	absolute, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, err
	}
	resolved, err := resolvePath(absolute, validateSameHost)
	if err != nil {
		return nil, err
	}
	environmentPath := filepath.Join(filepath.Dir(absolute), ".secondbox.generated.env")
	info, err := os.Lstat(environmentPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, manifestError("existing generated environment is absent or unprotected", err)
	}
	arguments := []string{"compose", "--project-name", resolved.ComposeProject(), "--env-file", environmentPath}
	composeFiles, err := existingMaterializedComposeFiles(environmentPath, resolved.ComposeFiles)
	if err != nil {
		return nil, err
	}
	for _, file := range composeFiles {
		arguments = append(arguments, "--file", file)
	}
	return append(arguments, command...), nil
}

func existingMaterializedComposeFiles(environmentPath string, selected []string) ([]string, error) {
	directory := environmentPath + ".compose"
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, manifestError("existing generated Compose asset directory is absent or unprotected", err)
	}
	files := make([]string, 0, len(selected))
	for _, logicalPath := range selected {
		path := filepath.Join(directory, filepath.Base(logicalPath))
		fileInfo, err := os.Lstat(path)
		if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 || fileInfo.Mode().Perm()&0o077 != 0 {
			return nil, manifestError("existing generated Compose asset is absent or unprotected: "+logicalPath, err)
		}
		files = append(files, path)
	}
	return files, nil
}

type SystemComposeExecutor struct {
	Input      io.Reader
	Output     io.Writer
	Diagnostic io.Writer
}

func (executor SystemComposeExecutor) Run(ctx context.Context, arguments []string) error {
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Stdin, command.Stdout, command.Stderr = executor.Input, executor.Output, executor.Diagnostic
	command.Env = composeEnvironment()
	return command.Run()
}

func composeEnvironment() []string {
	allow := map[string]bool{"PATH": true, "HOME": true, "DOCKER_CONFIG": true, "DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true, "DOCKER_API_VERSION": true, "SSH_AUTH_SOCK": true, "XDG_RUNTIME_DIR": true}
	result := []string{}
	for _, entry := range os.Environ() {
		name := strings.SplitN(entry, "=", 2)[0]
		if allow[name] {
			result = append(result, entry)
		}
	}
	slices.Sort(result)
	return result
}
