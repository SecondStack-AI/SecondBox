package main

import "github.com/SecondStack-AI/SecondBox/internal/cliui"

func secondboxHelp() cliui.Help {
	return cliui.Help{
		Title: "SecondBox CLI",
		Usage: "secondbox [GLOBAL OPTIONS] COMMAND [ARGUMENTS]",
		Commands: []cliui.Pair{
			{Key: "help", Value: "show this help"},
			{Key: "version", Value: "show CLI version information"},
			{Key: "platform login | controller login | application login", Value: "verify and store one typed authority"},
			{Key: "logout | whoami", Value: "inspect or remove the local authenticated session"},
			{Key: "tenant", Value: "manage tenants and tenant-controller authorities"},
			{Key: "subject | application-authority | usage", Value: "manage one authenticated tenant"},
			{Key: "run", Value: "run a command in a temporary Sandbox"},
			{Key: "exec", Value: "execute in an existing Sandbox"},
			{Key: "create PROFILE [--name NAME] [--metadata k=v] [--from REF]", Value: "create a Sandbox; return its Operation"},
			{Key: "start | stop | rm (delete) SANDBOX [--no-wait]", Value: "change lifecycle and wait; rm --force skips TTY confirmation"},
			{Key: "ls (list) [--all] [--name NAME] | get SANDBOX", Value: "inspect Sandboxes; --all includes other states in human output"},
			{Key: "cp [-r] SRC DST | ls-files SANDBOX /workspace/PATH", Value: "copy files using sandbox:/workspace/path operands or list a directory"},
			{Key: "ports forward SANDBOX LOCAL:REMOTE [--bind ADDRESS]", Value: "forward a local TCP port; one number uses the same port on both sides"},
			{Key: "snapshot SANDBOX --name NAME | snapshots SANDBOX", Value: "create a ready Snapshot or list Snapshots"},
			{Key: "restore SANDBOX REF | snapshot rm REF", Value: "restore or delete a Snapshot; REF is sandbox/name or snp_ identifier"},
			{Key: "shell | sandbox shell", Value: "attach an interactive Sandbox terminal"},
			{Key: "sandboxes | snapshots", Value: "manage durable Sandboxes and Snapshots"},
			{Key: "profiles | runner-pools | runners", Value: "inspect and manage compute configuration"},
			{Key: "files", Value: "transfer Sandbox files"},
			{Key: "leases | ports", Value: "manage generation Leases and port sessions"},
			{Key: "logs | timings | diagnostics", Value: "inspect bounded operational evidence and egress-context readiness"},
			{Key: "resources", Value: "check or apply explicit standard resources"},
			{Key: "operation", Value: "invoke an OpenAPI operationId directly"},
		},
		Options: []cliui.Pair{
			{Key: "--url URL", Value: "SecondBox API endpoint"},
			{Key: "--token TOKEN", Value: "bearer token; prefer a typed login or config"},
			{Key: "--authority-kind KIND", Value: "platform, tenant_controller, or application"},
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
