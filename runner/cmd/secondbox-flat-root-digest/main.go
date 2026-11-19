package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
)

func main() {
	if (len(os.Args) != 2 && len(os.Args) != 3) || !filepath.IsAbs(os.Args[1]) || filepath.Clean(os.Args[1]) != os.Args[1] {
		fmt.Fprintln(os.Stderr, "usage: secondbox-flat-root-digest CLEAN_ABSOLUTE_FLAT_ROOT [MATERIALIZATION_MANIFEST]")
		os.Exit(2)
	}
	digest, err := materialization.DigestFlatRoot(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) == 3 {
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		manifest, err := materialization.Decode(data)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if manifest.FlatRootDigest != digest {
			fmt.Fprintln(os.Stderr, "SecondBox packaged flat root differs from pinned materialization")
			os.Exit(1)
		}
	}
	fmt.Println(digest)
}
