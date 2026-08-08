package cliui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
)

type Help struct {
	Title    string
	Usage    string
	Commands []Pair
	Options  []Pair
	Footer   string
}

func (renderer Renderer) FormatHelp(help Help) string {
	styled := renderer.StyledOutput()
	theme := NewTheme(renderer.Capabilities.Output, styled)
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
	writePairs := func(heading string, pairs []Pair) {
		if len(pairs) == 0 {
			return
		}
		result.WriteString("\n")
		if styled {
			heading = theme.Primary.Bold(true).Render(heading)
		}
		result.WriteString(heading + "\n")
		width := 0
		for _, pair := range pairs {
			width = max(width, lipgloss.Width(pair.Key))
		}
		for _, pair := range pairs {
			key := fmt.Sprintf("%-*s", width, Sanitize(pair.Key))
			if styled {
				key = theme.Accent.Render(key)
			}
			fmt.Fprintf(&result, "  %s  %s\n", key, Sanitize(pair.Value))
		}
	}
	writePairs("Commands", help.Commands)
	writePairs("Global options", help.Options)
	if help.Footer != "" {
		footer := Sanitize(help.Footer)
		if styled {
			footer = theme.Muted.Render(footer)
		}
		result.WriteString("\n" + footer)
	}
	return result.String()
}

func (renderer Renderer) WriteHelp(help Help) error {
	_, err := io.WriteString(renderer.Output, renderer.FormatHelp(help)+"\n")
	return err
}
