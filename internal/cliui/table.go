package cliui

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

type Column struct {
	Key      string
	Title    string
	Priority int
	MinWidth int
}

type Row map[string]string

type Table struct {
	Columns            []Column
	Rows               []Row
	Empty              string
	ContinuationCursor string
}

func (renderer Renderer) WriteTable(table Table) error {
	width := renderer.Capabilities.Output.Width
	if width <= 0 {
		width = 80
	}
	text := renderTable(table, width, renderer.Capabilities.Unicode)
	if table.ContinuationCursor != "" {
		text += "Next cursor: " + Sanitize(table.ContinuationCursor) + "\n"
	}
	if renderer.StyledOutput() && text != "" {
		theme := NewTheme(renderer.Capabilities.Output, true)
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		for index, line := range lines {
			if width >= 48 && index == 0 {
				lines[index] = theme.Primary.Bold(true).Render(line)
				continue
			}
			if width < 48 {
				key, value, found := strings.Cut(line, ": ")
				if found {
					lines[index] = theme.Accent.Render(key+":") + " " + theme.Primary.Render(value)
				}
			}
		}
		text = strings.Join(lines, "\n") + "\n"
	}
	_, err := io.WriteString(renderer.Output, text)
	return err
}

func renderTable(table Table, width int, unicodeOK bool) string {
	if len(table.Rows) == 0 {
		if table.Empty == "" {
			return ""
		}
		return Sanitize(table.Empty) + "\n"
	}
	columns := append([]Column(nil), table.Columns...)
	sort.SliceStable(columns, func(i, j int) bool { return columns[i].Priority < columns[j].Priority })
	if width < 48 || len(columns) == 0 {
		return renderStacked(columns, table.Rows)
	}
	widths := make([]int, len(columns))
	for index, column := range columns {
		widths[index] = max(column.MinWidth, lipgloss.Width(column.Title))
		for _, row := range table.Rows {
			widths[index] = max(widths[index], lipgloss.Width(Sanitize(row[column.Key])))
		}
	}
	for totalWidth(widths) > width {
		changed := false
		for index := len(widths) - 1; index >= 0 && totalWidth(widths) > width; index-- {
			minimum := max(columns[index].MinWidth, 4)
			if widths[index] > minimum {
				widths[index]--
				changed = true
			}
		}
		if !changed {
			return renderStacked(columns, table.Rows)
		}
	}
	var result strings.Builder
	for index, column := range columns {
		if index > 0 {
			result.WriteString("  ")
		}
		result.WriteString(pad(truncate(Sanitize(column.Title), widths[index], unicodeOK), widths[index]))
	}
	result.WriteByte('\n')
	for _, row := range table.Rows {
		for index, column := range columns {
			if index > 0 {
				result.WriteString("  ")
			}
			result.WriteString(pad(truncate(Sanitize(row[column.Key]), widths[index], unicodeOK), widths[index]))
		}
		result.WriteByte('\n')
	}
	lines := strings.Split(result.String(), "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	return strings.Join(lines, "\n")
}

func renderStacked(columns []Column, rows []Row) string {
	var result strings.Builder
	for rowIndex, row := range rows {
		if rowIndex > 0 {
			result.WriteByte('\n')
		}
		for _, column := range columns {
			fmt.Fprintf(&result, "%s: %s\n", Sanitize(column.Title), Sanitize(row[column.Key]))
		}
	}
	return result.String()
}

func totalWidth(widths []int) int {
	total := max(0, (len(widths)-1)*2)
	for _, width := range widths {
		total += width
	}
	return total
}
func pad(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}
func truncate(value string, width int, unicodeOK bool) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	marker := "..."
	if unicodeOK {
		marker = "…"
	}
	limit := width - lipgloss.Width(marker)
	if limit <= 0 {
		return marker[:min(len(marker), width)]
	}
	var result strings.Builder
	for _, r := range value {
		if lipgloss.Width(result.String()+string(r)) > limit {
			break
		}
		result.WriteRune(r)
	}
	return result.String() + marker
}
