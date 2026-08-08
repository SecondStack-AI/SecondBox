package cliui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

type Help struct {
	Title    string
	Usage    string
	Commands []Pair
	Footer   string
}

func (renderer Renderer) FormatHelp(help Help) string {
	styled := renderer.StyledDiagnostic()
	theme := NewTheme(renderer.Capabilities.Diagnostic, styled)
	var result strings.Builder
	title := Sanitize(help.Title)
	if styled {
		title = theme.Accent.Render(title)
	}
	result.WriteString(title + "\n\n")
	heading := "Usage"
	if styled {
		heading = theme.Primary.Bold(true).Render(heading)
	}
	result.WriteString(heading + "\n  " + Sanitize(help.Usage) + "\n")
	if len(help.Commands) > 0 {
		result.WriteString("\n")
		heading = "Commands"
		if styled {
			heading = theme.Primary.Bold(true).Render(heading)
		}
		result.WriteString(heading + "\n")
		width := 0
		for _, command := range help.Commands {
			width = max(width, lipgloss.Width(command.Key))
		}
		for _, command := range help.Commands {
			key := fmt.Sprintf("%-*s", width, Sanitize(command.Key))
			if styled {
				key = theme.Accent.Render(key)
			}
			fmt.Fprintf(&result, "  %s  %s\n", key, Sanitize(command.Value))
		}
	}
	if help.Footer != "" {
		footer := Sanitize(help.Footer)
		if styled {
			footer = theme.Muted.Render(footer)
		}
		result.WriteString("\n" + footer)
	}
	return result.String()
}
