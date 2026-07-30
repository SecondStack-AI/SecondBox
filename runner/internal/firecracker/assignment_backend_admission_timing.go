package firecracker

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// assignmentSequence orders backend entries so a serialised command loop is
// visible directly: if assignments are admitted one at a time, consecutive
// sequence numbers arrive spaced by roughly one full start.
var assignmentSequence atomic.Int64

// assignmentAdmissionTimer records how long each admission phase takes inside
// the backend, and when the backend was entered at all.
//
// The runner-reported admission stage is emitted before the backend is called,
// so a large admission time can mean either queueing ahead of this call or slow
// work inside it. enteredAtUnixMs distinguishes the two.
type assignmentAdmissionTimer struct {
	assignmentID string
	sandboxID    string
	sequence     int64
	start        time.Time
	last         time.Time
}

func newAssignmentAdmissionTimer(assignmentID, sandboxID string) *assignmentAdmissionTimer {
	now := time.Now()
	timer := &assignmentAdmissionTimer{
		assignmentID: assignmentID,
		sandboxID:    sandboxID,
		sequence:     assignmentSequence.Add(1),
		start:        now,
		last:         now,
	}
	slog.Info(
		"assignment admission entered backend",
		"assignment", assignmentID,
		"sandbox", sandboxID,
		"sequence", timer.sequence,
		"enteredAtUnixMs", now.UnixMilli(),
	)
	return timer
}

func (t *assignmentAdmissionTimer) mark(phase string, attrs ...any) {
	if t == nil {
		return
	}
	now := time.Now()
	args := make([]any, 0, 12+len(attrs))
	args = append(args,
		"assignment", t.assignmentID,
		"sandbox", t.sandboxID,
		"sequence", t.sequence,
		"phase", phase,
		"phaseMs", now.Sub(t.last).Milliseconds(),
		"elapsedMs", now.Sub(t.start).Milliseconds(),
	)
	args = append(args, attrs...)
	slog.Info("assignment admission phase", args...)
	t.last = now
}
