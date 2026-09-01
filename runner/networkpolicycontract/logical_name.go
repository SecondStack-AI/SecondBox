// Package networkpolicycontract defines the provider-neutral logical gateway
// syntax shared by deployment tooling and every Runner network backend.
package networkpolicycontract

import (
	"fmt"
	"net/netip"
	"strings"
)

// GeneratedConfigProvenance marks egress-context documents produced by the
// supported deployment renderer without making the marker mandatory for
// operator-authored remote Runner configuration.
const GeneratedConfigProvenance = "secondbox-deploy"

// NormalizeLogicalGatewayName validates and canonicalizes one exact DNS name.
func NormalizeLogicalGatewayName(raw string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(raw))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" || len(domain) > 253 {
		return "", fmt.Errorf("domain length is invalid")
	}
	if strings.ContainsAny(domain, "*:/") {
		return "", fmt.Errorf("domain %q must be one exact DNS name", raw)
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return "", fmt.Errorf("domain %q is an IP address; use a CIDR destination", raw)
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("domain %q contains an invalid label", raw)
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') ||
				(character >= '0' && character <= '9') ||
				character == '-' {
				continue
			}
			return "", fmt.Errorf("domain %q must be an ASCII DNS name", raw)
		}
	}
	return domain, nil
}
