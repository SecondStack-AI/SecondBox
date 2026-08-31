package contracts

import (
	"strings"
	"testing"
)

func TestValidateEgressContextName(t *testing.T) {
	tests := map[string]struct {
		name    string
		wantErr bool
	}{
		"single character":       {name: "a"},
		"opaque label":           {name: "secondstack-staging-2"},
		"maximum length":         {name: strings.Repeat("a", EgressContextNameMaximumLength)},
		"empty":                  {name: "", wantErr: true},
		"too long":               {name: strings.Repeat("a", EgressContextNameMaximumLength+1), wantErr: true},
		"uppercase":              {name: "Staging", wantErr: true},
		"leading hyphen":         {name: "-staging", wantErr: true},
		"trailing hyphen":        {name: "staging-", wantErr: true},
		"underscore":             {name: "staging_east", wantErr: true},
		"hostname punctuation":   {name: "staging.example", wantErr: true},
		"network range":          {name: "10.0.0.0/8", wantErr: true},
		"surrounding whitespace": {name: " staging", wantErr: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateEgressContextName(test.name)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateEgressContextName(%q) error = %v, wantErr %t", test.name, err, test.wantErr)
			}
		})
	}
}
