package cliui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

type Renderer struct {
	Output       io.Writer
	Diagnostic   io.Writer
	Capabilities Capabilities
	OutputMode   OutputMode
	ColorMode    ColorMode
}

func (renderer Renderer) StyledOutput() bool {
	if renderer.OutputMode != OutputAuto || renderer.Capabilities.Dumb || renderer.Capabilities.CI {
		return false
	}
	return renderer.colorEnabled(renderer.Capabilities.Output)
}

func (renderer Renderer) StyledDiagnostic() bool {
	if renderer.OutputMode != OutputAuto || renderer.Capabilities.Dumb || renderer.Capabilities.CI {
		return false
	}
	return renderer.colorEnabled(renderer.Capabilities.Diagnostic)
}

func (renderer Renderer) colorEnabled(stream StreamCapabilities) bool {
	switch renderer.ColorMode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default:
		return stream.TTY && !renderer.Capabilities.NoColor
	}
}

func (renderer Renderer) HumanOutput() bool {
	return renderer.OutputMode == OutputPlain || (renderer.OutputMode == OutputAuto && renderer.Capabilities.Output.TTY && !renderer.Capabilities.Dumb)
}

type Pair struct{ Key, Value string }

type Summary struct {
	Title    string
	Status   Status
	Pairs    []Pair
	Warnings []string
	Next     string
}

type Status string

const (
	StatusPending  Status = "pending"
	StatusActive   Status = "active"
	StatusComplete Status = "complete"
	StatusWarning  Status = "warning"
	StatusFailed   Status = "failed"
)

func (renderer Renderer) WriteSummary(summary Summary) error {
	theme := NewTheme(renderer.Capabilities.Output, renderer.StyledOutput())
	var buffer strings.Builder
	title := Sanitize(summary.Title)
	if renderer.StyledOutput() {
		title = theme.Accent.Render(title)
	}
	buffer.WriteString(title + "\n")
	if summary.Status != "" {
		buffer.WriteString(statusMark(summary.Status, renderer.Capabilities.Unicode) + " " + styleStatus(theme, summary.Status, renderer.StyledOutput()) + "\n")
	}
	width := 0
	for _, pair := range summary.Pairs {
		if lipgloss.Width(pair.Key) > width {
			width = lipgloss.Width(pair.Key)
		}
	}
	for _, pair := range summary.Pairs {
		key, value := Sanitize(pair.Key), Sanitize(pair.Value)
		line := fmt.Sprintf("  %-*s  %s", width, key, value)
		if renderer.StyledOutput() {
			line = "  " + theme.Muted.Render(fmt.Sprintf("%-*s", width, key)) + "  " + theme.Primary.Render(value)
		}
		buffer.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	for _, warning := range summary.Warnings {
		line := "! " + Sanitize(warning)
		if renderer.StyledOutput() {
			line = theme.Warning.Render("!") + " " + Sanitize(warning)
		}
		buffer.WriteString(line + "\n")
	}
	if summary.Next != "" {
		line := "Next: " + Sanitize(summary.Next)
		if renderer.StyledOutput() {
			line = theme.Muted.Render("Next:") + " " + Sanitize(summary.Next)
		}
		buffer.WriteString(line + "\n")
	}
	_, err := io.WriteString(renderer.Output, buffer.String())
	return err
}

func (renderer Renderer) WriteError(problem error, hint string) error {
	if problem == nil {
		return nil
	}
	theme := NewTheme(renderer.Capabilities.Diagnostic, renderer.StyledDiagnostic())
	prefix := "Error:"
	if renderer.StyledDiagnostic() {
		prefix = theme.Error.Render(prefix)
	}
	message := prefix + " " + Sanitize(problem.Error()) + "\n"
	if hint != "" {
		label := "Next:"
		if renderer.StyledDiagnostic() {
			label = theme.Muted.Render(label)
		}
		message += label + " " + Sanitize(hint) + "\n"
	}
	_, err := io.WriteString(renderer.Diagnostic, message)
	return err
}

func styleStatus(theme Theme, status Status, styled bool) string {
	text := string(status)
	if !styled {
		return text
	}
	switch status {
	case StatusComplete:
		return theme.Success.Render(text)
	case StatusWarning:
		return theme.Warning.Render(text)
	case StatusFailed:
		return theme.Error.Render(text)
	case StatusActive:
		return theme.Accent.Render(text)
	default:
		return theme.Muted.Render(text)
	}
}

func statusMark(status Status, unicodeOK bool) string {
	if !unicodeOK {
		switch status {
		case StatusComplete:
			return "[ok]"
		case StatusFailed:
			return "[x]"
		case StatusWarning:
			return "[!]"
		case StatusActive:
			return "[>]"
		default:
			return "[ ]"
		}
	}
	switch status {
	case StatusComplete:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusWarning:
		return "!"
	case StatusActive:
		return "●"
	default:
		return "○"
	}
}

// WriteJSONPassthrough deliberately does not decode or normalize validated JSON.
func WriteJSONPassthrough(output io.Writer, validated []byte) error {
	_, err := io.Copy(output, bytes.NewReader(validated))
	return err
}

// Sanitize makes server-controlled values safe to place in a terminal view.
func Sanitize(value string) string {
	var result strings.Builder
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if r == '\n' || r == '\t' || (!unicode.IsControl(r) && r != utf8.RuneError) {
			result.WriteRune(r)
			continue
		}
		result.WriteRune('�')
	}
	return result.String()
}
