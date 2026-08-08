package main

import "github.com/SecondStack-AI/SecondBox/internal/cliui"

func secondboxDeployHelp() cliui.Help {
	return cliui.Help{
		Title: "SecondBox Deploy",
		Usage: "secondbox-deploy [GLOBAL OPTIONS] COMMAND [ARGUMENTS]",
		Commands: []cliui.Pair{
			{Key: "help", Value: "show this help"},
			{Key: "version", Value: "show CLI version information"},
			{Key: "install", Value: "guide an install; use --check, --resume DIRECTORY, or --support DIRECTORY --output ARCHIVE"},
			{Key: "uninstall", Value: "stop a deployment while preserving data; use --purge for deletion"},
			{Key: "init", Value: "create an explicit deployment manifest"},
			{Key: "validate", Value: "validate a deployment manifest"},
			{Key: "runner-template", Value: "emit the Runner TOML template"},
			{Key: "runner-init", Value: "issue one Runner identity"},
			{Key: "verify", Value: "verify a release artifact manifest"},
			{Key: "inspect", Value: "emit redacted deployment JSON"},
			{Key: "render", Value: "render a process environment file"},
			{Key: "migrate", Value: "migrate a legacy environment"},
			{Key: "compose", Value: "run config, prepare, up, or down"},
		},
		Options: []cliui.Pair{
			{Key: "--output auto|json|plain", Value: "select adaptive, machine, or stable text output"},
			{Key: "--color auto|always|never", Value: "control ANSI color"},
			{Key: "--accessible", Value: "use accessible prompts and output"},
			{Key: "-h, --help", Value: "show this help"},
		},
		Footer: "Global options must precede the command. Generated deployment artifacts remain machine-authoritative.",
	}
}
