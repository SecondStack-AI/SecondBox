package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
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
	case "validate":
		if len(arguments) != 2 {
			return usage()
		}
		resolved, err := deployconfig.Resolve(arguments[1])
		if err == nil {
			fmt.Printf("SecondBox deployment manifest valid: %s (%d Runner declarations)\n", arguments[1], len(resolved.Manifest.Runners))
		}
		return err
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
		arguments = append(arguments, "down")
	}
	return runDockerCompose(arguments)
}

func composeUpArguments(arguments []string, options ...string) []string {
	result := append(slices.Clone(arguments), "up", "--remove-orphans")
	return append(result, options...)
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
	return fmt.Errorf("usage: secondbox-deploy {init --mode development DIRECTORY|init --mode production [--input COMPLETE_MANIFEST] DIRECTORY|validate MANIFEST|render --output ENV MANIFEST|runner-init MANIFEST RUNNER_ID TARGET|inspect MANIFEST|migrate LEGACY_ENV TARGET|compose MANIFEST config|prepare|up|down}")
}
