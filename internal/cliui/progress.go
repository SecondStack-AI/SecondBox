package cliui

import (
	"context"
	"fmt"
	"sync"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

type activityStopMsg struct{}

type activityModel struct {
	spinner spinner.Model
	name    string
}

func (model activityModel) Init() tea.Cmd { return model.spinner.Tick }
func (model activityModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case activityStopMsg:
		return model, tea.Quit
	case spinner.TickMsg:
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(message)
		return model, command
	default:
		return model, nil
	}
}
func (model activityModel) View() tea.View {
	return tea.NewView(model.spinner.View() + " " + Sanitize(model.name))
}

// Activity is a bounded lifecycle indicator. Complete must be called before
// any guest or subprocess stream is forwarded.
type Activity struct {
	renderer Renderer
	name     string
	program  *tea.Program
	done     chan error
	finished chan struct{}
	plain    bool
	once     sync.Once
}

func (renderer Renderer) StartActivity(ctx context.Context, name string) (*Activity, error) {
	if renderer.Diagnostic == nil {
		return nil, fmt.Errorf("SecondBox CLI progress requires a diagnostic handle")
	}
	activity := &Activity{renderer: renderer, name: Sanitize(name), done: make(chan error, 1), finished: make(chan struct{})}
	if !renderer.Capabilities.Diagnostic.TTY || !renderer.StyledDiagnostic() {
		activity.plain = true
		if err := renderer.WritePhases([]Phase{{Name: name, Status: StatusActive}}); err != nil {
			return nil, err
		}
		return activity, nil
	}
	theme := NewTheme(renderer.Capabilities.Diagnostic, true)
	model := activityModel{spinner: spinner.New(spinner.WithSpinner(spinner.Line), spinner.WithStyle(theme.Accent)), name: name}
	activity.program = tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(renderer.Diagnostic), tea.WithoutSignalHandler())
	go func() { _, err := activity.program.Run(); activity.done <- err; close(activity.finished) }()
	go func() {
		select {
		case <-ctx.Done():
			activity.program.Send(activityStopMsg{})
		case <-activity.finished:
		}
	}()
	return activity, nil
}

func (activity *Activity) Complete(status Status, detail string) error {
	var result error
	activity.once.Do(func() {
		if activity.program != nil {
			activity.program.Send(activityStopMsg{})
			result = <-activity.done
		}
		if phaseErr := activity.renderer.WritePhases([]Phase{{Name: activity.name, Detail: detail, Status: status}}); result == nil {
			result = phaseErr
		}
	})
	return result
}

// WriteDeterminate writes one bounded progress snapshot. TTY diagnostics use
// the Bubbles progress renderer; redirected diagnostics receive plain counts.
func (renderer Renderer) WriteDeterminate(label string, current, total int64) error {
	if total <= 0 || current < 0 || current > total {
		return fmt.Errorf("SecondBox CLI progress requires 0 <= current <= total")
	}
	if renderer.Capabilities.Diagnostic.TTY && renderer.StyledDiagnostic() {
		width := renderer.Capabilities.Diagnostic.Width - len(label) - 8
		if width < 10 {
			width = 10
		}
		model := progress.New(progress.WithWidth(width), progress.WithDefaultBlend())
		_, err := fmt.Fprintf(renderer.Diagnostic, "%s %s\n", Sanitize(label), model.ViewAs(float64(current)/float64(total)))
		return err
	}
	_, err := fmt.Fprintf(renderer.Diagnostic, "%s: %d/%d\n", Sanitize(label), current, total)
	return err
}
