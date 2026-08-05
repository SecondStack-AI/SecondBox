package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releasefinalize"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return usage()
	}
	switch arguments[0] {
	case "version":
		if len(arguments) != 1 {
			return usage()
		}
		return buildinfo.Write(os.Stdout)
	case "init":
		if len(arguments) < 4 || arguments[1] != "--mode" {
			return usage()
		}
		switch arguments[2] {
		case "development":
			if len(arguments) != 4 {
				return usage()
			}
			path, err := deployconfig.InitDevelopment(arguments[3])
			if err == nil {
				fmt.Println(path)
			}
			return err
		case "production":
			if len(arguments) == 8 && arguments[3] == "--input" && (arguments[5] == "--release-index" || arguments[5] == "--qualification-artifact-manifest") {
				verified, err := verifyReleaseLocation(arguments[5], arguments[6])
				if err != nil {
					return err
				}
				path, err := deployconfig.InitProductionFromRelease(arguments[4], arguments[7], verified.Manifest, verified.ManifestBytes)
				if err == nil {
					fmt.Println(path)
				}
				return err
			}
			if len(arguments) == 6 && arguments[3] == "--input" {
				path, err := deployconfig.InitProductionFromManifest(arguments[4], arguments[5])
				if err == nil {
					fmt.Println(path)
				}
				return err
			}
			if len(arguments) != 4 {
				return usage()
			}
			path, err := deployconfig.InitProduction(arguments[3])
			if path != "" {
				fmt.Println(path)
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
			return usage()
		}
	case "validate":
		if len(arguments) != 2 {
			return usage()
		}
		resolved, err := deployconfig.Resolve(arguments[1])
		if err == nil {
			fmt.Printf("SecondBox deployment manifest valid: %s (%d Runner declarations)\n", arguments[1], len(resolved.Manifest.Runners))
		}
		return err
	case "verify":
		if len(arguments) != 3 || (arguments[1] != "artifact-manifest" && arguments[1] != "release-index") {
			return usage()
		}
		verified, err := verifyReleaseLocation("--"+arguments[1], arguments[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"version": verified.Manifest.Version, "tag": verified.Manifest.Tag, "sourceCommit": verified.Manifest.SourceCommit, "artifactManifestDigest": releaseDigest(verified.ManifestBytes), "qualified": verified.Qualification != nil})
	case "qualification-attestation":
		if len(arguments) != 7 || arguments[1] != "--manifest" || arguments[3] != "--input" || arguments[5] != "--output" {
			return usage()
		}
		return writeQualificationAttestation(arguments[2], arguments[4], arguments[6])
	case "release-index":
		if len(arguments) != 7 || arguments[1] != "--manifest" || arguments[3] != "--qualification" || arguments[5] != "--output" {
			return usage()
		}
		return writeReleaseIndex(arguments[2], arguments[4], arguments[6])
	case "render":
		if len(arguments) != 4 || arguments[1] != "--output" {
			return usage()
		}
		_, err := deployconfig.Render(arguments[3], arguments[2])
		if err == nil {
			fmt.Println(arguments[2])
		}
		return err
	case "runner-init":
		if len(arguments) != 4 {
			return usage()
		}
		return deployconfig.RunnerInit(arguments[1], arguments[2], arguments[3])
	case "inspect":
		if len(arguments) != 2 {
			return usage()
		}
		output, err := deployconfig.Inspect(arguments[1])
		if err != nil {
			return err
		}
		fmt.Println(string(output))
		return nil
	case "migrate":
		if len(arguments) != 3 {
			return usage()
		}
		path, err := deployconfig.MigrateLegacyEnvironment(arguments[1], arguments[2])
		if err == nil {
			fmt.Println(path)
		}
		return err
	case "compose":
		if len(arguments) != 3 {
			return usage()
		}
		return runCompose(arguments[1], arguments[2])
	default:
		return usage()
	}
}

func writeQualificationAttestation(manifestPath, inputPath, outputPath string) error {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(manifestBytes)
	if err != nil {
		return err
	}
	inputBytes, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	var input releasefinalize.QualificationInput
	decoder := json.NewDecoder(strings.NewReader(string(inputBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	attestation, err := releasefinalize.Qualification(manifest, manifestBytes, input)
	if err != nil {
		return err
	}
	return writeJSONCreateOnly(outputPath, attestation)
}

func writeReleaseIndex(manifestPath, qualificationPath, outputPath string) error {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(manifestBytes)
	if err != nil {
		return err
	}
	qualificationBytes, err := os.ReadFile(qualificationPath)
	if err != nil {
		return err
	}
	index, err := releasefinalize.Index(manifest, manifestBytes, qualificationBytes)
	if err != nil {
		return err
	}
	return writeJSONCreateOnly(outputPath, index)
}

func writeJSONCreateOnly(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func verifyReleaseLocation(kind, location string) (releaseverify.VerifiedRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Minute}
	switch kind {
	case "--release-index":
		return releaseverify.FinalRelease(ctx, location, releaseverify.HTTPFetcher(client))
	case "--qualification-artifact-manifest", "--artifact-manifest":
		return releaseverify.ArtifactManifest(ctx, location, releaseverify.HTTPFetcher(client))
	default:
		return releaseverify.VerifiedRelease{}, fmt.Errorf("SecondBox deployment release verification mode is invalid")
	}
}

func releaseDigest(data []byte) string {
	return releasecontract.Digest(data)
}

func runCompose(manifestPath, action string) error {
	if action != "config" && action != "prepare" && action != "up" && action != "down" {
		return fmt.Errorf("SecondBox deployment manifest: compose action must be config, prepare, up, or down")
	}
	absolute, err := filepath.Abs(manifestPath)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(filepath.Dir(absolute), ".secondbox.generated.env")
	resolved, err := deployconfig.Render(absolute, environmentPath)
	if err != nil {
		return err
	}
	arguments := []string{"compose", "--project-name", "secondbox", "--env-file", environmentPath}
	for _, file := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", file)
	}
	switch action {
	case "config":
		arguments = append(arguments, "config", "--quiet")
	case "prepare":
		if resolved.Manifest.Deployment.Mode != "development" {
			return fmt.Errorf("SecondBox deployment manifest: compose prepare requires development mode")
		}
		waitSeconds := strconv.FormatInt(*resolved.Manifest.Deployment.DevelopmentWaitSeconds, 10)
		if err := runDockerCompose(composeUpArguments(
			arguments,
			"--detach", "--wait", "--wait-timeout", waitSeconds,
			"postgres", "object-store",
		)); err != nil {
			return err
		}
		return runDockerCompose(composeUpArguments(arguments, "--no-deps", "object-store-init"))
	case "up":
		arguments = composeUpArguments(arguments, "--detach")
	case "down":
		arguments = composeDownArguments(arguments)
	}
	if err := runDockerCompose(arguments); err != nil {
		return err
	}
	if action == "up" {
		_, err := deployconfig.ApplyStandardResources(context.Background(), resolved, http.DefaultClient)
		return err
	}
	return nil
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

func usage() error {
	return fmt.Errorf("usage: secondbox-deploy {init --mode development DIRECTORY|init --mode production [--input COMPLETE_MANIFEST [--release-index URL|--qualification-artifact-manifest URL]] DIRECTORY|runner-template [--output FILE]|verify artifact-manifest URL|verify release-index URL|qualification-attestation --manifest FILE --input FILE --output FILE|release-index --manifest FILE --qualification FILE --output FILE|validate MANIFEST|render --output ENV MANIFEST|runner-init MANIFEST RUNNER_ID TARGET|inspect MANIFEST|migrate LEGACY_ENV TARGET|compose MANIFEST config|prepare|up|down}")
}
