package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
)

// Exit statuses of compose-update-check beyond 0 (supported) and 1 (invalid
// arguments or versions).
const (
	composeUpdateCheckUnsupportedExitCode = 2
	composeUpdateCheckUnstampedExitCode   = 3
)

type composeUpdateCheckReport struct {
	Supported            bool                              `json:"supported"`
	Verdict              deployconfig.ComposeUpdateVerdict `json:"verdict"`
	AppliedVersion       string                            `json:"appliedVersion"`
	TargetVersion        string                            `json:"targetVersion"`
	TargetSourceCommit   string                            `json:"targetSourceCommit"`
	MinimumSourceVersion string                            `json:"minimumSourceVersion"`
	Message              string                            `json:"message"`
}

// runComposeUpdateCheck answers whether this binary's release may replace the
// applied release of a Compose-managed deployment in place.
func runComposeUpdateCheck(arguments []string, renderer cliui.Renderer) error {
	flags := flag.NewFlagSet("compose-update-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from := flags.String("from", "", "applied release version")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("SecondBox Compose update check: %w", err)
	}
	if flags.NArg() != 0 || *from == "" {
		return errors.New("SecondBox Compose update check: usage: secondbox-deploy compose-update-check --from VERSION")
	}
	identity := buildinfo.Current()
	development, err := identity.Development()
	if err != nil {
		return &deployExitError{code: composeUpdateCheckUnstampedExitCode, err: fmt.Errorf("SecondBox Compose update check: %w", err)}
	}
	if development {
		return &deployExitError{code: composeUpdateCheckUnstampedExitCode, err: fmt.Errorf("SecondBox Compose update check: this secondbox-deploy carries the unstamped development identity %s; build it with an explicit RELEASE_VERSION and SOURCE_COMMIT", identity.Version)}
	}
	verdict, message, err := deployconfig.CheckComposeUpdateSource(*from, identity.Version)
	if err != nil {
		return err
	}
	report := composeUpdateCheckReport{Supported: verdict == deployconfig.ComposeUpdateSupported, Verdict: verdict, AppliedVersion: *from, TargetVersion: identity.Version, TargetSourceCommit: identity.SourceCommit, MinimumSourceVersion: deployconfig.ComposeUpdateMinimumSourceVersion, Message: message}
	if renderer.OutputMode == cliui.OutputJSON {
		if err := json.NewEncoder(renderer.Output).Encode(report); err != nil {
			return err
		}
	}
	if !report.Supported {
		return &deployExitError{code: composeUpdateCheckUnsupportedExitCode, err: errors.New("SecondBox Compose update check: " + message)}
	}
	if renderer.OutputMode == cliui.OutputJSON {
		return nil
	}
	pairs := []cliui.Pair{{Key: "Applied version", Value: report.AppliedVersion}, {Key: "Target version", Value: report.TargetVersion}, {Key: "Target source commit", Value: report.TargetSourceCommit}, {Key: "Minimum source version", Value: report.MinimumSourceVersion}}
	if renderer.HumanOutput() {
		return renderer.WriteSummary(cliui.Summary{Title: "Compose in-place update supported", Status: cliui.StatusComplete, Pairs: pairs})
	}
	_, err = fmt.Fprintf(renderer.Output, "SecondBox Compose update check: %s\n", message)
	return err
}
