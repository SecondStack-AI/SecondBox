package microvmguest

import (
	"errors"
	"testing"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"golang.org/x/sys/unix"
)

func TestProtocolFileErrorKind(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want guestv1.FileTerminalKind
	}{
		{"full workspace", unix.ENOSPC, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_WORKSPACE_FULL},
		{"quota exhausted", unix.EDQUOT, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_WORKSPACE_FULL},
		{"permission denied", unix.EACCES, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED},
		{"other write failure", errors.New("I/O failed"), guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := protocolFileErrorKind(test.err); got != test.want {
				t.Fatalf("kind = %v, want %v", got, test.want)
			}
		})
	}
}
