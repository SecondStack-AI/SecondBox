package microvmguest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// AssignmentNetworkIdentity is the per-Instance network identity a resumed guest
// installs during its one assignment bind.
//
// It exists because of what the pinned VMM can and cannot do. Firecracker's
// snapshot load can rebind a snapshotted interface to a different host TAP, but
// it cannot add an interface the VM state never recorded, and its network
// override carries no guest MAC. A template is therefore captured with its
// interface present, link down, and address-less, and everything that makes that
// interface this Sandbox's arrives here.
//
// It carries exactly what the cold-boot kernel `ip=` argument configures and
// nothing more: address, prefix length, and an on-link default gateway. That
// argument's trailing nameserver field reaches only /proc/net/pnp, which no
// resolver in the guest reads, so there is no resolver identity to reproduce.
type AssignmentNetworkIdentity struct {
	// Interface is the guest-visible name of the snapshotted interface. It is a
	// template compatibility-key constant, not a per-Instance value.
	Interface string `json:"interface"`
	// MACAddress must be unique per Instance. Resumed Instances share one host
	// bridge, so two guests carrying the template's captured MAC would make the
	// bridge forwarding database flap between their ports.
	MACAddress string `json:"macAddress"`
	// AddressCIDR is this Instance's guest address and prefix length. The host's
	// per-Instance policy drops every frame from the TAP whose source address is
	// not this one.
	AddressCIDR string `json:"addressCidr"`
	// Gateway is the host bridge address. It must be on-link within AddressCIDR.
	Gateway string `json:"gateway"`
}

// resolvedNetworkIdentity is the validated, parsed form the installer works
// from. Parsing happens once, before any interface is touched, so an incomplete
// identity cannot leave a half-configured link behind.
type resolvedNetworkIdentity struct {
	name       string
	hardware   net.HardwareAddr
	address    netip.Addr
	prefixBits int
	broadcast  netip.Addr
	gateway    netip.Addr
}

const maxGuestInterfaceNameLength = 15

func (identity AssignmentNetworkIdentity) resolve() (resolvedNetworkIdentity, error) {
	var resolved resolvedNetworkIdentity
	resolved.name = strings.TrimSpace(identity.Interface)
	if resolved.name == "" || len(resolved.name) > maxGuestInterfaceNameLength {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network interface name %q must be 1 through %d characters",
			identity.Interface,
			maxGuestInterfaceNameLength,
		)
	}
	hardware, err := net.ParseMAC(strings.TrimSpace(identity.MACAddress))
	if err != nil {
		return resolvedNetworkIdentity{}, fmt.Errorf("parse assignment network MAC address: %w", err)
	}
	if len(hardware) != 6 {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network MAC address %q must be 48-bit",
			identity.MACAddress,
		)
	}
	if hardware[0]&0x01 != 0 {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network MAC address %q is a group address",
			identity.MACAddress,
		)
	}
	resolved.hardware = hardware
	prefix, err := netip.ParsePrefix(strings.TrimSpace(identity.AddressCIDR))
	if err != nil {
		return resolvedNetworkIdentity{}, fmt.Errorf("parse assignment network address: %w", err)
	}
	resolved.address = prefix.Addr()
	if !resolved.address.Is4() {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network address %q must be IPv4",
			identity.AddressCIDR,
		)
	}
	resolved.prefixBits = prefix.Bits()
	if resolved.prefixBits < 1 || resolved.prefixBits > 30 {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network prefix length %d must be 1 through 30",
			resolved.prefixBits,
		)
	}
	resolved.broadcast = broadcastAddressOf(prefix)
	gateway, err := netip.ParseAddr(strings.TrimSpace(identity.Gateway))
	if err != nil {
		return resolvedNetworkIdentity{}, fmt.Errorf("parse assignment network gateway: %w", err)
	}
	if !gateway.Is4() {
		return resolvedNetworkIdentity{}, fmt.Errorf("assignment network gateway %q must be IPv4", identity.Gateway)
	}
	// The default route is installed through a directly connected gateway,
	// exactly as kernel IP autoconfiguration installs it on cold boot. An
	// off-link gateway would need a second route, so it is refused rather than
	// silently ignored.
	if !prefix.Masked().Contains(gateway) {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network gateway %s is not on-link within %s",
			gateway,
			prefix,
		)
	}
	if gateway == resolved.address {
		return resolvedNetworkIdentity{}, fmt.Errorf(
			"assignment network gateway %s is this guest's own address",
			gateway,
		)
	}
	resolved.gateway = gateway
	return resolved, nil
}

func broadcastAddressOf(prefix netip.Prefix) netip.Addr {
	octets := prefix.Masked().Addr().As4()
	for bit := range 32 - prefix.Bits() {
		octets[3-bit/8] |= 1 << (bit % 8)
	}
	return netip.AddrFrom4(octets)
}

// NetworkConfigurer installs the per-Instance network identity into a resumed
// guest. It is the network analogue of WorkspaceMounter: the template captures
// the device, the bind installs what makes it this Sandbox's.
type NetworkConfigurer interface {
	Configure(ctx context.Context, identity AssignmentNetworkIdentity) error
}
