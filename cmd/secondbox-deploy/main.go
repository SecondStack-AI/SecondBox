package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var presented *deployPresentationError
		if errors.As(err, &presented) && presented.renderer.Capabilities.Diagnostic.TTY && !presented.renderer.Capabilities.Dumb {
			_ = presented.renderer.WriteError(err, "Review the command and explicit deployment paths above.")
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		status := 1
		var exited interface{ ExitCode() int }
		if errors.As(err, &exited) {
			status = exited.ExitCode()
		}
		os.Exit(status)
	}
}

type deployPresentationError struct {
	cause    error
	renderer cliui.Renderer
}

func (failure *deployPresentationError) Error() string { return failure.cause.Error() }
func (failure *deployPresentationError) Unwrap() error { return failure.cause }

func run(arguments []string) (resultErr error) {
	global := flag.NewFlagSet("secondbox-deploy", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	outputValue := global.String("output", "auto", "output mode: auto, json, or plain")
	colorValue := global.String("color", "auto", "color mode: auto, always, or never")
	accessible := global.Bool("accessible", false, "use accessible prompts and output")
	helpLong := global.Bool("help", false, "show help")
	helpShort := global.Bool("h", false, "show help")
	if err := global.Parse(arguments); err != nil {
		return fmt.Errorf("SecondBox deployment CLI parse global options: %w", err)
	}
	outputMode, err := cliui.ParseOutputMode(*outputValue)
	if err != nil {
		return err
	}
	colorMode, err := cliui.ParseColorMode(*colorValue)
	if err != nil {
		return err
	}
	capabilities := cliui.Probe(cliui.ProbeOptions{Input: os.Stdin, Output: os.Stdout, Diagnostic: os.Stderr, Environment: os.Environ()})
	if *accessible {
		capabilities.Accessible = true
	}
	renderer := cliui.Renderer{Output: os.Stdout, Diagnostic: os.Stderr, Capabilities: capabilities, OutputMode: outputMode, ColorMode: colorMode}
	defer func() {
		if resultErr != nil {
			resultErr = &deployPresentationError{cause: resultErr, renderer: renderer}
		}
	}()
	if *helpLong || *helpShort {
		if len(global.Args()) != 0 {
			return errors.New("SecondBox Deploy --help accepts no command arguments; run secondbox-deploy help")
		}
		return renderer.WriteHelp(secondboxDeployHelp())
	}
	return runCommand(global.Args(), renderer)
}

func runCommand(arguments []string, renderer cliui.Renderer) error {
	if len(arguments) == 0 {
		return renderer.WriteHelp(secondboxDeployHelp())
	}
	switch arguments[0] {
	case "help":
		if len(arguments) != 1 {
			return errors.New("SecondBox Deploy help accepts no arguments")
		}
		return renderer.WriteHelp(secondboxDeployHelp())
	case "version":
		if len(arguments) != 1 {
			return usage(renderer)
		}
		if renderer.HumanOutput() {
			return renderer.WriteSummary(cliui.Summary{Title: "SecondBox Deploy", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Version", Value: buildinfo.Version}, {Key: "Source commit", Value: buildinfo.SourceCommit}}})
		}
		var encoded bytes.Buffer
		if err := buildinfo.Write(&encoded); err != nil {
			return err
		}
		return cliui.WriteJSONPassthrough(os.Stdout, encoded.Bytes())
	case "install":
		if len(arguments) == 3 && arguments[1] == "--resume" {
			return runInstallResume(context.Background(), arguments[2], renderer)
		}
		if len(arguments) == 3 && arguments[1] == "--recover-compose-network" {
			return runInstallComposeRecovery(context.Background(), arguments[2], renderer)
		}
		if len(arguments) == 5 && arguments[1] == "--resume" && arguments[3] == "--candidate-directory" {
			return runInstallCandidateResume(context.Background(), arguments[2], arguments[4], renderer)
		}
		if len(arguments) == 3 && arguments[1] == "--candidate-directory" {
			return runInstallCandidate(context.Background(), arguments[2], renderer)
		}
		if len(arguments) == 5 && arguments[1] == "--support" {
			return runInstallSupport(context.Background(), arguments[2:], renderer)
		}
		return runInstallPreflight(context.Background(), arguments[1:], renderer, install.SystemPreflightProbes())
	case "uninstall":
		return runInstallUninstall(context.Background(), arguments[1:], renderer)
	case "_install-host-apply":
		return runPrivateHostApply(context.Background(), arguments[1:])
	case "_install-host-teardown-verify":
		return runPrivateHostTeardownVerify(context.Background(), arguments[1:])
	case "_install-host-purge":
		return runPrivateHostPurge(context.Background(), arguments[1:])
	case "_install-host-purge-validate":
		return runPrivateHostPurgeValidate(arguments[1:])
	case "init":
		if len(arguments) < 4 || arguments[1] != "--mode" {
			return usage(renderer)
		}
		switch arguments[2] {
		case "development":
			if len(arguments) != 4 {
				return usage(renderer)
			}
			path, err := deployconfig.InitDevelopment(arguments[3])
			if err == nil {
				err = writeDeployReceipt(renderer, "Deployment initialized", []cliui.Pair{{Key: "Manifest", Value: path}}, path+"\n")
			}
			return err
		case "production":
			if len(arguments) == 8 && arguments[3] == "--input" && arguments[5] == "--artifact-manifest" {
				verified, err := verifyReleaseLocation(arguments[6])
				if err != nil {
					return err
				}
				path, err := deployconfig.InitProductionFromRelease(arguments[4], arguments[7], verified.Manifest, verified.ManifestBytes)
				if err == nil {
					err = writeDeployReceipt(renderer, "Deployment initialized", []cliui.Pair{{Key: "Manifest", Value: path}}, path+"\n")
				}
				return err
			}
			if len(arguments) == 6 && arguments[3] == "--input" {
				path, err := deployconfig.InitProductionFromManifest(arguments[4], arguments[5])
				if err == nil {
					err = writeDeployReceipt(renderer, "Deployment initialized", []cliui.Pair{{Key: "Manifest", Value: path}}, path+"\n")
				}
				return err
			}
			if len(arguments) != 4 {
				return usage(renderer)
			}
			path, err := deployconfig.InitProduction(arguments[3])
			if err == nil && path != "" {
				err = writeDeployReceipt(renderer, "Deployment initialized", []cliui.Pair{{Key: "Manifest", Value: path}}, path+"\n")
			}
			return err
		default:
			return fmt.Errorf("SecondBox deployment manifest: init mode must be development or production")
		}
	case "runner-template":
		switch {
		case len(arguments) == 1:
			_, err := os.Stdout.Write(deployconfig.RunnerTemplate())
			return err
		case len(arguments) == 3 && arguments[1] == "--output":
			if err := deployconfig.WriteRunnerTemplate(arguments[2]); err != nil {
				return err
			}
			fmt.Println(arguments[2])
			return nil
		default:
			return usage(renderer)
		}
	case "validate":
		if len(arguments) != 2 {
			return usage(renderer)
		}
		resolved, err := deployconfig.Resolve(arguments[1])
		if err == nil {
			err = writeDeployReceipt(renderer, "Deployment manifest valid", []cliui.Pair{{Key: "Manifest", Value: arguments[1]}, {Key: "Runner declarations", Value: strconv.Itoa(len(resolved.Manifest.Runners))}}, fmt.Sprintf("SecondBox deployment manifest valid: %s (%d Runner declarations)\n", arguments[1], len(resolved.Manifest.Runners)))
		}
		return err
	case "verify":
		if len(arguments) != 3 || arguments[1] != "artifact-manifest" {
			return usage(renderer)
		}
		verified, err := verifyReleaseLocation(arguments[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"version": verified.Manifest.Version, "tag": verified.Manifest.Tag, "sourceCommit": verified.Manifest.SourceCommit, "artifactManifestDigest": releaseDigest(verified.ManifestBytes)})
	case "render":
		if len(arguments) != 4 || arguments[1] != "--output" {
			return usage(renderer)
		}
		_, err := deployconfig.Render(arguments[3], arguments[2])
		if err == nil {
			fmt.Println(arguments[2])
		}
		return err
	case "runner-init":
		if len(arguments) != 4 {
			return usage(renderer)
		}
		if err := deployconfig.RunnerInit(arguments[1], arguments[2], arguments[3]); err != nil {
			return err
		}
		if renderer.HumanOutput() || renderer.OutputMode == cliui.OutputJSON {
			return writeDeployReceipt(renderer, "Runner enrolled", []cliui.Pair{{Key: "Runner", Value: arguments[2]}, {Key: "Identity", Value: arguments[3]}}, "")
		}
		return nil
	case "inspect":
		if len(arguments) != 2 {
			return usage(renderer)
		}
		output, err := deployconfig.Inspect(arguments[1])
		if err != nil {
			return err
		}
		fmt.Println(string(output))
		return nil
	case "migrate":
		if len(arguments) != 3 {
			return usage(renderer)
		}
		path, err := deployconfig.MigrateLegacyEnvironment(arguments[1], arguments[2])
		if err == nil {
			err = writeDeployReceipt(renderer, "Deployment migrated", []cliui.Pair{{Key: "Manifest", Value: path}}, path+"\n")
		}
		return err
	case "compose":
		if len(arguments) != 3 {
			return usage(renderer)
		}
		return runComposePresented(renderer, arguments[1], arguments[2])
	default:
		return usage(renderer)
	}
}

func runComposePresented(renderer cliui.Renderer, manifestPath, action string) error {
	// Docker owns both top-level streams for this passthrough command. Higher
	// level installer phases wrap Compose only when its streams are captured.
	return runCompose(manifestPath, action)
}

func writeDeployReceipt(renderer cliui.Renderer, title string, pairs []cliui.Pair, automatic string) error {
	if renderer.OutputMode == cliui.OutputJSON {
		values := make(map[string]string, len(pairs))
		for _, pair := range pairs {
			values[pair.Key] = pair.Value
		}
		return json.NewEncoder(renderer.Output).Encode(values)
	}
	if renderer.HumanOutput() {
		return renderer.WriteSummary(cliui.Summary{Title: title, Status: cliui.StatusComplete, Pairs: pairs})
	}
	_, err := io.WriteString(renderer.Output, automatic)
	return err
}

type deployExitError struct {
	code int
	err  error
}

func (err *deployExitError) Error() string { return err.err.Error() }
func (err *deployExitError) Unwrap() error { return err.err }
func (err *deployExitError) ExitCode() int { return err.code }

func runInstallPreflight(ctx context.Context, arguments []string, renderer cliui.Renderer, probes install.PreflightProbes) error {
	return runInstallPreflightWith(ctx, arguments, renderer, func(ctx context.Context) (install.HostFacts, error) {
		return install.Preflight(ctx, probes)
	})
}

func runInstallPreflightWith(ctx context.Context, arguments []string, renderer cliui.Renderer, preflight func(context.Context) (install.HostFacts, error)) error {
	return runInstallPreflightWithGuide(ctx, arguments, renderer, preflight, runGuidedInstall)
}

func runInstallPreflightWithGuide(ctx context.Context, arguments []string, renderer cliui.Renderer, preflight func(context.Context) (install.HostFacts, error), guide func(context.Context, cliui.Renderer, install.HostFacts, bool) error) error {
	checkOnly := len(arguments) == 1 && arguments[0] == "--check"
	advanced := len(arguments) == 1 && arguments[0] == "--advanced"
	if len(arguments) != 0 && !checkOnly && !advanced {
		return usage(renderer)
	}
	facts, err := preflight(ctx)
	if err != nil {
		return err
	}
	if renderer.OutputMode == cliui.OutputJSON {
		if err := json.NewEncoder(renderer.Output).Encode(facts); err != nil {
			return fmt.Errorf("SecondBox installer preflight output: %w", err)
		}
	} else {
		if _, err := io.WriteString(renderer.Output, install.RenderPreflight(facts)); err != nil {
			return fmt.Errorf("SecondBox installer preflight output: %w", err)
		}
	}
	if install.HasBlockingFindings(facts) {
		return &deployExitError{code: 2, err: errors.New("SecondBox installer preflight: host has blocked or needs-action findings; review every remedy above")}
	}
	if checkOnly {
		return nil
	}
	return guide(ctx, renderer, facts, advanced)
}

func verifyReleaseLocation(location string) (releaseverify.VerifiedRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return verifyReleaseLocationWithContext(ctx, location)
}

func verifyReleaseLocationWithContext(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	return releaseverify.ArtifactManifest(ctx, location, releaseverify.HTTPFetcher(client))
}

func releaseDigest(data []byte) string {
	return releasecontract.Digest(data)
}

func runCompose(manifestPath, action string) error {
	return deployconfig.RunCompose(context.Background(), manifestPath, action, deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: os.Stdout, Diagnostic: os.Stderr}, http.DefaultClient)
}

func composeUpArguments(arguments []string, options ...string) []string {
	result := append(slices.Clone(arguments), "up", "--remove-orphans")
	return append(result, options...)
}

func composeDownArguments(arguments []string) []string {
	return append(slices.Clone(arguments), "down", "--remove-orphans")
}

func runDockerCompose(arguments []string) error {
	command := exec.Command("docker", arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	allow := map[string]bool{"PATH": true, "HOME": true, "DOCKER_CONFIG": true, "DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true, "DOCKER_API_VERSION": true, "SSH_AUTH_SOCK": true, "XDG_RUNTIME_DIR": true}
	for _, entry := range os.Environ() {
		name := strings.SplitN(entry, "=", 2)[0]
		if allow[name] {
			command.Env = append(command.Env, entry)
		}
	}
	return command.Run()
}

func usage(renderer cliui.Renderer) error {
	_ = renderer
	return errors.New("SecondBox Deploy invalid command or arguments; run secondbox-deploy help")
}
