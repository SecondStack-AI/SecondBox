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

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

type candidateInput struct {
	Candidate                    bool                            `json:"candidate"`
	Version                      string                          `json:"version"`
	SourceCommit                 string                          `json:"sourceCommit"`
	ControlPlaneDigest           string                          `json:"controlPlaneDigest"`
	RunnerDigest                 string                          `json:"runnerDigest"`
	InstallerToolsDigest         string                          `json:"installerToolsDigest"`
	PostgresImage                string                          `json:"postgresImage"`
	ObjectStoreImage             string                          `json:"objectStoreImage"`
	ObjectStoreClientImage       string                          `json:"objectStoreClientImage"`
	MicroVMImageDigest           string                          `json:"microvmImageDigest"`
	MicroVMManifestDigest        string                          `json:"microvmManifestDigest"`
	MicroVMSigningKeyFingerprint string                          `json:"microvmSigningKeyFingerprint"`
	MicroVMRuntimeBundle         releasecontract.SignedComponent `json:"microvmRuntimeBundle"`
	MicroVMToolchainBundle       releasecontract.SignedComponent `json:"microvmToolchainBundle"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 5 && args[0] == "standard-documents" {
		return writeStandardDocuments(args[1], args[2], args[3], args[4])
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
	return errors.New("usage: secondbox-release-tool {standard-documents SIGNED_MANIFEST_DIGEST RUNTIME_BUNDLE_DIGEST TOOLCHAIN_BUNDLE_DIGEST OUTPUT_DIR|manifest INPUT_JSON OUTPUT_DIR|installer-qualification-subject ARTIFACT_MANIFEST|verify STAGING_DIR}")
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

func writeStandardDocuments(signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest, outputDirectory string) error {
	documents, err := standardresources.Documents(signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest)
	if err != nil {
		return err
	}
	for _, document := range documents {
		data, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outputDirectory, document.Name+".standard-bundle.json"), append(data, '\n'), 0o644); err != nil {
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
	ref := func(name string) (releasecontract.Reference, error) {
		content, err := os.ReadFile(filepath.Join(outputDirectory, name))
		if err != nil {
			return releasecontract.Reference{}, err
		}
		return releasecontract.Reference{Location: releasecontract.ArtifactManifestLocation(input.Version)[:strings.LastIndex(releasecontract.ArtifactManifestLocation(input.Version), "/")+1] + name, Digest: releasecontract.Digest(content)}, nil
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
	binaries := make([]releasecontract.BinaryArtifact, 0, 8)
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
			filename := fmt.Sprintf("%s_%s_%s", name, input.Version, strings.ReplaceAll(platform, "/", "_"))
			content, err := os.ReadFile(filepath.Join(outputDirectory, filename))
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
	if err := verifyQualificationEvidence(outputDirectory, input.Version, input.SourceCommit); err != nil {
		return err
	}
	qualificationEvidence, err := ref(fmt.Sprintf("secondbox-%s-qualification-evidence.json", input.Version))
	if err != nil {
		return err
	}
	var installerQualificationEvidence releasecontract.Reference
	if !input.Candidate {
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
	bundles := make([]releasecontract.StandardBundleArtifact, 0, 2)
	for _, name := range []string{standardresources.AgentCompartment, standardresources.DurableCoding} {
		filename := name + ".standard-bundle.json"
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
	manifest := releasecontract.ArtifactManifest{SchemaVersion: releasecontract.ArtifactManifestSchema, Candidate: input.Candidate, Identity: identity, OpenAPI: releasecontract.OpenAPIArtifact{Identity: identity, Reference: openapi}, RunnerProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1}, GuestProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1}, Platforms: releasecontract.PlatformMatrix{HostBinaries: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}, ControlPlane: []string{"linux/amd64", "linux/arm64"}, Runner: []string{"linux/amd64"}, InstallerTools: []string{"linux/amd64"}, Guest: []string{"linux/amd64"}, QualifiedRunnerGuest: []string{"linux/amd64"}}, GoSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.GoModule + "@" + tag, Package: goPackage}, TypeScriptSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.TypeScriptPackage + "@" + input.Version, Package: tsPackage}, ControlPlane: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.ControlPlaneImage + "@" + input.ControlPlaneDigest}, Runner: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.RunnerImage + "@" + input.RunnerDigest}, InstallerTools: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.InstallerToolsImage + "@" + input.InstallerToolsDigest}, BundledServices: releasecontract.BundledServiceImages{Postgres: input.PostgresImage, ObjectStore: input.ObjectStoreImage, ObjectStoreClient: input.ObjectStoreClientImage}, InstallBootstrap: installBootstrap, MicroVM: releasecontract.MicroVMArtifact{Identity: identity, ImageReference: releasecontract.MicroVMImage + "@" + input.MicroVMImageDigest, SignedManifestDigest: input.MicroVMManifestDigest, SigningKeyFingerprint: "SHA256:" + strings.ToUpper(input.MicroVMSigningKeyFingerprint), RuntimeBundle: input.MicroVMRuntimeBundle, ToolchainBundle: input.MicroVMToolchainBundle}, Binaries: binaries, SBOMs: []releasecontract.Reference{sbom}, QualificationEvidence: qualificationEvidence, InstallerQualificationEvidence: installerQualificationEvidence, StandardBundles: bundles}
	if err := manifest.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outputDirectory, fmt.Sprintf("secondbox-%s-artifact-manifest.json", input.Version)), append(encoded, '\n'), 0o644)
}

func verifyCandidate(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	allowlistData, err := os.ReadFile(filepath.Join(directory, "candidate-allowlist.json"))
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
	if err := verifyChecksums(directory, allowlist.Files); err != nil {
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
	if err := verifyCandidateMetadata(directory, manifest); err != nil {
		return err
	}
	refs := []releasecontract.Reference{manifest.OpenAPI.Reference, manifest.GoSDK.Package, manifest.TypeScriptSDK.Package, manifest.InstallBootstrap}
	refs = append(refs, manifest.SBOMs...)
	refs = append(refs, manifest.ArtifactAttestations...)
	refs = append(refs, manifest.QualificationEvidence)
	if !manifest.Candidate {
		refs = append(refs, manifest.InstallerQualificationEvidence)
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

func verifyChecksums(directory string, allowlist []string) error {
	file, err := os.Open(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("release candidate checksums: %w", err)
	}
	defer file.Close()
	checksums := make(map[string]string, len(allowlist)-1)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != 64 || filepath.Base(fields[1]) != fields[1] || fields[1] == "SHA256SUMS" {
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
		if name == "SHA256SUMS" || name == "candidate-allowlist.json" || strings.HasSuffix(name, ".oci.tar") {
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

func verifyCandidateMetadata(directory string, manifest releasecontract.ArtifactManifest) error {
	packageMetadata := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Version       string `json:"version"`
		SourceCommit  string `json:"sourceCommit"`
	}{}
	data, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf("secondbox-%s-package-metadata.json", manifest.Version)))
	if err != nil {
		return fmt.Errorf("release package metadata: %w", err)
	}
	if err := json.Unmarshal(data, &packageMetadata); err != nil || packageMetadata.SchemaVersion != 1 || packageMetadata.Version != manifest.Version || packageMetadata.SourceCommit != manifest.SourceCommit {
		return errors.New("release package metadata identity mismatch")
	}
	if err := verifyQualificationEvidence(directory, manifest.Version, manifest.SourceCommit); err != nil {
		return err
	}
	if !manifest.Candidate {
		if err := verifyInstallerQualificationEvidence(directory, manifest); err != nil {
			return err
		}
	}
	for filename, artifact := range map[string]releasecontract.OCIArtifact{
		"control-plane.oci.json":   manifest.ControlPlane,
		"runner.oci.json":          manifest.Runner,
		"installer-tools.oci.json": manifest.InstallerTools,
	} {
		if err := verifySyntheticOCIMetadata(directory, filename, manifest.Identity, artifact.Reference); err != nil {
			return err
		}
	}
	return verifySyntheticOCIMetadata(directory, "microvm-artifacts.oci.json", manifest.Identity, manifest.MicroVM.ImageReference)
}

func verifyQualificationEvidence(directory, version, sourceCommit string) error {
	filename := fmt.Sprintf("secondbox-%s-qualification-evidence.json", version)
	data, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil {
		return fmt.Errorf("release qualification evidence: %w", err)
	}
	evidence, err := releasecontract.DecodeQualificationEvidence(data)
	if err != nil {
		return err
	}
	return evidence.ValidateForRelease(sourceCommit)
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
