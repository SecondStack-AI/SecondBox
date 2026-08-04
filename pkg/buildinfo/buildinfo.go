// Package buildinfo contains immutable release identity injected at link time.
package buildinfo

import (
	"encoding/json"
	"io"
)

var (
	Version      = "0.0.0-development"
	SourceCommit = "development"
)

type Identity struct {
	Version      string `json:"version"`
	SourceCommit string `json:"sourceCommit"`
}

func Write(output io.Writer) error {
	return json.NewEncoder(output).Encode(Identity{Version: Version, SourceCommit: SourceCommit})
}
