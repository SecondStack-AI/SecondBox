package releasecontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func testSharedArtifactsRecord() SharedArtifactsRecord {
	record := SharedArtifactsRecord{SchemaVersion: SharedArtifactsSchema, Identity: Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: testCommit}, PostgresImage: "docker.io/library/postgres@" + testDigest}
	for _, name := range SharedArtifactFileNames("1.2.3") {
		record.Files = append(record.Files, SharedArtifactFile{Name: name, Digest: testDigest})
	}
	return record
}

func TestSharedArtifactFileModesNameEveryArchitectureNeutralFile(t *testing.T) {
	modes := SharedArtifactFileModes("1.2.3")
	if len(modes) != 14 {
		t.Fatalf("shared files = %d, want 6 documents and 8 host binaries", len(modes))
	}
	for name, mode := range map[string]uint32{
		"secondbox-1.2.3-openapi.json":          0o644,
		"secondbox-1.2.3-go-module.tar.gz":      0o644,
		"secondstack-ai-secondbox-1.2.3.tgz":    0o644,
		"secondbox-1.2.3-package-metadata.json": 0o644,
		"secondbox-1.2.3.spdx.json":             0o644,
		"install.sh":                            0o644,
		"secondbox_1.2.3_darwin_arm64":          0o755,
		"secondbox-deploy_1.2.3_linux_amd64":    0o755,
	} {
		if uint32(modes[name]) != mode {
			t.Errorf("mode of %s = %04o, want %04o", name, modes[name], mode)
		}
	}
}

func TestSharedArtifactsRecordRoundTrip(t *testing.T) {
	record := testSharedArtifactsRecord()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSharedArtifactsRecord(data)
	if err != nil {
		t.Fatalf("DecodeSharedArtifactsRecord: %v", err)
	}
	if err := decoded.ValidateForRelease("1.2.3", testCommit); err != nil {
		t.Fatalf("ValidateForRelease: %v", err)
	}
	if !strings.Contains(string(data), `"tag":"v1.2.3"`) {
		t.Fatalf("record does not carry its tag: %s", data)
	}
}

func TestSharedArtifactsRecordRejectsAnotherRelease(t *testing.T) {
	record := testSharedArtifactsRecord()
	if err := record.ValidateForRelease("1.2.4", testCommit); err == nil {
		t.Fatal("record of v1.2.3 accepted for v1.2.4")
	}
	if err := record.ValidateForRelease("1.2.3", strings.Repeat("f", 40)); err == nil {
		t.Fatal("record accepted for another source commit")
	}
}

func TestSharedArtifactsRecordRejectsMalformedRecords(t *testing.T) {
	for name, mutate := range map[string]func(*SharedArtifactsRecord){
		"schema":           func(record *SharedArtifactsRecord) { record.SchemaVersion = "secondbox.release/shared-artifacts/v0" },
		"tag":              func(record *SharedArtifactsRecord) { record.Tag = "v1.2.4" },
		"commit":           func(record *SharedArtifactsRecord) { record.SourceCommit = "HEAD" },
		"mutable postgres": func(record *SharedArtifactsRecord) { record.PostgresImage = "docker.io/library/postgres:18" },
		"missing file":     func(record *SharedArtifactsRecord) { record.Files = record.Files[1:] },
		"extra file": func(record *SharedArtifactsRecord) {
			record.Files = append(record.Files, SharedArtifactFile{Name: "extra", Digest: testDigest})
		},
		"duplicated file": func(record *SharedArtifactsRecord) { record.Files[1] = record.Files[0] },
		"unordered files": func(record *SharedArtifactsRecord) {
			record.Files[0], record.Files[1] = record.Files[1], record.Files[0]
		},
		"another version": func(record *SharedArtifactsRecord) {
			last := len(record.Files) - 1
			record.Files[last].Name = strings.ReplaceAll(record.Files[last].Name, "1.2.3", "1.2.4")
		},
		"noncanonical hash": func(record *SharedArtifactsRecord) { record.Files[0].Digest = strings.ToUpper(testDigest) },
	} {
		t.Run(name, func(t *testing.T) {
			record := testSharedArtifactsRecord()
			mutate(&record)
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSharedArtifactsRecord(data); err == nil {
				t.Fatal("malformed shared artifacts record accepted")
			}
		})
	}
	if _, err := DecodeSharedArtifactsRecord([]byte(`{"schemaVersion":"` + SharedArtifactsSchema + `","unknown":true}`)); err == nil {
		t.Fatal("record with an unknown field accepted")
	}
}
