package main

import "github.com/SecondStack-AI/SecondBox/internal/cliui"

var deployCommandContracts = map[string]cliui.CommandContract{
	"help":            {Command: "help", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"version":         {Command: "version", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"install":         {Command: "install", Output: cliui.OutputBoundedHuman, ReadsStdin: true, LongLived: true, ExitOwner: "installer"},
	"init":            {Command: "init", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"runner-template": {Command: "runner-template", Output: cliui.OutputRawBytes, ExitOwner: "cli"},
	"verify":          {Command: "verify", Output: cliui.OutputMachineJSON, LongLived: true, ExitOwner: "cli"},
	"validate":        {Command: "validate", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"render":          {Command: "render", Output: cliui.OutputRawBytes, ExitOwner: "cli"},
	"runner-init":     {Command: "runner-init", Output: cliui.OutputRawBytes, ExitOwner: "cli"},
	"inspect":         {Command: "inspect", Output: cliui.OutputMachineJSON, ExitOwner: "cli"},
	"migrate":         {Command: "migrate", Output: cliui.OutputBoundedHuman, ExitOwner: "cli"},
	"compose":         {Command: "compose", Output: cliui.OutputSubprocess, ReadsStdin: true, LongLived: true, ExitOwner: "docker-compose"},
}
