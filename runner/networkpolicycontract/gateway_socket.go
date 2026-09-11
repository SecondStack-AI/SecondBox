package networkpolicycontract

import (
	"fmt"
	"path"
	"strings"
)

func ValidateAttributedGatewaySocket(socketPath string) error {
	if !path.IsAbs(socketPath) || path.Clean(socketPath) != socketPath || socketPath == "/" || len(socketPath) > 107 || strings.ContainsAny(socketPath, "\x00\r\n") {
		return fmt.Errorf("attributed gateway socket must be a canonical absolute Unix socket path of at most 107 bytes")
	}
	return nil
}
