package cliui

import (
	"fmt"
	"io"
	"strings"
)

type Phase struct {
	Name, Detail string
	Status       Status
}

func (renderer Renderer) WritePhases(phases []Phase) error {
	theme := NewTheme(renderer.Capabilities.Diagnostic, renderer.StyledDiagnostic())
	var result strings.Builder
	for _, phase := range phases {
		mark := statusMark(phase.Status, renderer.Capabilities.Unicode)
		status := styleStatus(theme, phase.Status, renderer.StyledDiagnostic())
		fmt.Fprintf(&result, "%s %-8s %s", mark, status, Sanitize(phase.Name))
		if phase.Detail != "" {
			fmt.Fprintf(&result, " — %s", Sanitize(phase.Detail))
		}
		result.WriteByte('\n')
	}
	_, err := io.WriteString(renderer.Diagnostic, result.String())
	return err
}
