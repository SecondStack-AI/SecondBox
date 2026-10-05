package buildinfo

import (
	"strings"
	"testing"
)

func TestIdentityDevelopmentRequiresBothSentinelsOrNeither(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, test := range []struct {
		identity    Identity
		development bool
		partial     bool
	}{
		{Identity{Version: DevelopmentVersion, SourceCommit: DevelopmentSourceCommit}, true, false},
		{Identity{Version: "0.23.0", SourceCommit: commit}, false, false},
		{Identity{Version: DevelopmentVersion, SourceCommit: commit}, false, true},
		{Identity{Version: "0.23.0", SourceCommit: DevelopmentSourceCommit}, false, true},
	} {
		development, err := test.identity.Development()
		if test.partial != (err != nil) || development != test.development {
			t.Errorf("%#v Development() = %t, %v", test.identity, development, err)
		}
	}
}
