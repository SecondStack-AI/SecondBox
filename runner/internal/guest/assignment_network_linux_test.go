package microvmguest

import (
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// The wire encoding is the part of the install that cannot be observed from a
// unit test's effects, only from its bytes. Every field is host-endian and every
// attribute is padded to a 4-byte boundary while its recorded length is not, and
// a mistake in either is silently accepted by the kernel as a different request.

func TestNetlinkAttributeRecordsUnpaddedLengthAndPadsPayload(t *testing.T) {
	request := newRoutingNetlinkRequest(unix.RTM_SETLINK, 0, encodeIfInfomsg(unix.IfInfomsg{Index: 9}))
	request.attribute(unix.IFLA_ADDRESS, []byte{0x02, 0x9f, 0x1c, 0x44, 0x5e, 0x07})
	attribute := request.payload[unix.SizeofIfInfomsg:]
	if len(attribute) != 12 {
		t.Fatalf("attribute occupies %d bytes, want 12 (4 header + 6 value + 2 padding)", len(attribute))
	}
	if length := binary.NativeEndian.Uint16(attribute[0:2]); length != 10 {
		t.Fatalf("attribute length = %d, want the unpadded 10", length)
	}
	if kind := binary.NativeEndian.Uint16(attribute[2:4]); kind != unix.IFLA_ADDRESS {
		t.Fatalf("attribute type = %d", kind)
	}
	if got := attribute[4:10]; string(got) != string([]byte{0x02, 0x9f, 0x1c, 0x44, 0x5e, 0x07}) {
		t.Fatalf("attribute value = %v", got)
	}
	if attribute[10] != 0 || attribute[11] != 0 {
		t.Fatalf("attribute padding is not zeroed: %v", attribute[10:12])
	}
}

func TestEncodeIfInfomsgMatchesKernelLayout(t *testing.T) {
	encoded := encodeIfInfomsg(unix.IfInfomsg{
		Family: unix.AF_UNSPEC,
		Index:  0x11223344,
		Flags:  unix.IFF_UP,
		Change: unix.IFF_UP,
	})
	if len(encoded) != unix.SizeofIfInfomsg {
		t.Fatalf("encoded %d bytes, want %d", len(encoded), unix.SizeofIfInfomsg)
	}
	if encoded[0] != unix.AF_UNSPEC || encoded[1] != 0 {
		t.Fatalf("family and pad = %v", encoded[0:2])
	}
	if index := int32(binary.NativeEndian.Uint32(encoded[4:8])); index != 0x11223344 {
		t.Fatalf("index = %#x", index)
	}
	if flags := binary.NativeEndian.Uint32(encoded[8:12]); flags != unix.IFF_UP {
		t.Fatalf("flags = %#x", flags)
	}
	if change := binary.NativeEndian.Uint32(encoded[12:16]); change != unix.IFF_UP {
		t.Fatalf("change = %#x", change)
	}
}

func TestEncodeIfAddrmsgMatchesKernelLayout(t *testing.T) {
	encoded := encodeIfAddrmsg(unix.IfAddrmsg{
		Family:    unix.AF_INET,
		Prefixlen: 24,
		Scope:     unix.RT_SCOPE_UNIVERSE,
		Index:     7,
	})
	if len(encoded) != unix.SizeofIfAddrmsg {
		t.Fatalf("encoded %d bytes, want %d", len(encoded), unix.SizeofIfAddrmsg)
	}
	if encoded[0] != unix.AF_INET || encoded[1] != 24 || encoded[2] != 0 || encoded[3] != unix.RT_SCOPE_UNIVERSE {
		t.Fatalf("fixed header = %v", encoded[0:4])
	}
	if index := binary.NativeEndian.Uint32(encoded[4:8]); index != 7 {
		t.Fatalf("index = %d", index)
	}
}

func TestEncodeRtMsgMatchesKernelLayout(t *testing.T) {
	encoded := encodeRtMsg(unix.RtMsg{
		Family:   unix.AF_INET,
		Table:    unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC,
		Scope:    unix.RT_SCOPE_UNIVERSE,
		Type:     unix.RTN_UNICAST,
	})
	if len(encoded) != unix.SizeofRtMsg {
		t.Fatalf("encoded %d bytes, want %d", len(encoded), unix.SizeofRtMsg)
	}
	// A default route is destination length zero. Anything else would install a
	// host route to 0.0.0.0 instead.
	if encoded[0] != unix.AF_INET || encoded[1] != 0 || encoded[2] != 0 || encoded[3] != 0 {
		t.Fatalf("family through tos = %v", encoded[0:4])
	}
	if encoded[4] != unix.RT_TABLE_MAIN || encoded[5] != unix.RTPROT_STATIC {
		t.Fatalf("table and protocol = %v", encoded[4:6])
	}
	if encoded[6] != unix.RT_SCOPE_UNIVERSE || encoded[7] != unix.RTN_UNICAST {
		t.Fatalf("scope and type = %v", encoded[6:8])
	}
	if flags := binary.NativeEndian.Uint32(encoded[8:12]); flags != 0 {
		t.Fatalf("flags = %#x", flags)
	}
}

func TestNetlinkAlignRoundsUpToFourBytes(t *testing.T) {
	for input, want := range map[int]int{0: 0, 1: 4, 4: 4, 5: 8, 10: 12, 16: 16} {
		if got := netlinkAlign(input); got != want {
			t.Fatalf("netlinkAlign(%d) = %d, want %d", input, got, want)
		}
	}
}

func TestScanAcknowledgementReportsKernelErrno(t *testing.T) {
	connection := &routingNetlink{sequence: 3}
	datagram := netlinkErrorDatagram(3, -int32(unix.EEXIST))
	done, err := connection.scanAcknowledgement(unix.RTM_NEWADDR, datagram)
	if !done {
		t.Fatal("a matching error message must complete the request")
	}
	if err == nil || !isErrno(err, unix.EEXIST) {
		t.Fatalf("error = %v, want EEXIST", err)
	}
}

func TestScanAcknowledgementAcceptsZeroErrorAndSkipsOtherSequences(t *testing.T) {
	connection := &routingNetlink{sequence: 4}
	other := netlinkErrorDatagram(9, -int32(unix.EINVAL))
	done, err := connection.scanAcknowledgement(unix.RTM_NEWROUTE, other)
	if done || err != nil {
		t.Fatalf("another request's error was consumed: done=%t err=%v", done, err)
	}
	done, err = connection.scanAcknowledgement(unix.RTM_NEWROUTE, netlinkErrorDatagram(4, 0))
	if !done || err != nil {
		t.Fatalf("acknowledgement not accepted: done=%t err=%v", done, err)
	}
}

func TestScanAcknowledgementRejectsMalformedLength(t *testing.T) {
	connection := &routingNetlink{sequence: 1}
	datagram := netlinkErrorDatagram(1, 0)
	binary.NativeEndian.PutUint32(datagram[0:4], 4)
	if _, err := connection.scanAcknowledgement(unix.RTM_SETLINK, datagram); err == nil {
		t.Fatal("a reply shorter than its own header was accepted")
	}
}

func netlinkErrorDatagram(sequence uint32, code int32) []byte {
	datagram := make([]byte, unix.SizeofNlMsghdr+4)
	binary.NativeEndian.PutUint32(datagram[0:4], uint32(len(datagram)))
	binary.NativeEndian.PutUint16(datagram[4:6], unix.NLMSG_ERROR)
	binary.NativeEndian.PutUint32(datagram[8:12], sequence)
	binary.NativeEndian.PutUint32(datagram[unix.SizeofNlMsghdr:], uint32(code))
	return datagram
}

func isErrno(err error, want unix.Errno) bool {
	var errno unix.Errno
	return errors.As(err, &errno) && errno == want
}
