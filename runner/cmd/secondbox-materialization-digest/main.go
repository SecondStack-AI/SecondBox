// secondbox-materialization-digest prints the canonical digest of a backend
// materialization manifest exactly as runner startup computes it, so operator
// pins never depend on the manifest file's key order or formatting.
package main

import (
	"fmt"
	"os"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: secondbox-materialization-digest <manifest.json>")
		os.Exit(2)
	}
	content, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read materialization manifest: %v\n", err)
		os.Exit(1)
	}
	manifest, err := materialization.Decode(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode materialization manifest: %v\n", err)
		os.Exit(1)
	}
	digest, err := manifest.Digest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "compute materialization digest: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
