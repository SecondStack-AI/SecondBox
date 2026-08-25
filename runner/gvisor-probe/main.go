// Command secondbox-gvisor-probe proves the Task 0H gVisor feasibility
// mechanisms on a real Linux host without KVM. It is a bounded spike probe:
// no production composition may depend on it, and it changes no shared
// SecondBox contract.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type probeEnv struct {
	runscPath string
	guestPath string
	workDir   string
	rootless  bool
	stdout    io.Writer
}

type proof struct {
	name string
	run  func(*probeEnv) error
}

// proofs run in dependency order; each emits one or more evidence lines.
var proofs = []proof{
	{name: "sandbox-lifecycle", run: proofSandboxLifecycle},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secondbox-gvisor-probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runscPath := flags.String("runsc", "", "path to the pinned runsc binary")
	guestPath := flags.String("guest", "", "path to the probe guest binary")
	workDir := flags.String("work", "", "probe state directory; must not already exist")
	allowKVMHost := flags.Bool("allow-kvm-host", false,
		"development only: run although /dev/kvm exists; qualification must not pass this")
	rootless := flags.Bool("rootless", false,
		"development only: run runsc rootless; qualification runs as root")
	launchStateRoot := flags.String("internal-launch-state-root", "",
		"internal: supervise one runsc sandbox for the parent-death proof")
	launchBundle := flags.String("internal-launch-bundle", "", "internal: bundle for -internal-launch-state-root")
	launchID := flags.String("internal-launch-id", "", "internal: container ID for -internal-launch-state-root")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *launchStateRoot != "" {
		if err := runLauncher(*runscPath, *rootless, *launchStateRoot, *launchBundle, *launchID); err != nil {
			_, _ = fmt.Fprintf(stderr, "SecondBox gVisor probe launcher failed: %v\n", err)
			return 1
		}
		return 0
	}

	selected := flags.Args()
	if len(selected) == 0 {
		_, _ = fmt.Fprintln(stderr, "SecondBox gVisor probe requires proof names or \"all\"")
		return 2
	}
	if *runscPath == "" || *guestPath == "" || *workDir == "" {
		_, _ = fmt.Fprintln(stderr, "SecondBox gVisor probe requires -runsc, -guest, and -work")
		return 2
	}
	if !*allowKVMHost {
		if _, err := os.Stat("/dev/kvm"); err == nil {
			_, _ = fmt.Fprintln(stderr,
				"SecondBox gVisor probe qualification requires a host without /dev/kvm; "+
					"-allow-kvm-host is for development runs only")
			return 1
		}
	}
	for _, binary := range []string{*runscPath, *guestPath} {
		info, err := os.Stat(binary)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o100 == 0 {
			_, _ = fmt.Fprintf(stderr, "SecondBox gVisor probe requires an executable binary: %s\n", binary)
			return 1
		}
	}
	if _, err := os.Stat(*workDir); err == nil {
		_, _ = fmt.Fprintf(stderr, "SecondBox gVisor probe work directory must not already exist: %s\n", *workDir)
		return 1
	}
	if err := os.MkdirAll(*workDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "SecondBox gVisor probe cannot create work directory: %v\n", err)
		return 1
	}

	env := &probeEnv{
		runscPath: mustAbs(*runscPath),
		guestPath: mustAbs(*guestPath),
		workDir:   mustAbs(*workDir),
		rootless:  *rootless,
		stdout:    stdout,
	}
	for _, p := range selectProofs(selected) {
		if err := p.run(env); err != nil {
			emit(stdout, p.name, "failed", "error="+sanitizeValue(err.Error()))
			_, _ = fmt.Fprintf(stderr, "SecondBox gVisor probe proof %s failed: %v\n", p.name, err)
			return 1
		}
	}
	return 0
}

func selectProofs(names []string) []proof {
	if len(names) == 1 && names[0] == "all" {
		return proofs
	}
	var selected []proof
	for _, name := range names {
		for _, p := range proofs {
			if p.name == name {
				selected = append(selected, p)
			}
		}
	}
	return selected
}

func mustAbs(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

// emit writes one bounded evidence line in the probe's stable key=value form.
func emit(w io.Writer, proofName, status string, pairs ...string) {
	line := "proof=" + proofName
	if len(pairs) > 0 {
		line += " " + strings.Join(pairs, " ")
	}
	_, _ = fmt.Fprintf(w, "%s status=%s\n", line, status)
}

// sanitizeValue keeps evidence values single-token so lines stay parseable.
func sanitizeValue(value string) string {
	replaced := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '=':
			return '_'
		}
		return r
	}, value)
	const bound = 200
	if len(replaced) > bound {
		return replaced[:bound]
	}
	return replaced
}
