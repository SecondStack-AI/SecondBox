package service

import (
	"errors"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestValidateGrantExtensionAcceptsOneAddOnlyKindWithCreationEntryRules(t *testing.T) {
	tooManyGrants := make([]string, 33)
	for index := range tooManyGrants {
		tooManyGrants[index] = "profile-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
	}
	tests := map[string]struct {
		profileGrants []string
		scopes        []string
		valid         bool
	}{
		"scope only":            {profileGrants: []string{}, scopes: []string{"sandbox:ports"}, valid: true},
		"grant only":            {profileGrants: []string{"agent-compartment"}, scopes: []string{}, valid: true},
		"both kinds":            {profileGrants: []string{"coding"}, scopes: []string{"sandbox:read", "sandbox:ports:direct"}, valid: true},
		"nothing to add":        {profileGrants: []string{}, scopes: []string{}},
		"unknown scope":         {profileGrants: []string{}, scopes: []string{"sandbox:admin"}},
		"duplicate scope":       {profileGrants: []string{}, scopes: []string{"sandbox:read", "sandbox:read"}},
		"invalid Profile name":  {profileGrants: []string{"Not A Profile"}, scopes: []string{}},
		"duplicate grant":       {profileGrants: []string{"coding", "coding"}, scopes: []string{}},
		"too many grants":       {profileGrants: tooManyGrants, scopes: []string{}},
		"too many scope values": {profileGrants: []string{}, scopes: []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports", "sandbox:ports:direct", "sandbox:read"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateGrantExtension("Tenant ceiling", test.profileGrants, test.scopes)
			if test.valid && err != nil {
				t.Fatalf("valid extension rejected: %v", err)
			}
			if !test.valid && !errors.Is(err, ports.ErrInvalidRequest) {
				t.Fatalf("invalid extension error = %v", err)
			}
		})
	}
}
