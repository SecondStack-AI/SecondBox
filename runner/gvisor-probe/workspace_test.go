package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadExt4UUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	content := make([]byte, 4096)
	uuid := []byte{
		0x31, 0xe4, 0x0c, 0xd4, 0x5f, 0x5a, 0x4b, 0x54,
		0xa0, 0x6e, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab,
	}
	copy(content[ext4UUIDOffset:], uuid)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	got, err := readExt4UUID(image)
	if err != nil {
		t.Fatalf("readExt4UUID: %v", err)
	}
	if want := "31e40cd45f5a4b54a06e0123456789ab"; got != want {
		t.Errorf("uuid = %s, want %s", got, want)
	}
}

func TestReadExt4UUIDRejectsShortImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(path, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if _, err := readExt4UUID(image); err == nil {
		t.Error("expected an error for an image shorter than the superblock")
	}
}

func TestImageIdentityStableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	firstIdentity, err := imageIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := imageIdentity(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity != secondIdentity {
		t.Errorf("identities differ across reopen: %+v vs %+v", firstIdentity, secondIdentity)
	}
}

func TestCreateExt4ImageRejectsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createExt4Image(path, workspaceImageBytes, workspaceUUID); err == nil {
		t.Error("expected an error for a pre-existing image path")
	}
}
