package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func composeUpdateCheckRenderer(output io.Writer, mode cliui.OutputMode) cliui.Renderer {
	return cliui.Renderer{Output: output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(output, io.Discard), OutputMode: mode, ColorMode: cliui.ColorNever}
}

func stampBuildIdentity(t *testing.T, version, sourceCommit string) {
	t.Helper()
	originalVersion, originalCommit := buildinfo.Version, buildinfo.SourceCommit
	buildinfo.Version, buildinfo.SourceCommit = version, sourceCommit
	t.Cleanup(func() { buildinfo.Version, buildinfo.SourceCommit = originalVersion, originalCommit })
}

func exitCode(err error) int {
	var exited interface{ ExitCode() int }
	if errors.As(err, &exited) {
		return exited.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

func TestComposeUpdateMinimumSourceVersionSubsumesGuidedBoundaries(t *testing.T) {
	for _, boundary := range []string{minimumGuidedUpdateSourceVersion, tenantEgressRecreationBoundaryVersion} {
		comparison, err := releasecontract.CompareVersions(deployconfig.ComposeUpdateMinimumSourceVersion, boundary)
		if err != nil || comparison <= 0 {
			t.Fatalf("Compose update floor %s does not exceed boundary %s: %v", deployconfig.ComposeUpdateMinimumSourceVersion, boundary, err)
		}
	}
}

func TestComposeUpdateCheckReportsVerdictAndExitStatus(t *testing.T) {
	commit := strings.Repeat("a", 40)
	stampBuildIdentity(t, "0.23.0", commit)
	for _, test := range []struct {
		from, verdict string
		code          int
	}{
		{"0.22.0", "supported", 0},
		{"0.23.0", "supported", 0},
		{"0.13.0", "below_minimum_source_version", 2},
		{"0.24.0", "downgrade", 2},
	} {
		var output bytes.Buffer
		err := runCommand([]string{"compose-update-check", "--from", test.from}, composeUpdateCheckRenderer(&output, cliui.OutputJSON))
		if exitCode(err) != test.code {
			t.Fatalf("--from %s exit = %d (%v), want %d", test.from, exitCode(err), err, test.code)
		}
		var report composeUpdateCheckReport
		if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
			t.Fatalf("--from %s JSON output %q: %v", test.from, output.String(), decodeErr)
		}
		if string(report.Verdict) != test.verdict || report.Supported != (test.code == 0) || report.AppliedVersion != test.from || report.TargetVersion != "0.23.0" || report.TargetSourceCommit != commit || report.MinimumSourceVersion != "0.14.0" {
			t.Fatalf("--from %s report = %#v", test.from, report)
		}
		if test.verdict == "below_minimum_source_version" && !strings.Contains(err.Error(), "requires clean recreation") {
			t.Fatalf("below-floor error = %v", err)
		}
	}
	var plain bytes.Buffer
	if err := runCommand([]string{"compose-update-check", "--from=0.22.0"}, composeUpdateCheckRenderer(&plain, cliui.OutputAuto)); err != nil {
		t.Fatal(err)
	}
	if plain.String() != "SecondBox Compose update check: in-place update from 0.22.0 to 0.23.0 is supported\n" {
		t.Fatalf("automatic output = %q", plain.String())
	}
}

func TestComposeUpdateCheckRejectsInvalidInputAndUnstampedBinary(t *testing.T) {
	stampBuildIdentity(t, "0.23.0", strings.Repeat("a", 40))
	for _, arguments := range [][]string{{}, {"--from"}, {"--from", "0.22.0", "extra"}, {"--from", "0.22.0+build"}, {"--from", "v0.22.0"}} {
		var output bytes.Buffer
		err := runCommand(append([]string{"compose-update-check"}, arguments...), composeUpdateCheckRenderer(&output, cliui.OutputJSON))
		if exitCode(err) != 1 || output.Len() != 0 {
			t.Fatalf("%v exit = %d (%v), output %q", arguments, exitCode(err), err, output.String())
		}
	}
	stampBuildIdentity(t, buildinfo.DevelopmentVersion, buildinfo.DevelopmentSourceCommit)
	err := runCommand([]string{"compose-update-check", "--from", "0.22.0"}, composeUpdateCheckRenderer(io.Discard, cliui.OutputJSON))
	if exitCode(err) != 3 || !strings.Contains(err.Error(), "unstamped development identity") {
		t.Fatalf("development binary exit = %d (%v)", exitCode(err), err)
	}
	stampBuildIdentity(t, "0.23.0", buildinfo.DevelopmentSourceCommit)
	if err := runCommand([]string{"compose-update-check", "--from", "0.22.0"}, composeUpdateCheckRenderer(io.Discard, cliui.OutputJSON)); exitCode(err) != 3 || !strings.Contains(err.Error(), "partially stamped") {
		t.Fatalf("partially stamped binary exit = %d (%v)", exitCode(err), err)
	}
}
