package firecracker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIPTapConfigurerCreatesBridgeAndTap(t *testing.T) {
	var calls []string
	cfg := IPTapConfigurer{
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			calls = append(calls, call)
			switch call {
			case "ip link show agbr0":
				return []byte("missing"), errors.New("missing")
			case "ip addr show dev agbr0":
				return []byte("2: agbr0: <BROADCAST>"), nil
			default:
				return nil, nil
			}
		},
	}
	err := cfg.ConfigureTap(context.Background(), TapConfig{
		TapName:    "agfc123",
		BridgeName: "agbr0",
		BridgeCIDR: "172.30.0.1/24",
		OwnerUID:   1234,
	})
	if err != nil {
		t.Fatalf("configure tap: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"ip link add name agbr0 type bridge",
		"ip addr add 172.30.0.1/24 dev agbr0",
		"ip link set agbr0 up",
		"ip tuntap add dev agfc123 mode tap user 1234",
		"vnet_hdr",
		"ip link set agfc123 master agbr0",
		"ip link set agfc123 up",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands:\n%s\nmissing %q", joined, want)
		}
	}
}
