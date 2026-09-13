package scenario_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var scenarioCLIBuild struct {
	sync.Once
	directory string
	binary    string
	err       error
}

func TestMain(m *testing.M) {
	flag.Parse()
	tier := os.Getenv("SECONDBOX_SCENARIO_TIER")
	switch tier {
	case "", "release":
		skip := "^(TestScenarioCustomerSharedTenancyEndToEnd|TestScenarioSnapshotResumeStartsStopsAndMeasures)$"
		if existing := flag.Lookup("test.skip").Value.String(); existing != "" {
			skip += "|" + existing
		}
		if err := flag.Set("test.skip", skip); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "nightly":
	default:
		fmt.Fprintln(os.Stderr, "SecondBox scenario tier must be release or nightly")
		os.Exit(1)
	}
	code := m.Run()
	if scenarioCLIBuild.directory != "" {
		if err := os.RemoveAll(scenarioCLIBuild.directory); err != nil {
			fmt.Fprintln(os.Stderr, "SecondBox scenario CLI build cleanup:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func scenarioCLIBinary(t *testing.T) string {
	t.Helper()
	scenarioCLIBuild.Do(func() {
		scenarioCLIBuild.directory, scenarioCLIBuild.err = os.MkdirTemp("", "secondbox-scenario-cli-*")
		if scenarioCLIBuild.err != nil {
			return
		}
		scenarioCLIBuild.binary = filepath.Join(scenarioCLIBuild.directory, "secondbox")
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		build := exec.CommandContext(ctx, "go", "build", "-o", scenarioCLIBuild.binary, "../../cmd/secondbox")
		if output, err := build.CombinedOutput(); err != nil {
			scenarioCLIBuild.err = fmt.Errorf("SecondBox scenario build CLI: %w: %s", err, output)
		}
	})
	if scenarioCLIBuild.err != nil {
		t.Fatal(scenarioCLIBuild.err)
	}
	return scenarioCLIBuild.binary
}

type scenarioCLI struct {
	binary      string
	environment []string
	token       string
}

type scenarioCLIResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// Each subprocess uses only the fixture's application authority. An ambient
// platform session or an existing user config must never influence the scenario.
func newScenarioCLI(t *testing.T, baseURL, token, tenant, subject string) scenarioCLI {
	t.Helper()
	cli := scenarioCLI{binary: scenarioCLIBinary(t), token: token}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SECONDBOX_") {
			cli.environment = append(cli.environment, entry)
		}
	}
	cli.environment = append(cli.environment,
		"SECONDBOX_CONFIG="+filepath.Join(t.TempDir(), "config.json"),
		"SECONDBOX_URL="+baseURL, "SECONDBOX_TOKEN="+token,
		"SECONDBOX_TENANT_REF="+tenant, "SECONDBOX_SUBJECT_REF="+subject,
		"SECONDBOX_AUTHORITY_KIND=application",
	)
	return cli
}

func (cli scenarioCLI) invoke(ctx context.Context, args ...string) (scenarioCLIResult, error) {
	command := exec.CommandContext(ctx, cli.binary, args...)
	command.Env = cli.environment
	command.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := scenarioCLIResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return result, err
		}
		result.exitCode = exit.ExitCode()
	}
	return result, nil
}

func (cli scenarioCLI) redact(text string) string {
	if cli.token == "" {
		return text
	}
	return strings.ReplaceAll(text, cli.token, "[REDACTED]")
}

func (cli scenarioCLI) run(t *testing.T, ctx context.Context, args ...string) scenarioCLIResult {
	t.Helper()
	result, err := cli.invoke(ctx, args...)
	if err != nil {
		t.Fatalf("SecondBox scenario CLI %v: %s", args, cli.redact(err.Error()))
	}
	// Do not include either stream in the failure if a credential was emitted.
	if cli.token != "" && (strings.Contains(result.stdout, cli.token) || strings.Contains(result.stderr, cli.token)) {
		t.Fatal("SecondBox scenario CLI exposed application credential")
	}
	return result
}

func (cli scenarioCLI) success(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	result := cli.run(t, ctx, args...)
	if result.exitCode != 0 {
		t.Fatalf("SecondBox scenario CLI %v exit=%d stdout=%q stderr=%q", args, result.exitCode, result.stdout, result.stderr)
	}
	return result.stdout
}

func scenarioCLIJSON[T any](t *testing.T, output string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("SecondBox scenario CLI JSON: %v; stdout=%q", err, output)
	}
	return value
}
