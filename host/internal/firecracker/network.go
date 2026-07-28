package microvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"agent-manager/internal/egressproxy"
)

type SourceBindingRegistrar interface {
	Register(egressproxy.SourceBinding) error
	UnregisterContainer(containerID string)
	RetainContainers(containerIDs []string) error
}

type HostNetworkConfigurer interface {
	ConfigureTap(context.Context, TapConfig) error
	RemoveTap(context.Context, string) error
}

type TapConfig struct {
	AgentID    string
	InstanceID string
	TapName    string
	GuestIP    string
	BridgeName string
	BridgeCIDR string
	OwnerUID   int
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type IPTapConfigurer struct {
	run commandRunner
}

func (c IPTapConfigurer) ConfigureTap(ctx context.Context, cfg TapConfig) error {
	tap := strings.TrimSpace(cfg.TapName)
	if tap == "" {
		return fmt.Errorf("tap name is required")
	}
	if bridge := strings.TrimSpace(cfg.BridgeName); bridge != "" {
		if err := c.ensureBridge(ctx, bridge, strings.TrimSpace(cfg.BridgeCIDR)); err != nil {
			return err
		}
	}
	ownerUID := cfg.OwnerUID
	if ownerUID < 0 {
		return fmt.Errorf("tap owner uid must be non-negative")
	}
	if ownerUID == 0 {
		ownerUID = os.Getuid()
	}
	uid := fmt.Sprintf("%d", ownerUID)
	commands := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", uid, "vnet_hdr"},
	}
	if bridge := strings.TrimSpace(cfg.BridgeName); bridge != "" {
		commands = append(commands, []string{"ip", "link", "set", tap, "master", bridge})
	}
	commands = append(commands, []string{"ip", "link", "set", tap, "up"})
	for _, args := range commands {
		if out, err := c.command(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (c IPTapConfigurer) ensureBridge(ctx context.Context, bridge, cidr string) error {
	if _, err := c.command(ctx, "ip", "link", "show", bridge); err != nil {
		if out, createErr := c.command(ctx, "ip", "link", "add", "name", bridge, "type", "bridge"); createErr != nil {
			if !strings.Contains(string(out), "File exists") {
				return fmt.Errorf("create bridge %s: %w: %s", bridge, createErr, strings.TrimSpace(string(out)))
			}
		}
	}
	if cidr != "" {
		out, err := c.command(ctx, "ip", "addr", "show", "dev", bridge)
		if err != nil {
			return fmt.Errorf("inspect bridge address %s: %w: %s", bridge, err, strings.TrimSpace(string(out)))
		}
		if !strings.Contains(string(out), cidr) {
			if out, err := c.command(ctx, "ip", "addr", "add", cidr, "dev", bridge); err != nil && !strings.Contains(string(out), "File exists") {
				return fmt.Errorf("assign bridge address %s %s: %w: %s", bridge, cidr, err, strings.TrimSpace(string(out)))
			}
		}
	}
	if out, err := c.command(ctx, "ip", "link", "set", bridge, "up"); err != nil {
		return fmt.Errorf("bring bridge up %s: %w: %s", bridge, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c IPTapConfigurer) RemoveTap(ctx context.Context, tapName string) error {
	tap := strings.TrimSpace(tapName)
	if tap == "" {
		return nil
	}
	if out, err := c.command(ctx, "ip", "link", "delete", tap); err != nil {
		if strings.Contains(string(out), "Cannot find device") || strings.Contains(string(out), "does not exist") {
			return nil
		}
		return fmt.Errorf("ip link delete %s: %w: %s", tap, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c IPTapConfigurer) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c.run != nil {
		return c.run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
