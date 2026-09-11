package runtimemanager

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

type AttributedExecutionNetwork struct {
	Attribution        egressattribution.ExecutionAttribution
	GatewaySocket      string
	MaximumConnections int
	CompileOptions     networkpolicy.CompileOptions
}

// AttributedExecutionGuard belongs to the host Instance, not a guest connection.
// Reconnecting cannot restore its single exec allowance. Nil means ordinary execution.
type AttributedExecutionGuard struct {
	assignmentID string
	expiresAt    time.Time
	execClaimed  atomic.Bool
}

func NewAttributedExecutionGuard(assignmentID string, expiresAt time.Time) (*AttributedExecutionGuard, error) {
	if strings.TrimSpace(assignmentID) == "" || !expiresAt.After(time.Now()) {
		return nil, fmt.Errorf("attributed execution requires an assignment and future expiry")
	}
	return &AttributedExecutionGuard{assignmentID: assignmentID, expiresAt: expiresAt}, nil
}

func (guard *AttributedExecutionGuard) AdmitRead(assignmentID string) error {
	if guard == nil {
		return nil
	}
	if assignmentID != guard.assignmentID || !time.Now().Before(guard.expiresAt) {
		return fmt.Errorf("attributed execution assignment is mismatched or expired")
	}
	return nil
}

func (guard *AttributedExecutionGuard) AdmitExec(assignmentID string, deadline time.Time) error {
	if guard == nil {
		return nil
	}
	if err := guard.AdmitRead(assignmentID); err != nil {
		return err
	}
	if !deadline.After(time.Now()) || deadline.After(guard.expiresAt) {
		return fmt.Errorf("attributed execution deadline exceeds the live assignment")
	}
	if !guard.execClaimed.CompareAndSwap(false, true) {
		return fmt.Errorf("attributed execution already admitted its single exec")
	}
	return nil
}
