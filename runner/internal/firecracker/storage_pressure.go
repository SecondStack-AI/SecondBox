package firecracker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
)

var (
	ErrStoragePressureProbe            = errors.New("storage pressure probe failed")
	ErrStoragePressureAdmissionDenied  = errors.New("storage pressure admission denied")
	ErrStoragePressureDedicatedStorage = errors.New("storage pressure storage is not dedicated")
)

type storagePressureState string

const (
	storagePressureStateHealthy         storagePressureState = "healthy"
	storagePressureStateWarning         storagePressureState = "warning"
	storagePressureStateAdmissionDenied storagePressureState = "admission_denied"
)

type storagePressurePolicy struct {
	RecoveryPercent      int
	WarningPercent       int
	AdmissionDenyPercent int
}

func (p storagePressurePolicy) Validate() error {
	if p.RecoveryPercent < 1 ||
		p.RecoveryPercent >= p.WarningPercent ||
		p.WarningPercent >= p.AdmissionDenyPercent ||
		p.AdmissionDenyPercent >= 100 {
		return fmt.Errorf(
			"storage pressure thresholds must satisfy 0 < recovery < warning < admission deny < 100",
		)
	}
	return nil
}

type storagePressureSample struct {
	Backend                 string
	TotalBytes              uint64
	UsedBytes               uint64
	MetadataUsedBasisPoints uint64
}

type storagePressureProbe interface {
	Backend() string
	Sample(context.Context) (storagePressureSample, error)
}

type storagePressureController struct {
	mu           sync.Mutex
	policy       storagePressurePolicy
	probe        storagePressureProbe
	state        storagePressureState
	reservations map[string]uint64
	emit         func(context.Context, string) error
}

func newStoragePressureController(
	policy storagePressurePolicy,
	probe storagePressureProbe,
	emit func(context.Context, string) error,
) (*storagePressureController, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if probe == nil {
		return nil, fmt.Errorf("storage pressure probe is required")
	}
	if emit == nil {
		return nil, fmt.Errorf("storage pressure evidence emitter is required")
	}
	return &storagePressureController{
		policy:       policy,
		probe:        probe,
		state:        storagePressureStateHealthy,
		reservations: make(map[string]uint64),
		emit:         emit,
	}, nil
}

func newConfiguredStoragePressureController(
	cfg *config.Config,
	emit func(context.Context, string) error,
) (*storagePressureController, error) {
	if cfg == nil {
		return nil, fmt.Errorf("storage pressure configuration is required")
	}
	policy := storagePressurePolicy{
		RecoveryPercent:      cfg.MicroVMStoragePressureRecoveryPercent,
		WarningPercent:       cfg.MicroVMStoragePressureWarningPercent,
		AdmissionDenyPercent: cfg.MicroVMStoragePressureAdmissionDenyPercent,
	}
	probe := &ext4StoragePressureProbe{workspaceDir: cfg.RunnerWorkspaceRoot}
	return newStoragePressureController(policy, probe, emit)
}

func (c *storagePressureController) Observe(ctx context.Context) (storagePressureState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, err := c.sampleLocked(ctx)
	if err != nil {
		return "", err
	}
	return c.applyObservationLocked(ctx, sample, c.reservedBytesLocked())
}

func (c *storagePressureController) CheckAdmission(ctx context.Context, requestedBytes uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, err := c.sampleLocked(ctx)
	if err != nil {
		return err
	}
	reservedBytes, err := addStorageBytes(c.reservedBytesLocked(), requestedBytes)
	if err != nil {
		return err
	}
	state, err := c.applyObservationLocked(ctx, sample, reservedBytes)
	if err != nil {
		return err
	}
	if state == storagePressureStateAdmissionDenied {
		return fmt.Errorf(
			"%w: backend %s projected utilization reaches the configured denial threshold",
			ErrStoragePressureAdmissionDenied,
			sample.Backend,
		)
	}
	return nil
}

func (c *storagePressureController) Reserve(
	ctx context.Context,
	reservationID string,
	requestedBytes uint64,
) error {
	if strings.TrimSpace(reservationID) == "" || requestedBytes == 0 {
		return fmt.Errorf("storage pressure reservation identity and bytes are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existingBytes, exists := c.reservations[reservationID]; exists {
		if existingBytes == requestedBytes {
			return nil
		}
		return fmt.Errorf("storage pressure reservation %q changed size", reservationID)
	}
	sample, err := c.sampleLocked(ctx)
	if err != nil {
		return err
	}
	reservedBytes, err := addStorageBytes(c.reservedBytesLocked(), requestedBytes)
	if err != nil {
		return err
	}
	state, err := c.applyObservationLocked(ctx, sample, reservedBytes)
	if err != nil {
		return err
	}
	if state == storagePressureStateAdmissionDenied {
		return fmt.Errorf(
			"%w: backend %s projected utilization reaches the configured denial threshold",
			ErrStoragePressureAdmissionDenied,
			sample.Backend,
		)
	}
	c.reservations[reservationID] = requestedBytes
	return nil
}

func (c *storagePressureController) Release(ctx context.Context, reservationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reservations, reservationID)
	sample, err := c.sampleLocked(ctx)
	if err != nil {
		return err
	}
	_, err = c.applyObservationLocked(ctx, sample, c.reservedBytesLocked())
	return err
}

func (c *storagePressureController) ReservedBytes() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reservedBytesLocked()
}

func (c *storagePressureController) reservedBytesLocked() uint64 {
	var total uint64
	for _, bytes := range c.reservations {
		total += bytes
	}
	return total
}

func (c *storagePressureController) sampleLocked(ctx context.Context) (storagePressureSample, error) {
	sample, err := c.probe.Sample(ctx)
	if err != nil {
		evidenceErr := c.emit(ctx, "storage_pressure_probe_failed")
		return storagePressureSample{}, errors.Join(
			fmt.Errorf("%w: backend %s", ErrStoragePressureProbe, c.probe.Backend()),
			err,
			evidenceErr,
		)
	}
	if sample.TotalBytes == 0 || sample.UsedBytes > sample.TotalBytes {
		evidenceErr := c.emit(ctx, "storage_pressure_probe_failed")
		return storagePressureSample{}, errors.Join(
			fmt.Errorf("%w: backend %s returned invalid capacity", ErrStoragePressureProbe, c.probe.Backend()),
			evidenceErr,
		)
	}
	return sample, nil
}

func (c *storagePressureController) applyObservationLocked(
	ctx context.Context,
	sample storagePressureSample,
	reservedBytes uint64,
) (storagePressureState, error) {
	projectedUsedBytes, err := addStorageBytes(sample.UsedBytes, reservedBytes)
	if err != nil {
		return "", err
	}
	usageBasisPoints := percentBasisPoints(projectedUsedBytes, sample.TotalBytes)
	if sample.MetadataUsedBasisPoints > usageBasisPoints {
		usageBasisPoints = sample.MetadataUsedBasisPoints
	}
	recoveryBasisPoints := uint64(c.policy.RecoveryPercent * 100)
	warningBasisPoints := uint64(c.policy.WarningPercent * 100)
	admissionDenyBasisPoints := uint64(c.policy.AdmissionDenyPercent * 100)

	next := c.state
	switch c.state {
	case storagePressureStateHealthy:
		switch {
		case usageBasisPoints >= admissionDenyBasisPoints:
			next = storagePressureStateAdmissionDenied
		case usageBasisPoints >= warningBasisPoints:
			next = storagePressureStateWarning
		}
	case storagePressureStateWarning:
		switch {
		case usageBasisPoints >= admissionDenyBasisPoints:
			next = storagePressureStateAdmissionDenied
		case usageBasisPoints <= recoveryBasisPoints:
			next = storagePressureStateHealthy
		}
	case storagePressureStateAdmissionDenied:
		if usageBasisPoints <= recoveryBasisPoints {
			next = storagePressureStateHealthy
		}
	default:
		return "", fmt.Errorf("%w: unknown state %q", ErrStoragePressureProbe, c.state)
	}
	if next == c.state {
		return next, nil
	}
	previous := c.state
	c.state = next
	terminalKind := ""
	switch next {
	case storagePressureStateWarning:
		terminalKind = "storage_pressure_warning"
	case storagePressureStateAdmissionDenied:
		terminalKind = "storage_pressure_admission_denied"
	case storagePressureStateHealthy:
		if previous != storagePressureStateHealthy {
			terminalKind = "storage_pressure_recovered"
		}
	}
	if terminalKind != "" {
		if err := c.emit(ctx, terminalKind); err != nil {
			return "", fmt.Errorf("emit storage pressure evidence %s: %w", terminalKind, err)
		}
	}
	return next, nil
}

func addStorageBytes(left, right uint64) (uint64, error) {
	if math.MaxUint64-left < right {
		return 0, fmt.Errorf("%w: projected byte count overflows", ErrStoragePressureAdmissionDenied)
	}
	return left + right, nil
}

func percentBasisPoints(usedBytes, totalBytes uint64) uint64 {
	if usedBytes >= totalBytes {
		return 10000
	}
	high, low := bits.Mul64(usedBytes, 10000)
	quotient, remainder := bits.Div64(high, low, totalBytes)
	if remainder != 0 {
		quotient++
	}
	return quotient
}

type ext4StoragePressureProbe struct {
	workspaceDir  string
	backend       string
	forbiddenDirs []string
}

func (p *ext4StoragePressureProbe) Backend() string {
	if p.backend != "" {
		return p.backend
	}
	return "ext4"
}

func (p *ext4StoragePressureProbe) Sample(context.Context) (storagePressureSample, error) {
	if err := validateDedicatedStorageFilesystem(p.workspaceDir, p.forbiddenDirs...); err != nil {
		return storagePressureSample{}, err
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(p.workspaceDir, &filesystem); err != nil {
		return storagePressureSample{}, fmt.Errorf("%w: statfs workspace: %v", ErrStoragePressureProbe, err)
	}
	totalBytes := filesystem.Blocks * uint64(filesystem.Bsize)
	availableBytes := filesystem.Bavail * uint64(filesystem.Bsize)
	if availableBytes > totalBytes {
		return storagePressureSample{}, fmt.Errorf("%w: ext4 available bytes exceed total bytes", ErrStoragePressureProbe)
	}
	return storagePressureSample{
		Backend:    "ext4",
		TotalBytes: totalBytes,
		UsedBytes:  totalBytes - availableBytes,
	}, nil
}

func validateDedicatedStorageFilesystem(workspaceDir string, forbiddenDirs ...string) error {
	workspaceInfo, err := os.Lstat(workspaceDir)
	if err != nil {
		return fmt.Errorf("%w: inspect workspace: %v", ErrStoragePressureProbe, err)
	}
	if workspaceInfo.Mode()&os.ModeSymlink != 0 || !workspaceInfo.IsDir() {
		return fmt.Errorf(
			"%w: workspace must be a non-symbolic-link directory",
			ErrStoragePressureDedicatedStorage,
		)
	}
	rootInfo, err := os.Stat("/")
	if err != nil {
		return fmt.Errorf("%w: inspect host root: %v", ErrStoragePressureProbe, err)
	}
	workspaceStat, workspaceOK := workspaceInfo.Sys().(*syscall.Stat_t)
	rootStat, rootOK := rootInfo.Sys().(*syscall.Stat_t)
	if !workspaceOK || !rootOK {
		return fmt.Errorf("%w: filesystem device identity is unavailable", ErrStoragePressureProbe)
	}
	if workspaceStat.Dev == rootStat.Dev {
		return fmt.Errorf(
			"%w: workspace %q shares the host root filesystem",
			ErrStoragePressureDedicatedStorage,
			workspaceDir,
		)
	}
	for _, forbiddenDir := range forbiddenDirs {
		forbiddenInfo, err := os.Stat(forbiddenDir)
		if err != nil {
			return fmt.Errorf("%w: inspect forbidden storage %q: %v", ErrStoragePressureProbe, forbiddenDir, err)
		}
		forbiddenStat, ok := forbiddenInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%w: forbidden filesystem device identity is unavailable", ErrStoragePressureProbe)
		}
		if workspaceStat.Dev == forbiddenStat.Dev {
			return fmt.Errorf(
				"%w: storage %q shares filesystem with %q",
				ErrStoragePressureDedicatedStorage,
				workspaceDir,
				forbiddenDir,
			)
		}
	}
	return nil
}
