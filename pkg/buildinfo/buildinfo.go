// Package buildinfo contains immutable release identity injected at link time.
package buildinfo

import (
	"encoding/json"
	"fmt"
	"io"
)

// An unstamped build carries these sentinels. Release and source-built images
// stamp both values at link time and refuse to build without them.
const (
	DevelopmentVersion      = "0.0.0-development"
	DevelopmentSourceCommit = "development"
)

var (
	Version      = DevelopmentVersion
	SourceCommit = DevelopmentSourceCommit
)

type Identity struct {
	Version      string `json:"version"`
	SourceCommit string `json:"sourceCommit"`
}

// Current returns the identity stamped into the running binary.
func Current() Identity {
	return Identity{Version: Version, SourceCommit: SourceCommit}
}

// Development reports whether the identity is the unstamped development
// sentinel. A build that stamped only one of the two values is rejected.
func (identity Identity) Development() (bool, error) {
	version := identity.Version == DevelopmentVersion
	commit := identity.SourceCommit == DevelopmentSourceCommit
	if version != commit {
		return false, fmt.Errorf("SecondBox build identity is partially stamped: version %q, source commit %q", identity.Version, identity.SourceCommit)
	}
	return version, nil
}

func Write(output io.Writer) error {
	return json.NewEncoder(output).Encode(Current())
}
