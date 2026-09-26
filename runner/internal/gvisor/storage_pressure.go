//go:build linux

package gvisor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sync"
	"syscall"
)

var errPhysicalStoragePressure = errors.New("SecondBox gVisor workspace storage admission denied")

// physicalStoragePressure checks the filesystem visible through the Workspace
// mount. Logical Workspace capacities do not contribute to this measurement.
type physicalStoragePressure struct {
	mu       sync.Mutex
	root     string
	recovery uint64
	warning  uint64
	deny     uint64
	state    string
	probe    func(string) (uint64, uint64, error)
}

func newPhysicalStoragePressure(config Config) (*physicalStoragePressure, error) {
	if config.StorageRecoveryPercent < 1 ||
		config.StorageRecoveryPercent >= config.StorageWarningPercent ||
		config.StorageWarningPercent >= config.StorageDenyPercent ||
		config.StorageDenyPercent >= 100 {
		return nil, fmt.Errorf("SecondBox gVisor storage pressure thresholds must satisfy 0 < recovery < warning < admission deny < 100")
	}
	return &physicalStoragePressure{
		root: config.WorkspaceRoot, recovery: uint64(config.StorageRecoveryPercent * 100),
		warning: uint64(config.StorageWarningPercent * 100), deny: uint64(config.StorageDenyPercent * 100),
		state: "healthy", probe: probeWorkspaceFilesystem,
	}, nil
}

func probeWorkspaceFilesystem(root string) (uint64, uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return 0, 0, fmt.Errorf("SecondBox gVisor workspace filesystem probe: %w", err)
	}
	if fs.Bsize <= 0 || fs.Blocks > math.MaxUint64/uint64(fs.Bsize) || fs.Bavail > fs.Blocks {
		return 0, 0, fmt.Errorf("SecondBox gVisor workspace filesystem probe returned invalid capacity")
	}
	total := fs.Blocks * uint64(fs.Bsize)
	available := fs.Bavail * uint64(fs.Bsize)
	if total == 0 || available > total {
		return 0, 0, fmt.Errorf("SecondBox gVisor workspace filesystem probe returned invalid capacity")
	}
	return total - available, total, nil
}

func (pressure *physicalStoragePressure) observe(context.Context) (string, error) {
	pressure.mu.Lock()
	defer pressure.mu.Unlock()
	used, total, err := pressure.probe(pressure.root)
	if err != nil {
		return "unavailable", err
	}
	if total == 0 || used > total {
		return "unavailable", fmt.Errorf("SecondBox gVisor workspace filesystem probe returned invalid capacity")
	}
	high, low := bits.Mul64(used, 10000)
	basisPoints, remainder := bits.Div64(high, low, total)
	if remainder != 0 {
		basisPoints++
	}
	switch pressure.state {
	case "healthy", "warning":
		if basisPoints >= pressure.deny {
			pressure.state = "admission_denied"
		} else if basisPoints >= pressure.warning {
			pressure.state = "warning"
		} else if basisPoints <= pressure.recovery {
			pressure.state = "healthy"
		}
	case "admission_denied":
		if basisPoints <= pressure.recovery {
			pressure.state = "healthy"
		}
	default:
		return "unavailable", fmt.Errorf("SecondBox gVisor workspace storage pressure state %q is invalid", pressure.state)
	}
	return pressure.state, nil
}

func (pressure *physicalStoragePressure) admit(ctx context.Context) error {
	state, err := pressure.observe(ctx)
	if err != nil {
		return err
	}
	if state == "admission_denied" {
		return errPhysicalStoragePressure
	}
	return nil
}
