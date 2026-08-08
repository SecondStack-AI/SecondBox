package main

import "github.com/SecondStack-AI/SecondBox/internal/cliui"

func secondboxHelp() cliui.Help {
	return cliui.Help{
		Title: "SecondBox CLI",
		Usage: "secondbox [GLOBAL OPTIONS] COMMAND [ARGUMENTS]",
		Commands: []cliui.Pair{
			{Key: "help", Value: "show this help"},
			{Key: "version", Value: "show CLI version information"},
			{Key: "login | logout | whoami", Value: "manage the local authenticated session"},
			{Key: "run", Value: "run a command in a temporary Sandbox"},
			{Key: "exec", Value: "execute in an existing Sandbox"},
			{Key: "shell | sandbox shell", Value: "attach an interactive Sandbox terminal"},
			{Key: "sandboxes | snapshots", Value: "manage durable Sandboxes and Snapshots"},
			{Key: "profiles | runner-pools | runners", Value: "inspect and manage compute configuration"},
			{Key: "files | artifacts", Value: "transfer Sandbox files and immutable Artifacts"},
			{Key: "leases | ports", Value: "manage generation Leases and port sessions"},
			{Key: "logs | timings | diagnostics", Value: "inspect bounded operational evidence"},
			{Key: "resources", Value: "check or apply explicit standard resources"},
			{Key: "operation", Value: "invoke an OpenAPI operationId directly"},
		},
		Options: []cliui.Pair{
			{Key: "--url URL", Value: "SecondBox API endpoint"},
			{Key: "--token TOKEN", Value: "platform token; prefer login or config"},
			{Key: "--tenant-ref REF", Value: "trusted caller tenant reference"},
			{Key: "--subject-ref REF", Value: "trusted caller subject reference"},
			{Key: "--output auto|json|plain", Value: "select adaptive, machine, or stable text output"},
			{Key: "--color auto|always|never", Value: "control ANSI color"},
			{Key: "--accessible", Value: "use accessible prompts and output"},
			{Key: "-h, --help", Value: "show this help"},
		},
		Footer: "Global options must precede the command. Use --output plain for stable diagnostic transcripts.",
	}
}
