//go:build darwin

package runtimeconfig

import (
	"errors"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

func validatePlatformBackendKind(kind string) error {
	if kind != "microsandbox" {
		return fmt.Errorf("SECONDBOX_COMPUTE_BACKEND must be microsandbox on Darwin")
	}
	return nil
}

func loadPlatformBackendComposition(
	Composition,
	*runnercontrol.GRPCConnector,
) (Composition, error) {
	return Composition{}, errors.New("SecondBox Darwin runner cannot load Firecracker composition")
}
