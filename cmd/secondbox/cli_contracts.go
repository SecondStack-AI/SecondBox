package main

import (
	"context"
	"io"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
)

// commandContracts is the stdout/exit-status inventory. Raw and guest-stream
// entries are never passed to a human renderer, even on a TTY.
var commandContracts = map[string]cliui.CommandContract{
	"help":                        {Command: "help", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"version":                     {Command: "version", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"login":                       {Command: "login", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"application login":           {Command: "application login", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"platform login":              {Command: "platform login", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"controller login":            {Command: "controller login", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"logout":                      {Command: "logout", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"whoami":                      {Command: "whoami", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"run":                         {Command: "run", Output: cliui.OutputGuestStream, ReadsStdin: true, LongLived: true, ExitOwner: "guest"},
	"exec":                        {Command: "exec", Output: cliui.OutputGuestStream, ReadsStdin: true, LongLived: true, ExitOwner: "guest"},
	"shell":                       {Command: "shell", Output: cliui.OutputGuestStream, ReadsStdin: true, LongLived: true, ExitOwner: "guest"},
	"sandbox shell":               {Command: "sandbox shell", Output: cliui.OutputGuestStream, ReadsStdin: true, LongLived: true, ExitOwner: "guest"},
	"exec stream":                 {Command: "exec stream", Output: cliui.OutputGuestStream, ReadsStdin: true, LongLived: true, ExitOwner: "guest"},
	"logs tail":                   {Command: "logs tail", Output: cliui.OutputRawBytes, ExitOwner: "cli"},
	"logs follow":                 {Command: "logs follow", Output: cliui.OutputRawBytes, LongLived: true, ExitOwner: "cli"},
	"diagnostics bundle":          {Command: "diagnostics bundle", Output: cliui.OutputRawBytes, ExitOwner: "cli"},
	"diagnostics egress-contexts": {Command: "diagnostics egress-contexts", Output: cliui.OutputMachineJSON, ExitOwner: "cli"},
	"timings sandbox":             {Command: "timings sandbox", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"timings operation":           {Command: "timings operation", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"timings summary":             {Command: "timings summary", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"resources check":             {Command: "resources check", Output: cliui.OutputMachineJSON, ReadsStdin: true, ExitOwner: "cli"},
	"resources apply":             {Command: "resources apply", Output: cliui.OutputMachineJSON, ReadsStdin: true, ExitOwner: "cli"},
	"operation":                   {Command: "operation", Output: cliui.OutputRawBytes, ReadsStdin: true, ExitOwner: "api"},
	"tenant":                      {Command: "tenant", Output: cliui.OutputBoundedHuman, ReadsStdin: true, ExitOwner: "api"},
	"controller-authority":        {Command: "controller-authority", Output: cliui.OutputBoundedHuman, ReadsStdin: true, ExitOwner: "api"},
	"subject":                     {Command: "subject", Output: cliui.OutputBoundedHuman, ReadsStdin: true, ExitOwner: "api"},
	"application-authority":       {Command: "application-authority", Output: cliui.OutputBoundedHuman, ReadsStdin: true, ExitOwner: "api"},
	"usage":                       {Command: "usage", Output: cliui.OutputBoundedHuman, ExitOwner: "api"},
	"deployment usage":            {Command: "deployment usage", Output: cliui.OutputBoundedHuman, ExitOwner: "api"},
}

func init() {
	for command := range commandAliases {
		if _, exists := commandContracts[command]; exists {
			continue
		}
		kind := cliui.OutputMachineJSON
		switch command {
		case "files read":
			kind = cliui.OutputRawBytes
		case "exec stream":
			kind = cliui.OutputGuestStream
		}
		commandContracts[command] = cliui.CommandContract{Command: command, Output: kind, ExitOwner: "api"}
	}
}

type presentationContextKey struct{}

type presentation struct {
	renderer   cliui.Renderer
	accessible bool
	input      io.Reader
}

func withPresentation(ctx context.Context, value presentation) context.Context {
	return context.WithValue(ctx, presentationContextKey{}, value)
}

func presentationFromContext(ctx context.Context, output io.Writer) presentation {
	if value, ok := ctx.Value(presentationContextKey{}).(presentation); ok {
		return value
	}
	capabilities := cliui.ForWriter(output, nil)
	return presentation{renderer: cliui.Renderer{Output: output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: cliui.OutputAuto, ColorMode: cliui.ColorAuto}}
}
