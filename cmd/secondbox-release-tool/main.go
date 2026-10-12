package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

type candidateInput struct {
	Candidate                   bool                            `json:"candidate"`
	Version                     string                          `json:"version"`
	SourceCommit                string                          `json:"sourceCommit"`
	ControlPlaneDigest          string                          `json:"controlPlaneDigest"`
	RunnerDigest                string                          `json:"runnerDigest"`
	InstallerToolsDigest        string                          `json:"installerToolsDigest"`
	PostgresImage               string                          `json:"postgresImage"`
	MicroVMImageDigest          string                          `json:"microvmImageDigest"`
	MicroVMManifestDigest       string                          `json:"microvmManifestDigest"`
	MicroVMRuntimeBundle        releasecontract.BundleComponent `json:"microvmRuntimeBundle"`
	MicroVMToolchainBundle      releasecontract.BundleComponent `json:"microvmToolchainBundle"`
	GVisorRunnerDigest          string                          `json:"gvisorRunnerDigest"`
	GVisorImageDigest           string                          `json:"gvisorImageDigest"`
	GVisorMaterializationDigest string                          `json:"gvisorMaterializationDigest"`
	GVisorFlatRootDigest        string                          `json:"gvisorFlatRootDigest"`
	ImagePlatforms              []string                        `json:"imagePlatforms"`
	GuestArchitecture           string                          `json:"guestArchitecture"`
	GVisorRunscRelease          string                          `json:"gvisorRunscRelease"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 3 && args[0] == "standard-documents" {
		return writeStandardDocuments(args[1], args[2])
	}
	if len(args) == 3 && args[0] == "manifest" {
		return writeManifest(args[1], args[2])
	}
	if len(args) == 2 && args[0] == "verify" {
		return verifyCandidate(args[1])
	}
	if len(args) == 2 && args[0] == "installer-qualification-subject" {
		return writeInstallerQualificationSubject(args[1])
	}
	if len(args) == 5 && args[0] == "write-shared-artifacts-record" {
		return writeSharedArtifactsRecord(args[1], args[2], args[3], args[4])
	}
	if len(args) == 4 && args[0] == "verify-shared-artifacts" {
		return verifySharedArtifacts(args[1], args[2], args[3])
	}
	return errors.New("usage: secondbox-release-tool {standard-documents ARCHITECTURE OUTPUT_DIR|manifest INPUT_JSON OUTPUT_DIR|installer-qualification-subject ARTIFACT_MANIFEST|write-shared-artifacts-record SHARED_DIR VERSION SOURCE_COMMIT POSTGRES_IMAGE|verify-shared-artifacts SHARED_DIR VERSION SOURCE_COMMIT|verify STAGING_DIR}")
}

// Staging-only files of one guest architecture's artifact set carry the same
// suffix as its published files, so two sets can share one release.
func checksumsFileName(guestArchitecture string) string {
	return "SHA256SUMS" + releasecontract.ArchitectureAssetSuffix(guestArchitecture)
}

func allowlistFileName(guestArchitecture string) string {
	return "candidate-allowlist" + releasecontract.ArchitectureAssetSuffix(guestArchitecture) + ".json"
}

func ociMetadataFileName(image, guestArchitecture string) string {
	return image + releasecontract.ArchitectureAssetSuffix(guestArchitecture) + ".oci.json"
}

// checkSharedArtifactFiles authenticates a shared artifacts directory apart
// from its record: exactly the shared files of version as regular files with
// their staged modes, package metadata of this release, and an install
// bootstrap that embeds this version and the Linux amd64 deploy binary digest.
func checkSharedArtifactFiles(directory, version, sourceCommit string, withRecord bool) (map[string]string, error) {
	modes := releasecontract.SharedArtifactFileModes(version)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("shared artifacts directory: %w", err)
	}
	present := map[string]bool{}
	recordPresent := false
	for _, entry := range entries {
		name := entry.Name()
		if name == releasecontract.SharedArtifactsRecordFileName && withRecord {
			if !entry.Type().IsRegular() {
				return nil, fmt.Errorf("shared artifacts record %s is not a regular file", name)
			}
			recordPresent = true
			continue
		}
		if _, ok := modes[name]; !ok {
			return nil, fmt.Errorf("shared artifacts directory contains unknown entry %s", name)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != modes[name] {
			return nil, fmt.Errorf("shared artifact %s must be a regular file with mode %04o, found %s", name, modes[name], info.Mode())
		}
		present[name] = true
	}
	digests := make(map[string]string, len(modes))
	for name := range modes {
		if !present[name] {
			return nil, fmt.Errorf("shared artifacts directory lacks %s", name)
		}
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		digests[name] = releasecontract.Digest(content)
	}
	if withRecord && !recordPresent {
		return nil, errors.New("shared artifacts directory lacks its record")
	}
	if err := verifyPackageMetadata(directory, version, sourceCommit); err != nil {
		return nil, err
	}
	bootstrap, err := os.ReadFile(filepath.Join(directory, "install.sh"))
	if err != nil {
		return nil, err
	}
	deployDigest := strings.TrimPrefix(digests[releasecontract.HostBinaryFileName(version, "secondbox-deploy", "linux/amd64")], "sha256:")
	for _, line := range []string{"version='" + version + "'", "expected_sha256='" + deployDigest + "'"} {
		if !strings.Contains("\n"+string(bootstrap)+"\n", "\n"+line+"\n") {
			return nil, fmt.Errorf("shared install bootstrap lacks %s", line)
		}
	}
	return digests, nil
}

// writeSharedArtifactsRecord records the shared files that release-build-shared.sh
// produced in directory for version, sourceCommit and the bundled Postgres image.
func writeSharedArtifactsRecord(directory, version, sourceCommit, postgresImage string) error {
	digests, err := checkSharedArtifactFiles(directory, version, sourceCommit, false)
	if err != nil {
		return err
	}
	record := releasecontract.SharedArtifactsRecord{SchemaVersion: releasecontract.SharedArtifactsSchema, Identity: releasecontract.Identity{Version: version, Tag: "v" + version, SourceCommit: sourceCommit}, PostgresImage: postgresImage}
	for _, name := range releasecontract.SharedArtifactFileNames(version) {
		record.Files = append(record.Files, releasecontract.SharedArtifactFile{Name: name, Digest: digests[name]})
	}
	if err := record.ValidateForRelease(version, sourceCommit); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, releasecontract.SharedArtifactsRecordFileName), append(encoded, '\n'), 0o644)
}

// verifySharedArtifacts authenticates a shared artifacts directory for the
// release a staging builds and prints the shared file names it stages.
func verifySharedArtifacts(directory, version, sourceCommit string) error {
	data, err := os.ReadFile(filepath.Join(directory, releasecontract.SharedArtifactsRecordFileName))
	if err != nil {
		return fmt.Errorf("shared artifacts record: %w", err)
	}
	record, err := releasecontract.DecodeSharedArtifactsRecord(data)
	if err != nil {
		return err
	}
	if err := record.ValidateForRelease(version, sourceCommit); err != nil {
		return err
	}
	digests, err := checkSharedArtifactFiles(directory, version, sourceCommit, true)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(record.Files))
	for _, file := range record.Files {
		if digests[file.Name] != file.Digest {
			return fmt.Errorf("shared artifact %s digest differs from the shared artifacts record", file.Name)
		}
		names = append(names, file.Name)
	}
	_, err = fmt.Fprintln(os.Stdout, strings.Join(names, "\n"))
	return err
}

func writeInstallerQualificationSubject(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		return err
	}
	digest, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, digest)
	return err
}

func writeStandardDocuments(architecture, outputDirectory string) error {
	documents, err := standardresources.Documents(architecture)
	if err != nil {
		return err
	}
	for _, document := range documents {
		data, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outputDirectory, releasecontract.StandardBundleFileName(document.Name, architecture)), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeManifest(inputPath, outputDirectory string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	var input candidateInput
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	tag := "v" + input.Version
	parsed, err := releasecontract.ParseTag(tag)
	if err != nil || parsed != input.Version {
		return fmt.Errorf("release version: %w", err)
	}
	identity := releasecontract.Identity{Version: input.Version, Tag: tag, SourceCommit: input.SourceCommit}
	if _, err := standardresources.StandardPool(input.GuestArchitecture); err != nil {
		return fmt.Errorf("release guest architecture: %w", err)
	}
	guestPlatform := "linux/" + input.GuestArchitecture
	ref := func(name string) (releasecontract.Reference, error) {
		content, err := os.ReadFile(filepath.Join(outputDirectory, name))
		if err != nil {
			return releasecontract.Reference{}, err
		}
		return releasecontract.Reference{Location: releasecontract.ReleaseFileLocation(input.Version, name), Digest: releasecontract.Digest(content)}, nil
	}
	openapiName := fmt.Sprintf("secondbox-%s-openapi.json", input.Version)
	openapi, err := ref(openapiName)
	if err != nil {
		return err
	}
	goPackage, err := ref(fmt.Sprintf("secondbox-%s-go-module.tar.gz", input.Version))
	if err != nil {
		return err
	}
	tsName := fmt.Sprintf("secondstack-ai-secondbox-%s.tgz", input.Version)
	tsPackage, err := ref(tsName)
	if err != nil {
		return err
	}
	binaries := make([]releasecontract.BinaryArtifact, 0, len(releasecontract.HostBinaryNames)*len(releasecontract.HostBinaryPlatforms))
	for _, name := range releasecontract.HostBinaryNames {
		for _, platform := range releasecontract.HostBinaryPlatforms {
			content, err := os.ReadFile(filepath.Join(outputDirectory, releasecontract.HostBinaryFileName(input.Version, name, platform)))
			if err != nil {
				return err
			}
			binaries = append(binaries, releasecontract.BinaryArtifact{Identity: identity, Name: name, Platform: platform, Location: releasecontract.BinaryLocation(input.Version, name, platform), SHA256: strings.TrimPrefix(releasecontract.Digest(content), "sha256:")})
		}
	}
	sbom, err := ref(fmt.Sprintf("secondbox-%s.spdx.json", input.Version))
	if err != nil {
		return err
	}
	if err := verifyQualificationEvidence(outputDirectory, input.Version, input.SourceCommit, input.GuestArchitecture); err != nil {
		return err
	}
	qualificationEvidence, err := ref(releasecontract.QualificationEvidenceFileName(input.Version, input.GuestArchitecture))
	if err != nil {
		return err
	}
	var installerQualificationEvidence releasecontract.Reference
	if !input.Candidate && input.GuestArchitecture == standardresources.ArchitectureAMD64 {
		if err := verifyInstallerQualificationEvidenceSource(outputDirectory, input.Version, input.SourceCommit); err != nil {
			return err
		}
		installerQualificationEvidence, err = ref(fmt.Sprintf("secondbox-%s-installer-qualification-evidence.json", input.Version))
		if err != nil {
			return err
		}
	}
	installBootstrap, err := ref("install.sh")
	if err != nil {
		return err
	}
	// gVisor ships for amd64 guests only.
	var gvisor *releasecontract.GVisorArtifact
	if input.GuestArchitecture == standardresources.ArchitectureAMD64 {
		gvisorMaterialization, err := ref(fmt.Sprintf("secondbox-%s-gvisor-materialization.json", input.Version))
		if err != nil {
			return err
		}
		gvisorEvidence, err := ref(fmt.Sprintf("secondbox-%s-gvisor-qualification-evidence.json", input.Version))
		if err != nil {
			return err
		}
		var gvisorPodEvidence releasecontract.Reference
		if len(input.ImagePlatforms) == 2 {
			gvisorPodEvidence, err = ref(fmt.Sprintf("secondbox-%s-gvisor-pod-qualification-evidence.json", input.Version))
			if err != nil {
				return err
			}
		}
		gvisor = &releasecontract.GVisorArtifact{Identity: identity, RunnerReference: releasecontract.GVisorRunnerImage + "@" + input.GVisorRunnerDigest, ImageReference: releasecontract.GVisorImage + "@" + input.GVisorImageDigest, Materialization: gvisorMaterialization, MaterializationDigest: input.GVisorMaterializationDigest, FlatRootDigest: input.GVisorFlatRootDigest, RunscRelease: input.GVisorRunscRelease, QualificationEvidence: gvisorEvidence, PodQualificationEvidence: gvisorPodEvidence}
	}
	bundles := make([]releasecontract.StandardBundleArtifact, 0, len(standardresources.BundleNames()))
	for _, name := range standardresources.BundleNames() {
		filename := releasecontract.StandardBundleFileName(name, input.GuestArchitecture)
		bundleRef, err := ref(filename)
		if err != nil {
			return err
		}
		bundleData, err := os.ReadFile(filepath.Join(outputDirectory, filename))
		if err != nil {
			return err
		}
		bundle, err := standardresources.DecodeDocument(bundleData)
		if err != nil {
			return err
		}
		profiles := make([]releasecontract.StandardProfileIdentity, 0, len(bundle.Profile.Revisions))
		for _, revision := range bundle.Profile.Revisions {
			profiles = append(profiles, releasecontract.StandardProfileIdentity{Name: bundle.Profile.Name, Revision: revision.Number, SpecDigest: revision.SpecDigest})
		}
		bundles = append(bundles, releasecontract.StandardBundleArtifact{Identity: identity, Name: name, Document: bundleRef, Profiles: profiles})
	}
	manifest := releasecontract.ArtifactManifest{SchemaVersion: releasecontract.ArtifactManifestSchema, Candidate: input.Candidate, Identity: identity, OpenAPI: releasecontract.OpenAPIArtifact{Identity: identity, Reference: openapi}, RunnerProtocol: releasecontract.ProtocolWindow{Minimum: runnerv1.SupportedProtocolMinimum, Maximum: runnerv1.SupportedProtocolMaximum}, GuestProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1}, Platforms: releasecontract.PlatformMatrix{HostBinaries: releasecontract.HostBinaryPlatforms, ControlPlane: input.ImagePlatforms, Runner: []string{guestPlatform}, InstallerTools: []string{guestPlatform}, Guest: []string{guestPlatform}, QualifiedRunnerGuest: []string{guestPlatform}}, GoSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.GoModule + "@" + tag, Package: goPackage}, TypeScriptSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.TypeScriptPackage + "@" + input.Version, Package: tsPackage}, ControlPlane: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.ControlPlaneImage + "@" + input.ControlPlaneDigest}, Runner: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.RunnerImage + "@" + input.RunnerDigest}, InstallerTools: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.InstallerToolsImage + "@" + input.InstallerToolsDigest}, BundledServices: releasecontract.BundledServiceImages{Postgres: input.PostgresImage}, InstallBootstrap: installBootstrap, MicroVM: releasecontract.MicroVMArtifact{Identity: identity, ImageReference: releasecontract.MicroVMImage + "@" + input.MicroVMImageDigest, BundleManifestDigest: input.MicroVMManifestDigest, RuntimeBundle: input.MicroVMRuntimeBundle, ToolchainBundle: input.MicroVMToolchainBundle}, GVisor: gvisor, Binaries: binaries, SBOMs: []releasecontract.Reference{sbom}, QualificationEvidence: qualificationEvidence, InstallerQualificationEvidence: installerQualificationEvidence, StandardBundles: bundles}
	if err := manifest.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outputDirectory, releasecontract.ArtifactManifestFileName(input.Version, input.GuestArchitecture)), append(encoded, '\n'), 0o644)
}

func verifyCandidate(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var manifestPath string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-artifact-manifest.json") {
			if manifestPath != "" {
				return errors.New("release candidate contains multiple artifact manifests")
			}
			manifestPath = filepath.Join(directory, entry.Name())
		}
	}
	if manifestPath == "" {
		return errors.New("release candidate artifact manifest is absent")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		return err
	}
	guestArchitecture, err := manifest.GuestArchitecture()
	if err != nil {
		return err
	}
	if filepath.Base(manifestPath) != releasecontract.ArtifactManifestFileName(manifest.Version, guestArchitecture) {
		return errors.New("release candidate artifact manifest filename differs from its version and guest architecture")
	}
	allowlistData, err := os.ReadFile(filepath.Join(directory, allowlistFileName(guestArchitecture)))
	if err != nil {
		return fmt.Errorf("release candidate allowlist: %w", err)
	}
	var allowlist struct {
		SchemaVersion int      `json:"schemaVersion"`
		Files         []string `json:"files"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(allowlistData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&allowlist); err != nil || allowlist.SchemaVersion != 1 || len(allowlist.Files) == 0 {
		return errors.New("release candidate allowlist is invalid")
	}
	actualFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("release candidate contains directory %s", entry.Name())
		}
		actualFiles = append(actualFiles, entry.Name())
	}
	// The allowlist names a set of files, not an order. Comparing it sorted keeps
	// the candidate contract exact while leaving the staging script's collation
	// and the directory read order free to disagree.
	expectedFiles := append([]string(nil), allowlist.Files...)
	sort.Strings(expectedFiles)
	sort.Strings(actualFiles)
	if strings.Join(actualFiles, "\x00") != strings.Join(expectedFiles, "\x00") {
		return errors.New("release candidate contains missing or unknown files")
	}
	if err := verifyChecksums(directory, allowlist.Files, guestArchitecture); err != nil {
		return err
	}
	if err := verifyCandidateMetadata(directory, manifest, guestArchitecture); err != nil {
		return err
	}
	refs := []releasecontract.Reference{manifest.OpenAPI.Reference, manifest.GoSDK.Package, manifest.TypeScriptSDK.Package, manifest.InstallBootstrap}
	refs = append(refs, manifest.SBOMs...)
	refs = append(refs, manifest.ArtifactAttestations...)
	refs = append(refs, manifest.QualificationEvidence)
	requiresInstallerQualification, err := manifest.RequiresInstallerQualification()
	if err != nil {
		return err
	}
	if requiresInstallerQualification {
		refs = append(refs, manifest.InstallerQualificationEvidence)
	}
	if manifest.GVisor != nil {
		refs = append(refs, manifest.GVisor.Materialization, manifest.GVisor.QualificationEvidence)
		if manifest.GVisor.PodQualificationEvidence != (releasecontract.Reference{}) {
			refs = append(refs, manifest.GVisor.PodQualificationEvidence)
		}
		// The staged documents must already satisfy public release
		// verification, not only the staging script's shape checks.
		staged := func(reference releasecontract.Reference) ([]byte, error) {
			return os.ReadFile(filepath.Join(directory, filepath.Base(reference.Location)))
		}
		materialization, err := staged(manifest.GVisor.Materialization)
		if err != nil {
			return err
		}
		if err := manifest.GVisor.VerifyGVisorMaterialization(materialization); err != nil {
			return err
		}
		for pod, reference := range map[bool]releasecontract.Reference{false: manifest.GVisor.QualificationEvidence, true: manifest.GVisor.PodQualificationEvidence} {
			if pod && reference == (releasecontract.Reference{}) {
				continue
			}
			data, err := staged(reference)
			if err != nil {
				return err
			}
			evidence, err := releasecontract.DecodeGVisorQualificationEvidence(data, pod)
			if err != nil {
				return err
			}
			if err := evidence.ValidateForRelease(manifest.SourceCommit, pod); err != nil {
				return err
			}
		}
	}
	for _, bundle := range manifest.StandardBundles {
		refs = append(refs, bundle.Document)
	}
	for _, ref := range refs {
		name := filepath.Base(ref.Location)
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return fmt.Errorf("candidate object %s: %w", name, err)
		}
		if releasecontract.Digest(content) != ref.Digest {
			return fmt.Errorf("candidate object %s digest mismatch", name)
		}
	}
	for _, binary := range manifest.Binaries {
		name := filepath.Base(binary.Location)
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if strings.TrimPrefix(releasecontract.Digest(content), "sha256:") != binary.SHA256 {
			return fmt.Errorf("candidate binary %s digest mismatch", name)
		}
	}
	return nil
}

func verifyChecksums(directory string, allowlist []string, guestArchitecture string) error {
	checksumsName := checksumsFileName(guestArchitecture)
	file, err := os.Open(filepath.Join(directory, checksumsName))
	if err != nil {
		return fmt.Errorf("release candidate checksums: %w", err)
	}
	defer file.Close()
	checksums := make(map[string]string, len(allowlist)-1)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != 64 || filepath.Base(fields[1]) != fields[1] || fields[1] == checksumsName {
			return errors.New("release candidate checksums are malformed")
		}
		if _, exists := checksums[fields[1]]; exists {
			return fmt.Errorf("release candidate checksum for %s is duplicated", fields[1])
		}
		checksums[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	want := append([]string(nil), allowlist...)
	sort.Strings(want)
	for _, name := range want {
		if name == checksumsName || name == allowlistFileName(guestArchitecture) || strings.HasSuffix(name, ".oci.tar") {
			continue
		}
		digest, ok := checksums[name]
		if !ok {
			return fmt.Errorf("release candidate checksum for %s is absent", name)
		}
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if strings.TrimPrefix(releasecontract.Digest(content), "sha256:") != digest {
			return fmt.Errorf("release candidate checksum for %s does not match", name)
		}
		delete(checksums, name)
	}
	if len(checksums) != 0 {
		return errors.New("release candidate checksums contain an unknown file")
	}
	return nil
}

func verifyPackageMetadata(directory, version, sourceCommit string) error {
	packageMetadata := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Version       string `json:"version"`
		SourceCommit  string `json:"sourceCommit"`
	}{}
	data, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf("secondbox-%s-package-metadata.json", version)))
	if err != nil {
		return fmt.Errorf("release package metadata: %w", err)
	}
	if err := json.Unmarshal(data, &packageMetadata); err != nil || packageMetadata.SchemaVersion != 1 || packageMetadata.Version != version || packageMetadata.SourceCommit != sourceCommit {
		return errors.New("release package metadata identity mismatch")
	}
	return nil
}

func verifyCandidateMetadata(directory string, manifest releasecontract.ArtifactManifest, guestArchitecture string) error {
	if err := verifyPackageMetadata(directory, manifest.Version, manifest.SourceCommit); err != nil {
		return err
	}
	if err := verifyQualificationEvidence(directory, manifest.Version, manifest.SourceCommit, guestArchitecture); err != nil {
		return err
	}
	requiresInstallerQualification, err := manifest.RequiresInstallerQualification()
	if err != nil {
		return err
	}
	if requiresInstallerQualification {
		if err := verifyInstallerQualificationEvidence(directory, manifest); err != nil {
			return err
		}
	}
	artifacts := map[string]releasecontract.OCIArtifact{
		ociMetadataFileName("control-plane", guestArchitecture):   manifest.ControlPlane,
		ociMetadataFileName("runner", guestArchitecture):          manifest.Runner,
		ociMetadataFileName("installer-tools", guestArchitecture): manifest.InstallerTools,
	}
	if manifest.GVisor != nil {
		artifacts["runner-gvisor.oci.json"] = releasecontract.OCIArtifact{Identity: manifest.GVisor.Identity, Reference: manifest.GVisor.RunnerReference}
		artifacts["gvisor-artifacts.oci.json"] = releasecontract.OCIArtifact{Identity: manifest.GVisor.Identity, Reference: manifest.GVisor.ImageReference}
	}
	for filename, artifact := range artifacts {
		if err := verifySyntheticOCIMetadata(directory, filename, manifest.Identity, artifact.Reference); err != nil {
			return err
		}
	}
	return verifySyntheticOCIMetadata(directory, ociMetadataFileName("microvm-artifacts", guestArchitecture), manifest.Identity, manifest.MicroVM.ImageReference)
}

func verifyQualificationEvidence(directory, version, sourceCommit, guestArchitecture string) error {
	data, err := os.ReadFile(filepath.Join(directory, releasecontract.QualificationEvidenceFileName(version, guestArchitecture)))
	if err != nil {
		return fmt.Errorf("release qualification evidence: %w", err)
	}
	evidence, err := releasecontract.DecodeQualificationEvidence(data)
	if err != nil {
		return err
	}
	return evidence.ValidateForRelease(sourceCommit, guestArchitecture)
}

func verifyInstallerQualificationEvidenceSource(directory, version, sourceCommit string) error {
	filename := fmt.Sprintf("secondbox-%s-installer-qualification-evidence.json", version)
	data, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil {
		return fmt.Errorf("release installer qualification evidence: %w", err)
	}
	evidence, err := releasecontract.DecodeInstallerQualificationEvidence(data)
	if err != nil {
		return err
	}
	return evidence.ValidateForRelease(sourceCommit, evidence.ReleaseManifestDigest)
}

func verifyInstallerQualificationEvidence(directory string, manifest releasecontract.ArtifactManifest) error {
	filename := fmt.Sprintf("secondbox-%s-installer-qualification-evidence.json", manifest.Version)
	data, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil {
		return fmt.Errorf("release installer qualification evidence: %w", err)
	}
	evidence, err := releasecontract.DecodeInstallerQualificationEvidence(data)
	if err != nil {
		return err
	}
	subject, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		return err
	}
	return evidence.ValidateForRelease(manifest.SourceCommit, subject)
}

func verifySyntheticOCIMetadata(directory, filename string, identity releasecontract.Identity, reference string) error {
	data, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil {
		return fmt.Errorf("release OCI metadata %s: %w", filename, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("release OCI metadata %s is malformed", filename)
	}
	// BuildKit metadata is paired with and verified through the staged OCI layout.
	// Synthetic test metadata carries its image identity directly.
	if _, buildkit := metadata["containerimage.digest"]; buildkit {
		return nil
	}
	digest := strings.TrimPrefix(reference[strings.LastIndex(reference, "@")+1:], "")
	if metadata["version"] != identity.Version || metadata["sourceCommit"] != identity.SourceCommit || metadata["digest"] != digest {
		return fmt.Errorf("release OCI metadata %s identity mismatch", filename)
	}
	return nil
}
