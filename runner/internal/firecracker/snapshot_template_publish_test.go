package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

const snapshotTemplatePublishSchemaVersion = 1

type snapshotTemplatePublishReport struct {
	SchemaVersion   int    `json:"schemaVersion"`
	TemplateID      string `json:"templateId"`
	CacheRoot       string `json:"cacheRoot"`
	ArtifactVersion string `json:"artifactVersion"`
	MemoryMiB       int    `json:"memoryMiB"`
	WorkspaceMiB    int    `json:"workspaceMiB"`
	VCPUCount       int    `json:"vcpuCount"`
	ProcessLimit    int    `json:"processLimit"`
	NetworkDevice   bool   `json:"networkDevice"`
	SharedImage     bool   `json:"sharedImage"`
	BuildMillis     int64  `json:"templateBuildMilliseconds"`
	AdmissionMillis int64  `json:"cacheAdmissionMilliseconds"`
	PublishedAt     string `json:"publishedAt"`
}

// TestSmokePublishSnapshotResumeTemplate is the interim operator flow for a
// populated resume cache, and it is deliberately a harness rather than a Runner
// capability: a Runner does not build its own templates yet, so nothing in a
// production deployment advertises snapshot-resume until an operator publishes
// one.
//
// Everything about the template it publishes follows from the Runner's own
// environment. It loads the Runner's configuration through the same
// LoadRunnerFirecrackerConfigFromEnv the Runner uses, so the kernel arguments,
// CPU template, vsock ports, signed bundle, and trust anchor cannot disagree
// with it, and it derives the compatibility key through the same production
// function the resume start path uses. The only values it takes separately are
// the Profile shape the template is for, because that is the operator's choice
// rather than the Runner's.
//
// It must run as root and jailed. A template captured unjailed records the
// build's absolute drive paths and can never be resumed into a jail at all.
func TestSmokePublishSnapshotResumeTemplate(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_PUBLISH_SNAPSHOT_TEMPLATE") != "1" {
		t.Skip("set SECONDBOX_RUNNER_PUBLISH_SNAPSHOT_TEMPLATE=1 to publish a snapshot-resume template")
	}
	if os.Geteuid() != 0 {
		t.Fatal("publishing a snapshot-resume template must run as root: the jailer chroots, creates device nodes, chowns, and drops UID")
	}
	buildRoot := requiredEnv(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_BUILD_ROOT")
	outputPath := requiredEnv(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_OUTPUT")
	memoryMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_MEMORY_MIB")
	workspaceMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_WORKSPACE_MIB")
	vcpus := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_VCPUS")
	processLimit := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_PROCESS_LIMIT")

	cfg, err := LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		t.Fatalf("load Runner configuration: %v", err)
	}
	if cfg.MicroVMAllowUnjailed {
		t.Fatal("a template must be captured under the jailer: an unjailed capture records absolute drive paths no Instance can own")
	}
	cacheRoot := cfg.MicroVMSnapshotTemplateCacheRoot
	// The build's own state never lands in the Runner's directories, but it must
	// share a filesystem with the cache root: the sealed rootfs is reflinked into
	// the staging directory and the golden memory file is hard-linked out of the
	// jail.
	cfg.MicroVMRunDir = filepath.Join(buildRoot, "run")
	cfg.MicroVMLogDir = filepath.Join(buildRoot, "log")
	cfg.RunnerWorkspaceRoot = filepath.Join(buildRoot, "ws")
	cfg.MicroVMJailerChrootBaseDir = filepath.Join(buildRoot, "j")
	for _, directory := range []string{
		cfg.MicroVMRunDir, cfg.MicroVMLogDir, cfg.RunnerWorkspaceRoot, cfg.MicroVMJailerChrootBaseDir,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create template build directory %q: %v", directory, err)
		}
	}
	// The template's machine shape is the Profile's, because the compatibility
	// key records it and a shape mismatch is a cache miss.
	cfg.MicroVMMemoryMiB = memoryMiB
	cfg.MicroVMWorkspaceSizeMiB = workspaceMiB
	cfg.MicroVMVCPUs = vcpus

	cache, err := NewSnapshotTemplateCache(cacheRoot)
	if err != nil {
		t.Fatalf("open the Runner's snapshot template cache: %v", err)
	}
	manifest, err := loadSignedArtifactManifest(
		filepath.Join(filepath.Dir(cfg.MicroVMKernelPath), "manifest.json"),
	)
	if err != nil {
		t.Fatalf("read the Runner's signed bundle manifest: %v", err)
	}

	buildStartedAt := time.Now()
	key, published := buildSnapshotResumeTemplate(
		t, cfg, cache, memoryMiB, workspaceMiB,
		func(opts runtimemanager.StartOpts) runtimemanager.StartOpts {
			return productionTemplateStartOpts(opts, manifest, cfg, processLimit)
		},
	)
	buildMillis := time.Since(buildStartedAt).Milliseconds()

	// Admitting here rather than leaving it to the first start pays the one-time
	// full digest verification of the published files where an operator can see
	// it, and proves the Runner will be able to admit exactly what was published.
	admissionStartedAt := time.Now()
	if _, err := cache.Resolve(key); err != nil {
		t.Fatalf("admit the published template: %v", err)
	}
	admissionMillis := time.Since(admissionStartedAt).Milliseconds()

	ready, err := cache.HasTemplateForBundle(SnapshotTemplateBundleIdentity{
		ArtifactVersion:       manifest.ArtifactVersion,
		Architecture:          manifest.Architecture,
		RuntimeBundleDigest:   manifest.RuntimeBundle.ManifestDigest,
		ToolchainBundleDigest: manifest.ToolchainBundle.ManifestDigest,
	})
	if err != nil || !ready {
		t.Fatalf("the published template does not make this Runner snapshot-resume ready: ready=%t err=%v", ready, err)
	}

	report := snapshotTemplatePublishReport{
		SchemaVersion:   snapshotTemplatePublishSchemaVersion,
		TemplateID:      published.TemplateID,
		CacheRoot:       cacheRoot,
		ArtifactVersion: manifest.ArtifactVersion,
		MemoryMiB:       memoryMiB,
		WorkspaceMiB:    workspaceMiB,
		VCPUCount:       vcpus,
		ProcessLimit:    processLimit,
		NetworkDevice:   key.HasNetworkDevice(),
		SharedImage:     key.HasSharedImage(),
		BuildMillis:     buildMillis,
		AdmissionMillis: admissionMillis,
		PublishedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode template publish report: %v", err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write template publish report: %v", err)
	}
	if err := syncDirectory(filepath.Dir(outputPath)); err != nil {
		t.Fatalf("sync template publish report directory: %v", err)
	}
	t.Logf(
		"published snapshot-resume template %s into %s in %d ms, admitted in %d ms",
		published.TemplateID[:16], cacheRoot, buildMillis, admissionMillis,
	)
}

// productionTemplateStartOpts restates the launch options exactly as
// AssignmentBackend.StartAssignment produces them for a Profile of this shape.
// The signed identity comes from the Runner's own verified manifest, which is
// what matchSignedAssignmentComponent proves every assignment against, so a
// template built here is keyed to the assignments this Runner will accept.
func productionTemplateStartOpts(
	opts runtimemanager.StartOpts,
	manifest signedArtifactManifest,
	cfg *config.Config,
	processLimit int,
) runtimemanager.StartOpts {
	opts.GuestBuildID = manifest.ArtifactVersion
	opts.ImageManifestDigest = manifest.RuntimeBundle.ManifestDigest
	opts.ToolchainManifestDigest = manifest.ToolchainBundle.ManifestDigest
	opts.RuntimeClass = runtimemanager.RuntimeClassToolExecutor
	opts.SandboxPolicy = &runtimemanager.SandboxRuntimePolicy{
		VCPUs:             cfg.MicroVMVCPUs,
		CPUMillis:         cfg.MicroVMVCPUs * 1000,
		MemoryMiB:         cfg.MicroVMMemoryMiB,
		WorkspaceSizeMiB:  cfg.MicroVMWorkspaceSizeMiB,
		ProcessLimit:      processLimit,
		WorkspaceWritable: true,
		SharedReadOnly:    true,
	}
	return opts
}
