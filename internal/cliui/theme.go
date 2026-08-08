package cliui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type Theme struct {
	Accent  lipgloss.Style
	Primary lipgloss.Style
	Muted   lipgloss.Style
	Success lipgloss.Style
	Warning lipgloss.Style
	Error   lipgloss.Style
}

func NewTheme(capability StreamCapabilities, styled bool) Theme {
	if !styled {
		return Theme{}
	}
	accent, primary, muted := "#7C6CF2", "#F4F2FF", "#A6A1B3"
	if capability.Background == BackgroundLight {
		accent, primary, muted = "#5143B8", "#24212C", "#6B6575"
	}
	convert := func(trueColor, ansi256, ansi string) color.Color {
		switch capability.Color {
		case ProfileTrueColor:
			return lipgloss.Color(trueColor)
		case ProfileANSI256:
			return lipgloss.Color(ansi256)
		default:
			return lipgloss.Color(ansi)
		}
	}
	return Theme{
		Accent:  lipgloss.NewStyle().Foreground(convert(accent, "99", "5")).Bold(true),
		Primary: lipgloss.NewStyle().Foreground(convert(primary, "255", "7")),
		Muted:   lipgloss.NewStyle().Foreground(convert(muted, "245", "8")),
		Success: lipgloss.NewStyle().Foreground(convert("#2EBC78", "42", "2")).Bold(true),
		Warning: lipgloss.NewStyle().Foreground(convert("#D99A2B", "214", "3")).Bold(true),
		Error:   lipgloss.NewStyle().Foreground(convert("#E05263", "203", "1")).Bold(true),
	}
}
