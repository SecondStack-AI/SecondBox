package microvmguest

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// LinuxNetworkConfigurer installs a resumed guest's network identity over
// rtnetlink.
//
// The guest rootfs deliberately carries no userspace networking tools, because a
// cold-booted guest never needs one: the kernel's own IP autoconfiguration
// consumes the `ip=` boot argument. A resumed guest has no such argument — its
// kernel finished booting before this Sandbox existed — so the agent speaks to
// the same kernel subsystem directly.
type LinuxNetworkConfigurer struct{}

// Configure applies the identity in the one order that never lets a
// half-configured guest emit a frame: the address and hardware address are
// installed while the link is still down, the link comes up only once both
// succeeded, and the default route is last.
func (LinuxNetworkConfigurer) Configure(_ context.Context, identity AssignmentNetworkIdentity) error {
	resolved, err := identity.resolve()
	if err != nil {
		return err
	}
	device, err := net.InterfaceByName(resolved.name)
	if err != nil {
		return fmt.Errorf("resolve assignment network interface %q: %w", resolved.name, err)
	}
	if device.Flags&net.FlagUp != 0 {
		return fmt.Errorf(
			"assignment network interface %q is already up; a template is captured with its link down",
			resolved.name,
		)
	}
	connection, err := dialRoutingNetlink()
	if err != nil {
		return err
	}
	defer connection.close()
	index := int32(device.Index)
	if err := connection.setInterfaceHardwareAddress(index, resolved.hardware); err != nil {
		return fmt.Errorf("set assignment network hardware address: %w", err)
	}
	if err := connection.addInterfaceAddress(index, resolved); err != nil {
		return fmt.Errorf("add assignment network address: %w", err)
	}
	if err := connection.setInterfaceUp(index); err != nil {
		return fmt.Errorf("bring assignment network interface up: %w", err)
	}
	if err := connection.addDefaultRoute(index, resolved); err != nil {
		return fmt.Errorf("add assignment network default route: %w", err)
	}
	return nil
}

// routingNetlink is one rtnetlink socket used for the whole install. It is
// request/acknowledgement only: every message asks for NLM_F_ACK and the reply
// is read before the next one is sent, so a rejected step fails the bind instead
// of being discovered later.
type routingNetlink struct {
	fd       int
	sequence uint32
}

// routingNetlinkTimeout bounds a lost acknowledgement. The install runs inside
// the guest's one assignment bind, which the runner is waiting on, so it must
// fail rather than block.
const routingNetlinkTimeout = 5 * time.Second

func dialRoutingNetlink() (*routingNetlink, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open rtnetlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind rtnetlink socket: %w", err)
	}
	timeout := unix.NsecToTimeval(routingNetlinkTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("set rtnetlink receive timeout: %w", err)
	}
	return &routingNetlink{fd: fd}, nil
}

func (c *routingNetlink) close() {
	_ = unix.Close(c.fd)
}

func (c *routingNetlink) setInterfaceHardwareAddress(index int32, hardware net.HardwareAddr) error {
	request := newRoutingNetlinkRequest(unix.RTM_SETLINK, 0, encodeIfInfomsg(unix.IfInfomsg{
		Family: unix.AF_UNSPEC,
		Index:  index,
	}))
	request.attribute(unix.IFLA_ADDRESS, hardware)
	return c.execute(request)
}

func (c *routingNetlink) setInterfaceUp(index int32) error {
	return c.execute(newRoutingNetlinkRequest(unix.RTM_SETLINK, 0, encodeIfInfomsg(unix.IfInfomsg{
		Family: unix.AF_UNSPEC,
		Index:  index,
		Flags:  unix.IFF_UP,
		Change: unix.IFF_UP,
	})))
}

// addInterfaceAddress uses NLM_F_EXCL rather than a replace: a template's
// interface is captured address-less, so an address already being present means
// this guest is not the identity-neutral one the runner believes it resumed.
func (c *routingNetlink) addInterfaceAddress(index int32, identity resolvedNetworkIdentity) error {
	request := newRoutingNetlinkRequest(
		unix.RTM_NEWADDR,
		unix.NLM_F_CREATE|unix.NLM_F_EXCL,
		encodeIfAddrmsg(unix.IfAddrmsg{
			Family:    unix.AF_INET,
			Prefixlen: uint8(identity.prefixBits),
			Scope:     unix.RT_SCOPE_UNIVERSE,
			Index:     uint32(index),
		}),
	)
	local := identity.address.As4()
	broadcast := identity.broadcast.As4()
	request.attribute(unix.IFA_LOCAL, local[:])
	request.attribute(unix.IFA_ADDRESS, local[:])
	request.attribute(unix.IFA_BROADCAST, broadcast[:])
	return c.execute(request)
}

func (c *routingNetlink) addDefaultRoute(index int32, identity resolvedNetworkIdentity) error {
	request := newRoutingNetlinkRequest(
		unix.RTM_NEWROUTE,
		unix.NLM_F_CREATE|unix.NLM_F_EXCL,
		encodeRtMsg(unix.RtMsg{
			Family:   unix.AF_INET,
			Table:    unix.RT_TABLE_MAIN,
			Protocol: unix.RTPROT_STATIC,
			Scope:    unix.RT_SCOPE_UNIVERSE,
			Type:     unix.RTN_UNICAST,
		}),
	)
	gateway := identity.gateway.As4()
	outputInterface := make([]byte, 4)
	binary.NativeEndian.PutUint32(outputInterface, uint32(index))
	request.attribute(unix.RTA_GATEWAY, gateway[:])
	request.attribute(unix.RTA_OIF, outputInterface)
	return c.execute(request)
}

// routingNetlinkRequest is one rtnetlink message under construction: a fixed
// family header followed by attributes.
type routingNetlinkRequest struct {
	kind    uint16
	flags   uint16
	payload []byte
}

func newRoutingNetlinkRequest(kind uint16, flags uint16, header []byte) routingNetlinkRequest {
	return routingNetlinkRequest{kind: kind, flags: flags, payload: header}
}

func (r *routingNetlinkRequest) attribute(kind uint16, value []byte) {
	length := unix.SizeofRtAttr + len(value)
	attribute := make([]byte, netlinkAlign(length))
	binary.NativeEndian.PutUint16(attribute[0:2], uint16(length))
	binary.NativeEndian.PutUint16(attribute[2:4], kind)
	copy(attribute[unix.SizeofRtAttr:], value)
	r.payload = append(r.payload, attribute...)
}

func (c *routingNetlink) execute(request routingNetlinkRequest) error {
	c.sequence++
	length := unix.SizeofNlMsghdr + len(request.payload)
	message := make([]byte, netlinkAlign(length))
	binary.NativeEndian.PutUint32(message[0:4], uint32(length))
	binary.NativeEndian.PutUint16(message[4:6], request.kind)
	binary.NativeEndian.PutUint16(message[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK|request.flags)
	binary.NativeEndian.PutUint32(message[8:12], c.sequence)
	binary.NativeEndian.PutUint32(message[12:16], 0)
	copy(message[unix.SizeofNlMsghdr:], request.payload)
	if err := unix.Sendto(c.fd, message, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("send rtnetlink request type %d: %w", request.kind, err)
	}
	return c.acknowledgement(request.kind)
}

func (c *routingNetlink) acknowledgement(kind uint16) error {
	buffer := make([]byte, unix.Getpagesize())
	for {
		read, _, err := unix.Recvfrom(c.fd, buffer, 0)
		if err != nil {
			return fmt.Errorf("read rtnetlink acknowledgement for type %d: %w", kind, err)
		}
		done, err := c.scanAcknowledgement(kind, buffer[:read])
		if err != nil || done {
			return err
		}
	}
}

// scanAcknowledgement walks one datagram's netlink messages and reports whether
// this request's outcome was in it. Messages for other sequences belong to
// nothing this socket asked for and are skipped.
func (c *routingNetlink) scanAcknowledgement(kind uint16, datagram []byte) (bool, error) {
	for len(datagram) >= unix.SizeofNlMsghdr {
		length := int(binary.NativeEndian.Uint32(datagram[0:4]))
		if length < unix.SizeofNlMsghdr || length > len(datagram) {
			return false, fmt.Errorf("malformed rtnetlink reply length %d for type %d", length, kind)
		}
		messageType := binary.NativeEndian.Uint16(datagram[4:6])
		sequence := binary.NativeEndian.Uint32(datagram[8:12])
		payload := datagram[unix.SizeofNlMsghdr:length]
		if sequence == c.sequence {
			switch messageType {
			case unix.NLMSG_ERROR:
				if len(payload) < 4 {
					return false, fmt.Errorf("truncated rtnetlink error for type %d", kind)
				}
				code := int32(binary.NativeEndian.Uint32(payload[0:4]))
				if code == 0 {
					return true, nil
				}
				return true, fmt.Errorf("rtnetlink rejected type %d: %w", kind, unix.Errno(-code))
			case unix.NLMSG_DONE:
				return true, nil
			}
		}
		advance := netlinkAlign(length)
		if advance > len(datagram) {
			return false, nil
		}
		datagram = datagram[advance:]
	}
	return false, nil
}

func netlinkAlign(length int) int {
	return (length + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
}

func encodeIfInfomsg(message unix.IfInfomsg) []byte {
	encoded := make([]byte, unix.SizeofIfInfomsg)
	encoded[0] = message.Family
	binary.NativeEndian.PutUint16(encoded[2:4], message.Type)
	binary.NativeEndian.PutUint32(encoded[4:8], uint32(message.Index))
	binary.NativeEndian.PutUint32(encoded[8:12], message.Flags)
	binary.NativeEndian.PutUint32(encoded[12:16], message.Change)
	return encoded
}

func encodeIfAddrmsg(message unix.IfAddrmsg) []byte {
	encoded := make([]byte, unix.SizeofIfAddrmsg)
	encoded[0] = message.Family
	encoded[1] = message.Prefixlen
	encoded[2] = message.Flags
	encoded[3] = message.Scope
	binary.NativeEndian.PutUint32(encoded[4:8], message.Index)
	return encoded
}

func encodeRtMsg(message unix.RtMsg) []byte {
	encoded := make([]byte, unix.SizeofRtMsg)
	encoded[0] = message.Family
	encoded[1] = message.Dst_len
	encoded[2] = message.Src_len
	encoded[3] = message.Tos
	encoded[4] = message.Table
	encoded[5] = message.Protocol
	encoded[6] = message.Scope
	encoded[7] = message.Type
	binary.NativeEndian.PutUint32(encoded[8:12], message.Flags)
	return encoded
}
