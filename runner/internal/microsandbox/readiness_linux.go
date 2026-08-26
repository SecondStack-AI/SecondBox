//go:build linux

package microsandbox

import (
	"context"
	"fmt"
	"os"
)

func platformReadiness(_ context.Context, _ validatedConfig) error {
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("SecondBox Microsandbox readiness KVM: %w", err)
	}
	if err := kvm.Close(); err != nil {
		return fmt.Errorf("SecondBox Microsandbox readiness close KVM: %w", err)
	}
	return nil
}
