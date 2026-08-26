package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
)

func main() {
	if len(os.Args) != 2 || !filepath.IsAbs(os.Args[1]) || filepath.Clean(os.Args[1]) != os.Args[1] {
		fmt.Fprintln(os.Stderr, "usage: secondbox-flat-root-digest CLEAN_ABSOLUTE_FLAT_ROOT")
		os.Exit(2)
	}
	digest, err := materialization.DigestFlatRoot(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
