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
		field         string
	}{
		"scope only":            {profileGrants: []string{}, scopes: []string{"sandbox:ports"}},
		"grant only":            {profileGrants: []string{"agent-compartment"}, scopes: []string{}},
		"both kinds":            {profileGrants: []string{"coding"}, scopes: []string{"sandbox:read", "sandbox:ports:direct"}},
		"nothing to add":        {profileGrants: []string{}, scopes: []string{}, field: "body"},
		"unknown scope":         {profileGrants: []string{}, scopes: []string{"sandbox:admin"}, field: "applicationScopes"},
		"duplicate scope":       {profileGrants: []string{}, scopes: []string{"sandbox:read", "sandbox:read"}, field: "applicationScopes"},
		"invalid Profile name":  {profileGrants: []string{"Not A Profile"}, scopes: []string{}, field: "profileGrants"},
		"duplicate grant":       {profileGrants: []string{"coding", "coding"}, scopes: []string{}, field: "profileGrants"},
		"too many grants":       {profileGrants: tooManyGrants, scopes: []string{}, field: "profileGrants"},
		"too many scope values": {profileGrants: []string{}, scopes: []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports", "sandbox:ports:direct", "sandbox:read"}, field: "applicationScopes"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateGrantExtension("applicationScopes", test.profileGrants, test.scopes)
			if test.field == "" {
				if err != nil {
					t.Fatalf("valid extension rejected: %v", err)
				}
				return
			}
			var fieldError *ports.InvalidFieldError
			if !errors.Is(err, ports.ErrInvalidRequest) || !errors.As(err, &fieldError) || fieldError.Field != test.field {
				t.Fatalf("invalid extension error = %v, want field %q", err, test.field)
			}
		})
	}
}
