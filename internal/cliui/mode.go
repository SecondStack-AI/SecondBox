// Package cliui owns terminal capability detection and human-facing rendering
// for the secondbox and secondbox-deploy commands. Domain and command packages
// pass typed values to this package; they do not construct terminal styles.
package cliui

import (
	"fmt"
	"strings"
)

// OutputMode selects the representation written by bounded commands.
type OutputMode string

const (
	OutputAuto  OutputMode = "auto"
	OutputJSON  OutputMode = "json"
	OutputPlain OutputMode = "plain"
)

func ParseOutputMode(value string) (OutputMode, error) {
	mode := OutputMode(strings.ToLower(value))
	switch mode {
	case OutputAuto, OutputJSON, OutputPlain:
		return mode, nil
	default:
		return "", fmt.Errorf("SecondBox CLI output mode must be auto, json, or plain")
	}
}

// ColorMode controls ANSI color independently of the output representation.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

func ParseColorMode(value string) (ColorMode, error) {
	mode := ColorMode(strings.ToLower(value))
	switch mode {
	case ColorAuto, ColorAlways, ColorNever:
		return mode, nil
	default:
		return "", fmt.Errorf("SecondBox CLI color mode must be auto, always, or never")
	}
}

// OutputKind records a command's permanent stdout contract.
type OutputKind string

const (
	OutputBoundedHuman OutputKind = "bounded-human"
	OutputMachineJSON  OutputKind = "machine-json"
	OutputRawBytes     OutputKind = "raw-bytes"
	OutputGuestStream  OutputKind = "guest-stream"
	OutputSubprocess   OutputKind = "subprocess"
)

// CommandContract is the reviewable inventory used to prevent a new command
// from accidentally gaining presentation bytes.
type CommandContract struct {
	Command    string
	Output     OutputKind
	ReadsStdin bool
	LongLived  bool
	ExitOwner  string
}
