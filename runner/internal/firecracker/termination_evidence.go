package firecracker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const successfulGuestExitMessage = "Firecracker exited successfully"

type observedTerminationReason string

const (
	observedTerminationGuestShutdown      observedTerminationReason = "guest_shutdown"
	observedTerminationResourceExhaustion observedTerminationReason = "resource_exhaustion"
	observedTerminationInternalFailure    observedTerminationReason = "internal_failure"
)

// InstanceTerminalObservation is bounded, assignment-correlated evidence that a
// ready Firecracker instance disappeared without an explicit Runner stop.
type InstanceTerminalObservation struct {
	BackendReference string
	Reason           string
	ObservedAt       time.Time
	EvidenceDigest   string
}

type terminationEvidence struct {
	ready               bool
	explicitStop        bool
	baselineOOMKills    *uint64
	observedOOMKills    *uint64
	successfulGuestExit bool
	evidenceErr         error
}

var readTerminationEvidenceFile = os.ReadFile

func parseOOMKillCounter(data []byte) (uint64, error) {
	var (
		value uint64
		found bool
	)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		if found {
			return 0, errors.New("duplicate oom_kill counter")
		}
		parsed, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse oom_kill counter: %w", err)
		}
		value = parsed
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan cgroup memory evidence: %w", err)
	}
	if !found {
		return 0, errors.New("oom_kill counter is absent")
	}
	return value, nil
}

func hasExactSuccessfulGuestExitMarker(reader io.Reader) bool {
	found, err := scanExactSuccessfulGuestExitMarker(reader)
	return err == nil && found
}

func scanExactSuccessfulGuestExitMarker(reader io.Reader) (bool, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		timestamp, remainder, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if _, err := time.Parse("2006-01-02T15:04:05.999999999", timestamp); err != nil {
			continue
		}
		closeBracket := strings.IndexByte(remainder, ']')
		if closeBracket < 2 || closeBracket+2 > len(remainder) ||
			remainder[0] != '[' || remainder[closeBracket+1] != ' ' {
			continue
		}
		if remainder[closeBracket+2:] == successfulGuestExitMessage {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan Firecracker terminal log: %w", err)
	}
	return false, nil
}

func classifyPostReadyTermination(
	evidence terminationEvidence,
) (observedTerminationReason, bool) {
	if !evidence.ready || evidence.explicitStop {
		return "", false
	}
	if evidence.evidenceErr != nil ||
		evidence.baselineOOMKills == nil ||
		evidence.observedOOMKills == nil ||
		*evidence.observedOOMKills < *evidence.baselineOOMKills {
		return observedTerminationInternalFailure, true
	}
	if *evidence.observedOOMKills > *evidence.baselineOOMKills {
		return observedTerminationResourceExhaustion, true
	}
	if evidence.successfulGuestExit {
		return observedTerminationGuestShutdown, true
	}
	return observedTerminationInternalFailure, true
}

func (m *Manager) oomKillCounterPath(instanceID string) (string, error) {
	if m == nil || m.cfg == nil || strings.TrimSpace(instanceID) == "" {
		return "", errors.New("Firecracker cgroup evidence identity is incomplete")
	}
	parent := strings.Trim(strings.TrimSpace(m.cfg.MicroVMJailerParentCgroup), "/")
	if parent == "" || parent == "." || strings.Contains(parent, "..") {
		return "", errors.New("Firecracker parent cgroup is invalid")
	}
	switch m.cfg.MicroVMJailerCgroupVersion {
	case 1:
		return filepath.Join(
			"/sys/fs/cgroup/memory", parent, instanceID, "memory.oom_control",
		), nil
	case 2:
		return filepath.Join(
			"/sys/fs/cgroup", parent, instanceID, "memory.events.local",
		), nil
	default:
		return "", errors.New("Firecracker cgroup version is unsupported")
	}
}

func (m *Manager) readOOMKillCounter(instanceID string) (uint64, error) {
	path, err := m.oomKillCounterPath(instanceID)
	if err != nil {
		return 0, err
	}
	data, err := readTerminationEvidenceFile(path)
	if err != nil {
		return 0, fmt.Errorf("read Firecracker cgroup OOM evidence %q: %w", path, err)
	}
	return parseOOMKillCounter(data)
}

// MarkAssignmentReady establishes the post-ready evidence baseline only after
// the Runner has emitted the authoritative ready AssignmentResult.
func (m *Manager) MarkAssignmentReady(
	instanceID string,
	observer func(context.Context, InstanceTerminalObservation) error,
) error {
	if observer == nil {
		return errors.New("Firecracker terminal observer is required")
	}
	baseline, baselineErr := m.readOOMKillCounter(instanceID)
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[instanceID]
	if inst == nil {
		return errors.New("Firecracker instance disappeared before ready authority")
	}
	inst.ready = true
	inst.terminalObserver = observer
	if baselineErr != nil {
		inst.terminationEvidenceErr = baselineErr
		return nil
	}
	inst.baselineOOMKills = &baseline
	return nil
}

func (m *Manager) observeNaturalTermination(inst *instance) error {
	m.mu.Lock()
	evidence := terminationEvidence{
		ready:            inst.ready,
		explicitStop:     inst.explicitStop,
		baselineOOMKills: inst.baselineOOMKills,
		evidenceErr:      inst.terminationEvidenceErr,
	}
	observer := inst.terminalObserver
	m.mu.Unlock()
	if !evidence.ready || evidence.explicitStop {
		return nil
	}
	observedOOMKills, err := m.readOOMKillCounter(inst.id)
	if err != nil {
		evidence.evidenceErr = errors.Join(evidence.evidenceErr, err)
	} else {
		evidence.observedOOMKills = &observedOOMKills
	}
	log, err := os.Open(inst.logPath)
	if err != nil {
		evidence.evidenceErr = errors.Join(evidence.evidenceErr, fmt.Errorf("open Firecracker terminal log: %w", err))
	} else {
		evidence.successfulGuestExit, err = scanExactSuccessfulGuestExitMarker(log)
		closeErr := log.Close()
		evidence.evidenceErr = errors.Join(evidence.evidenceErr, err, closeErr)
	}
	reason, emit := classifyPostReadyTermination(evidence)
	if !emit {
		return nil
	}
	if observer == nil {
		return errors.New("Firecracker ready instance lacks terminal observer")
	}
	observedAt := time.Now().UTC()
	digestInput := strings.Join([]string{
		"secondbox-instance-terminal-v1",
		inst.id,
		inst.assignmentID,
		string(reason),
		formatCounter(evidence.baselineOOMKills),
		formatCounter(evidence.observedOOMKills),
		strconv.FormatBool(evidence.successfulGuestExit),
	}, "\x00")
	digest := sha256.Sum256([]byte(digestInput))
	return observer(context.Background(), InstanceTerminalObservation{
		BackendReference: inst.id,
		Reason:           string(reason),
		ObservedAt:       observedAt,
		EvidenceDigest:   "sha256:" + hex.EncodeToString(digest[:]),
	})
}

func formatCounter(counter *uint64) string {
	if counter == nil {
		return "unavailable"
	}
	return strconv.FormatUint(*counter, 10)
}
