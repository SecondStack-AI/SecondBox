package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryHandoffRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escaped", "/absolute", "a/../escaped", "a\\escaped"} {
		if err := writeRegistryHandoff(filepath.Join(root, "registry"), map[string]string{"tenants.json": "{}", name: "secret"}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "linked")
	if err := os.Symlink(target, registry); err != nil {
		t.Fatal(err)
	}
	if err := writeRegistryHandoff(registry, map[string]string{"tenants.json": "{}"}); err == nil {
		t.Fatal("followed registry symlink")
	}
	regular := filepath.Join(root, "valid")
	files := map[string]string{"tenants.json": "{}", "certificates/registry.crt": "certificate"}
	if err := writeRegistryHandoff(regular, files); err != nil {
		t.Fatal(err)
	}
	if err := writeRegistryHandoff(regular, files); err != nil {
		t.Fatal(err)
	}
	files["tenants.json"] = "changed"
	if err := writeRegistryHandoff(regular, files); err == nil {
		t.Fatal("overwrote changed enrollment handoff")
	}
}
