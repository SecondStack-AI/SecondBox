package contracts

import (
	"errors"
	"regexp"
)

const (
	// EgressContextNameMaximumLength bounds an opaque operator-selected egress
	// routing context everywhere it is stored, transported, audited, or shown.
	EgressContextNameMaximumLength = 63
	// EgressContextNamePattern is the canonical ASCII syntax shared by HTTP,
	// persistence, Runner configuration and protocol, audit, and diagnostics.
	EgressContextNamePattern = `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`
	// RunnerEgressContextSetMaximumSize bounds one registration snapshot.
	RunnerEgressContextSetMaximumSize = 64
)

var egressContextNamePattern = regexp.MustCompile(EgressContextNamePattern)

// ValidateEgressContextName validates one provider-neutral opaque context name.
// The name is an identifier only; it does not encode a hostname, address, CIDR,
// Tenant reference, gateway identity, or gateway mapping digest.
func ValidateEgressContextName(name string) error {
	if len(name) > EgressContextNameMaximumLength || !egressContextNamePattern.MatchString(name) {
		return errors.New("SecondBox egress context name must contain 1 to 63 lowercase ASCII letters, digits, or hyphens and must begin and end with a letter or digit")
	}
	return nil
}
