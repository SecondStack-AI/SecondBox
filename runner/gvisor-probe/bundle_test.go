package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeGuest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(path, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteBundleShape(t *testing.T) {
	bundleDir := t.TempDir()
	err := writeBundle(bundleDir, writeFakeGuest(t), bundleConfig{
		GuestArgs: []string{"stay", "/probe-host/marker"},
		Binds: []bindMount{
			{Source: "/host/markers", Destination: "/probe-host", ReadOnly: false},
			{Source: "/host/assets", Destination: "/assets", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("writeBundle: %v", err)
	}

	encoded, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec ociSpec
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatalf("config.json does not decode: %v", err)
	}
	if spec.Version != "1.2.0" {
		t.Errorf("ociVersion = %q", spec.Version)
	}
	if !spec.Root.Readonly || spec.Root.Path != "rootfs" {
		t.Errorf("root = %+v, want read-only rootfs", spec.Root)
	}
	if want := []string{"/guest", "stay", "/probe-host/marker"}; strings.Join(spec.Process.Args, " ") != strings.Join(want, " ") {
		t.Errorf("process args = %v, want %v", spec.Process.Args, want)
	}

	mountsByDestination := map[string]ociMount{}
	for _, mount := range spec.Mounts {
		mountsByDestination[mount.Destination] = mount
	}
	if mountsByDestination["/proc"].Type != "proc" {
		t.Errorf("missing proc mount: %+v", spec.Mounts)
	}
	writable := mountsByDestination["/probe-host"]
	if writable.Type != "bind" || strings.Join(writable.Options, ",") != "bind,rw" {
		t.Errorf("writable bind = %+v", writable)
	}
	readOnly := mountsByDestination["/assets"]
	if readOnly.Type != "bind" || strings.Join(readOnly.Options, ",") != "bind,ro" {
		t.Errorf("read-only bind = %+v", readOnly)
	}

	namespaces := map[string]bool{}
	for _, namespace := range spec.Linux.Namespaces {
		namespaces[namespace.Type] = true
	}
	for _, required := range []string{"pid", "mount", "ipc", "uts"} {
		if !namespaces[required] {
			t.Errorf("missing %s namespace", required)
		}
	}

	installed, err := os.Stat(filepath.Join(bundleDir, "rootfs", guestBinaryName))
	if err != nil {
		t.Fatalf("guest binary not installed: %v", err)
	}
	if installed.Mode().Perm()&0o100 == 0 {
		t.Errorf("guest binary not executable: %v", installed.Mode())
	}
}

func TestWriteBundleRejectsEmptyArguments(t *testing.T) {
	if err := writeBundle(t.TempDir(), writeFakeGuest(t), bundleConfig{}); err == nil {
		t.Fatal("expected error for empty guest arguments")
	}
}

func TestWriteBundleRejectsMissingGuest(t *testing.T) {
	err := writeBundle(t.TempDir(), filepath.Join(t.TempDir(), "absent"), bundleConfig{
		GuestArgs: []string{"hello", "/probe-host/marker"},
	})
	if err == nil {
		t.Fatal("expected error for missing guest binary")
	}
}

func TestRunscBaseArguments(t *testing.T) {
	rooted := runscBaseArguments("/state", false)
	if strings.Join(rooted, " ") != "--root /state --network=none --platform=systrap" {
		t.Errorf("rooted arguments = %v", rooted)
	}
	rootless := runscBaseArguments("/state", true)
	if rootless[len(rootless)-1] != "--rootless" {
		t.Errorf("rootless arguments = %v", rootless)
	}
}

func TestEmitAndSanitize(t *testing.T) {
	var buffer bytes.Buffer
	emit(&buffer, "sandbox-boot-exit", "passed", "boot_millis=42", "marker=ok")
	if got := buffer.String(); got != "proof=sandbox-boot-exit boot_millis=42 marker=ok status=passed\n" {
		t.Errorf("emit line = %q", got)
	}
	if got := sanitizeValue("multi word=value\nline"); strings.ContainsAny(got, " =\n") {
		t.Errorf("sanitizeValue left separators: %q", got)
	}
	if got := sanitizeValue(strings.Repeat("x", 500)); len(got) != 200 {
		t.Errorf("sanitizeValue bound = %d", len(got))
	}
}

func TestSelectProofs(t *testing.T) {
	if got := selectProofs([]string{"all"}); len(got) != len(proofs) {
		t.Errorf("all selected %d of %d proofs", len(got), len(proofs))
	}
	if got := selectProofs([]string{"sandbox-lifecycle"}); len(got) != 1 || got[0].name != "sandbox-lifecycle" {
		t.Errorf("named selection = %v", got)
	}
	if got := selectProofs([]string{"absent"}); len(got) != 0 {
		t.Errorf("unknown selection = %v", got)
	}
}
