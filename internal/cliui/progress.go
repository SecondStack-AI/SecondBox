package cliui

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"
)

const activityFrameInterval = 80 * time.Millisecond

var unicodeActivityFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var asciiActivityFrames = [...]string{"|", "/", "-", "\\"}

// Activity is a bounded lifecycle indicator. Complete must be called before
// any guest or subprocess stream is forwarded.
type Activity struct {
	renderer Renderer
	name     string
	done     chan error
	finished chan struct{}
	stop     chan struct{}
	animated bool
	once     sync.Once
	stopOnce sync.Once
}

func (renderer Renderer) StartActivity(ctx context.Context, name string) (*Activity, error) {
	if renderer.Diagnostic == nil {
		return nil, fmt.Errorf("SecondBox CLI progress requires a diagnostic handle")
	}
	activity := &Activity{renderer: renderer, name: Sanitize(name), done: make(chan error, 1), finished: make(chan struct{})}
	if !renderer.Capabilities.Diagnostic.TTY || !renderer.StyledDiagnostic() {
		if err := renderer.WritePhases([]Phase{{Name: name, Status: StatusActive}}); err != nil {
			return nil, err
		}
		return activity, nil
	}
	activity.animated = true
	activity.stop = make(chan struct{})
	go activity.animate()
	go func() {
		select {
		case <-ctx.Done():
			activity.stopAnimation()
		case <-activity.finished:
		}
	}()
	return activity, nil
}

func (activity *Activity) Complete(status Status, detail string) error {
	var result error
	activity.once.Do(func() {
		if activity.animated {
			activity.stopAnimation()
			result = <-activity.done
		}
		if phaseErr := activity.renderer.WritePhases([]Phase{{Name: activity.name, Detail: detail, Status: status}}); result == nil {
			result = phaseErr
		}
	})
	return result
}

func (activity *Activity) stopAnimation() {
	activity.stopOnce.Do(func() { close(activity.stop) })
}

func (activity *Activity) animate() {
	defer close(activity.finished)
	ticker := time.NewTicker(activityFrameInterval)
	defer ticker.Stop()
	frames := unicodeActivityFrames[:]
	if !activity.renderer.Capabilities.Unicode {
		frames = asciiActivityFrames[:]
	}
	theme := NewTheme(activity.renderer.Capabilities.Diagnostic, true)
	frame, previousWidth := 0, 1
	for {
		width := activity.currentWidth()
		line := activityLine(theme, frames[frame], activity.name, width, activity.renderer.Capabilities.Unicode)
		if err := clearActivityRows(activity.renderer.Diagnostic, previousWidth, width); err != nil {
			activity.done <- err
			return
		}
		if _, err := io.WriteString(activity.renderer.Diagnostic, line); err != nil {
			activity.done <- err
			return
		}
		previousWidth = lipgloss.Width(line)
		select {
		case <-ticker.C:
			frame = (frame + 1) % len(frames)
		case <-activity.stop:
			err := clearActivityRows(activity.renderer.Diagnostic, previousWidth, activity.currentWidth())
			activity.done <- err
			return
		}
	}
}

func (activity *Activity) currentWidth() int {
	width := activity.renderer.Capabilities.Diagnostic.Width
	if output, ok := activity.renderer.Diagnostic.(interface{ Fd() uintptr }); ok {
		if current, _, err := term.GetSize(int(output.Fd())); err == nil && current > 0 {
			width = current
		}
	}
	return max(1, width)
}

func activityLine(theme Theme, frame, name string, width int, unicodeOK bool) string {
	// Leave the last column unused so terminals do not enter pending-wrap state.
	limit := max(1, width-1)
	if limit == 1 {
		return theme.Accent.Render(frame)
	}
	return theme.Accent.Render(frame) + " " + truncate(name, limit-2, unicodeOK)
}

func clearActivityRows(output io.Writer, previousWidth, currentWidth int) error {
	rows := (max(1, previousWidth) + max(1, currentWidth) - 1) / max(1, currentWidth)
	if _, err := io.WriteString(output, "\r\x1b[2K"); err != nil {
		return err
	}
	for row := 1; row < rows; row++ {
		if _, err := io.WriteString(output, "\x1b[1A\r\x1b[2K"); err != nil {
			return err
		}
	}
	return nil
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
