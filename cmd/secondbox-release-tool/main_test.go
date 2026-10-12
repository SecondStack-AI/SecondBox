package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

const (
	sharedTestVersion  = "1.2.3"
	sharedTestCommit   = "0123456789abcdef0123456789abcdef01234567"
	sharedTestPostgres = "docker.io/library/postgres@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// writeSharedTestDirectory lays out the shared files release-build-shared.sh
// produces, with an install bootstrap bound to the Linux amd64 deploy binary.
func writeSharedTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for name, mode := range releasecontract.SharedArtifactFileModes(sharedTestVersion) {
		content := []byte("shared " + name)
		switch name {
		case fmt.Sprintf("secondbox-%s-package-metadata.json", sharedTestVersion):
			content = []byte(fmt.Sprintf(`{"schemaVersion":1,"version":%q,"sourceCommit":%q}`, sharedTestVersion, sharedTestCommit))
		case "install.sh":
			continue
		}
		writeSharedTestFile(t, directory, name, content, uint32(mode))
	}
	deploy, err := os.ReadFile(filepath.Join(directory, releasecontract.HostBinaryFileName(sharedTestVersion, "secondbox-deploy", "linux/amd64")))
	if err != nil {
		t.Fatal(err)
	}
	writeSharedTestFile(t, directory, "install.sh", []byte(installBootstrap(sharedTestVersion, deploy)), 0o644)
	return directory
}

func installBootstrap(version string, deploy []byte) string {
	sum := sha256.Sum256(deploy)
	return fmt.Sprintf("#!/bin/sh\nversion='%s'\nexpected_sha256='%s'\n", version, hex.EncodeToString(sum[:]))
}

func writeSharedTestFile(t *testing.T, directory, name string, content []byte, mode uint32) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, os.FileMode(mode)); err != nil {
		t.Fatal(err)
	}
}

func recordSharedTestDirectory(t *testing.T) string {
	t.Helper()
	directory := writeSharedTestDirectory(t)
	if err := writeSharedArtifactsRecord(directory, sharedTestVersion, sharedTestCommit, sharedTestPostgres); err != nil {
		t.Fatalf("writeSharedArtifactsRecord: %v", err)
	}
	return directory
}

func TestSharedArtifactsRecordBindsTheBuiltFiles(t *testing.T) {
	directory := recordSharedTestDirectory(t)
	data, err := os.ReadFile(filepath.Join(directory, releasecontract.SharedArtifactsRecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	record, err := releasecontract.DecodeSharedArtifactsRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if record.Tag != "v"+sharedTestVersion || record.SourceCommit != sharedTestCommit || record.PostgresImage != sharedTestPostgres || len(record.Files) != 14 {
		t.Fatalf("record = %+v", record)
	}
	if err := verifySharedArtifacts(directory, sharedTestVersion, sharedTestCommit); err != nil {
		t.Fatalf("verifySharedArtifacts: %v", err)
	}
}

func TestSharedArtifactsRecordRefusesAnIncompleteBuild(t *testing.T) {
	directory := writeSharedTestDirectory(t)
	if err := os.Remove(filepath.Join(directory, "secondbox-1.2.3.spdx.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeSharedArtifactsRecord(directory, sharedTestVersion, sharedTestCommit, sharedTestPostgres); err == nil {
		t.Fatal("record written for an incomplete shared build")
	}
}

func TestVerifySharedArtifactsRejectsMismatchedSets(t *testing.T) {
	deployLinuxAMD64 := releasecontract.HostBinaryFileName(sharedTestVersion, "secondbox-deploy", "linux/amd64")
	for name, test := range map[string]struct {
		mutate  func(t *testing.T, directory string)
		version string
		commit  string
		want    string
	}{
		"another version":       {version: "1.2.4", want: "not v1.2.4"},
		"another source commit": {commit: strings.Repeat("f", 40), want: "shared artifacts record identifies"},
		"tampered file": {want: "digest differs from the shared artifacts record", mutate: func(t *testing.T, directory string) {
			appendSharedTestFile(t, directory, "secondbox_1.2.3_darwin_arm64")
		}},
		"missing file": {want: "lacks secondbox-1.2.3-openapi.json", mutate: func(t *testing.T, directory string) {
			removeSharedTestFile(t, directory, "secondbox-1.2.3-openapi.json")
		}},
		"undeclared file": {want: "unknown entry extra", mutate: func(t *testing.T, directory string) {
			writeSharedTestFile(t, directory, "extra", []byte("extra"), 0o644)
		}},
		"non-executable binary": {want: "must be a regular file with mode 0755", mutate: func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, deployLinuxAMD64), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		"absent record": {want: "shared artifacts record", mutate: func(t *testing.T, directory string) {
			removeSharedTestFile(t, directory, releasecontract.SharedArtifactsRecordFileName)
		}},
		"package metadata of another commit": {want: "package metadata identity mismatch", mutate: func(t *testing.T, directory string) {
			name := "secondbox-1.2.3-package-metadata.json"
			writeSharedTestFile(t, directory, name, []byte(fmt.Sprintf(`{"schemaVersion":1,"version":%q,"sourceCommit":%q}`, sharedTestVersion, strings.Repeat("f", 40))), 0o644)
			rewriteSharedTestRecordDigest(t, directory, name)
		}},
		"install bootstrap for another deploy binary": {want: "shared install bootstrap lacks expected_sha256=", mutate: func(t *testing.T, directory string) {
			appendSharedTestFile(t, directory, deployLinuxAMD64)
			rewriteSharedTestRecordDigest(t, directory, deployLinuxAMD64)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			directory := recordSharedTestDirectory(t)
			if test.mutate != nil {
				test.mutate(t, directory)
			}
			version, commit := sharedTestVersion, sharedTestCommit
			if test.version != "" {
				version = test.version
			}
			if test.commit != "" {
				commit = test.commit
			}
			err := verifySharedArtifacts(directory, version, commit)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifySharedArtifacts error = %v, want %q", err, test.want)
			}
		})
	}
}

func appendSharedTestFile(t *testing.T, directory, name string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("tampered"); err != nil {
		t.Fatal(err)
	}
}

func removeSharedTestFile(t *testing.T, directory, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
}

// rewriteSharedTestRecordDigest re-records one file, as a consistent forgery of
// file and record would.
func rewriteSharedTestRecordDigest(t *testing.T, directory, name string) {
	t.Helper()
	recordPath := filepath.Join(directory, releasecontract.SharedArtifactsRecordFileName)
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := releasecontract.DecodeSharedArtifactsRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	for index := range record.Files {
		if record.Files[index].Name == name {
			record.Files[index].Digest = releasecontract.Digest(content)
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}
