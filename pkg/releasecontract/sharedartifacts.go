package releasecontract

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// The architecture-neutral files of a release are built once and published
// byte for byte by every guest architecture's artifact set. The shared
// artifacts record binds that directory to one version, tag and source commit.
const (
	SharedArtifactsSchema         = "secondbox.release/shared-artifacts/v1"
	SharedArtifactsRecordFileName = "shared-artifacts.json"
)

var (
	HostBinaryNames     = []string{"secondbox", "secondbox-deploy"}
	HostBinaryPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}
)

type SharedArtifactsRecord struct {
	SchemaVersion string `json:"schemaVersion"`
	Identity
	PostgresImage string               `json:"postgresImage"`
	Files         []SharedArtifactFile `json:"files"`
}

type SharedArtifactFile struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

func HostBinaryFileName(version, name, platform string) string {
	return fmt.Sprintf("%s_%s_%s", name, version, strings.ReplaceAll(platform, "/", "_"))
}

// SharedArtifactFileModes names every shared file of version with the mode it
// is staged and published with: host binaries are executable, documents are not.
func SharedArtifactFileModes(version string) map[string]fs.FileMode {
	modes := map[string]fs.FileMode{
		fmt.Sprintf("secondbox-%s-openapi.json", version):          0o644,
		fmt.Sprintf("secondbox-%s-go-module.tar.gz", version):      0o644,
		fmt.Sprintf("secondstack-ai-secondbox-%s.tgz", version):    0o644,
		fmt.Sprintf("secondbox-%s-package-metadata.json", version): 0o644,
		fmt.Sprintf("secondbox-%s.spdx.json", version):             0o644,
		"install.sh": 0o644,
	}
	for _, name := range HostBinaryNames {
		for _, platform := range HostBinaryPlatforms {
			modes[HostBinaryFileName(version, name, platform)] = 0o755
		}
	}
	return modes
}

func SharedArtifactFileNames(version string) []string {
	modes := SharedArtifactFileModes(version)
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func DecodeSharedArtifactsRecord(data []byte) (SharedArtifactsRecord, error) {
	var record SharedArtifactsRecord
	if err := decodeStrict(data, &record); err != nil {
		return SharedArtifactsRecord{}, contractError("decode shared artifacts record: %v", err)
	}
	if err := record.Validate(); err != nil {
		return SharedArtifactsRecord{}, err
	}
	return record, nil
}

// Validate requires exactly the shared file set of the record's version, each
// named once, in byte order, with a canonical digest.
func (record SharedArtifactsRecord) Validate() error {
	if record.SchemaVersion != SharedArtifactsSchema {
		return contractError("shared artifacts record schema must be %s", SharedArtifactsSchema)
	}
	if err := validateIdentity(record.Identity); err != nil {
		return err
	}
	repository, _, found := strings.Cut(record.PostgresImage, "@")
	if !found || repository == "" {
		return contractError("shared artifacts record Postgres image must be digest-pinned")
	}
	if err := validateOCIReference(repository, record.PostgresImage); err != nil {
		return err
	}
	want := SharedArtifactFileNames(record.Version)
	if len(record.Files) != len(want) {
		return contractError("shared artifacts record must name exactly the %d shared files of v%s", len(want), record.Version)
	}
	for index, file := range record.Files {
		if file.Name != want[index] {
			return contractError("shared artifacts record file %q is unexpected, duplicated or out of order", file.Name)
		}
		if !digestPattern.MatchString(file.Digest) {
			return contractError("shared artifacts record file %q digest must be canonical sha256", file.Name)
		}
	}
	return nil
}

// ValidateForRelease binds the record to the release a staging builds.
func (record SharedArtifactsRecord) ValidateForRelease(version, sourceCommit string) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Version != version || record.SourceCommit != sourceCommit {
		return contractError("shared artifacts record identifies v%s at %s, not v%s at %s", record.Version, record.SourceCommit, version, sourceCommit)
	}
	return nil
}
