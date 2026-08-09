package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func guidedFacts() install.HostFacts {
	return install.HostFacts{SchemaVersion: install.HostFactsSchema, ObservedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), HostIdentity: "machine-id:guided", OS: "linux", Architecture: "amd64", InvokingUID: int64(os.Getuid()), InvokingGID: int64(os.Getgid()), KernelVersion: "6.12", CgroupVersion: 2, CgroupControllers: []string{"cpu", "memory", "pids", "io"}, CPUCount: 8, MemoryBytes: 32 << 30, Virtualization: "hardware", BtrfsSupported: true, KVMAccessible: true, TUNAccessible: true, Devices: []install.DeviceFact{}, ListeningPorts: []install.PortFact{}, Routes: []install.RouteFact{{Destination: "192.0.2.0/24", Interface: "eth0"}}, DNSUpstreams: []string{"192.0.2.53"}, AssignedUIDs: []int64{0, 1000}, CandidateUIDRanges: []install.UIDRange{{Start: 200000, Count: 64}}, Utilities: map[string]string{"docker": "/usr/bin/docker"}, Findings: []install.Finding{{ID: "platform", Class: install.FindingPass, Summary: "Linux amd64 host"}}}
}

func fakeGuidedRelease() releaseverify.VerifiedRelease {
	fingerprint := "SHA256:" + strings.Repeat("A", 64)
	return releaseverify.VerifiedRelease{Manifest: releasecontract.ArtifactManifest{Identity: releasecontract.Identity{Version: "0.4.0", Tag: "v0.4.0", SourceCommit: strings.Repeat("a", 40)}, ControlPlane: releasecontract.OCIArtifact{Reference: "ghcr.io/secondstack-ai/secondbox/control-plane@sha256:" + strings.Repeat("b", 64)}, Runner: releasecontract.OCIArtifact{Reference: "ghcr.io/secondstack-ai/secondbox/runner@sha256:" + strings.Repeat("c", 64)}, InstallerTools: releasecontract.OCIArtifact{Reference: "ghcr.io/secondstack-ai/secondbox/installer-tools@sha256:" + strings.Repeat("1", 64)}, BundledServices: releasecontract.BundledServiceImages{Postgres: "docker.io/library/postgres@sha256:" + strings.Repeat("2", 64), ObjectStore: "docker.io/rustfs/rustfs@sha256:" + strings.Repeat("3", 64), ObjectStoreClient: "quay.io/minio/mc@sha256:" + strings.Repeat("4", 64)}, MicroVM: releasecontract.MicroVMArtifact{ImageReference: "ghcr.io/secondstack-ai/secondbox/microvm-artifacts@sha256:" + strings.Repeat("d", 64), SigningKeyFingerprint: fingerprint}, Binaries: []releasecontract.BinaryArtifact{{Name: "secondbox", Platform: "linux/amd64", SHA256: strings.Repeat("e", 64)}, {Name: "secondbox-deploy", Platform: "linux/amd64", SHA256: strings.Repeat("f", 64)}}}, ManifestBytes: []byte("verified manifest bytes")}
}

func usePublishedGuidedReleaseBuild(t *testing.T) {
	t.Helper()
	originalVersion, originalCommit := buildinfo.Version, buildinfo.SourceCommit
	buildinfo.Version = "0.4.0"
	buildinfo.SourceCommit = strings.Repeat("a", 40)
	t.Cleanup(func() {
		buildinfo.Version = originalVersion
		buildinfo.SourceCommit = originalCommit
	})
}

func TestGuidedInstallAccessibleAcceptsAndPersistsCanonicalPlan(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	home := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	var output, diagnostic bytes.Buffer
	capabilities := cliui.ForWriter(&output, &diagnostic)
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	dependencies := guidedInstallDependencies{
		Input:          strings.NewReader("1\ny\n1\ny\ny\n"),
		Now:            func() time.Time { return now },
		HomeDirectory:  func() (string, error) { return home, nil },
		AvailableBytes: func(string) (int64, error) { return 100 << 30, nil },
		VerifyRelease:  func(context.Context, string) (releaseverify.VerifiedRelease, error) { return fakeGuidedRelease(), nil },
		OperationID:    func() (string, error) { return "install_0123456789abcdef", nil },
		MakeDirectory:  func(path string) error { return os.Mkdir(path, 0o700) },
		WriteAccepted:  install.WriteAccepted,
		HostApply:      func(context.Context, string, string) error { return nil },
		RunForm: func(ctx context.Context, form cliui.Form, handles cliui.FormHandles) error {
			return form.Run(ctx, handles)
		},
	}
	if err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), false, dependencies); err != nil {
		t.Fatalf("%v\ndiagnostic:\n%s", err, diagnostic.String())
	}
	operationDirectory := filepath.Join(home, "secondbox-install_0123456789abcdef")
	plan, _, err := install.ReadPlan(filepath.Join(operationDirectory, "install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	receiptBytes, err := os.ReadFile(filepath.Join(operationDirectory, "install-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := install.DecodeReceipt(receiptBytes, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.CompletedStages) != 2 || receipt.CompletedStages[1].Stage != install.StagePlanAccepted || plan.Storage.Choice != install.StorageBtrfsImage {
		t.Fatalf("accepted plan/receipt = %#v %#v", plan.Storage, receipt.CompletedStages)
	}
	if !strings.Contains(output.String(), "Host preparation complete") || !strings.Contains(diagnostic.String(), "Privileged host actions accepted for sudo") || strings.Contains(output.String()+diagnostic.String(), "Bearer ") {
		t.Fatalf("output = %q; diagnostic = %q", output.String(), diagnostic.String())
	}
}

func TestGuidedInstallOffersAndPersistsExistingReflinkFilesystem(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	home := t.TempDir()
	facts := guidedFacts()
	facts.Devices = []install.DeviceFact{{Path: "/dev/sdb", Identity: "8:16", Filesystem: "xfs", SizeBytes: 200 << 30, AvailableBytes: 160 << 30, Mountpoint: "/srv/secondbox-dedicated", JailerCompatible: true}}
	capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	dependencies := guidedInstallDependencies{
		Input: strings.NewReader("1\ny\n1\ny\ny\n"), Now: time.Now,
		HomeDirectory:  func() (string, error) { return home, nil },
		AvailableBytes: func(string) (int64, error) { return 100 << 30, nil },
		VerifyRelease:  func(context.Context, string) (releaseverify.VerifiedRelease, error) { return fakeGuidedRelease(), nil },
		OperationID:    func() (string, error) { return "install_0123456789abcdef", nil },
		MakeDirectory:  func(path string) error { return os.Mkdir(path, 0o700) },
		WriteAccepted:  install.WriteAccepted,
		HostApply:      func(context.Context, string, string) error { return nil },
		RunForm: func(ctx context.Context, form cliui.Form, handles cliui.FormHandles) error {
			return form.Run(ctx, handles)
		},
	}
	if err := runGuidedInstallWith(context.Background(), renderer, facts, false, dependencies); err != nil {
		t.Fatal(err)
	}
	plan, _, err := install.ReadPlan(filepath.Join(home, "secondbox-install_0123456789abcdef", "install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Storage.Choice != install.StorageExistingMount || plan.Storage.ExistingDeviceIdentity != "8:16" || !strings.HasPrefix(plan.Storage.WorkspacePath, "/srv/secondbox-dedicated/") {
		t.Fatalf("existing filesystem plan = %#v", plan.Storage)
	}
}

func TestGuidedInstallRejectsNonTTYAndJSONBeforeReleaseFetch(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	for _, test := range []struct {
		name       string
		outputMode cliui.OutputMode
	}{
		{"non-TTY", cliui.OutputPlain},
		{"JSON", cliui.OutputJSON},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
			renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: test.outputMode, ColorMode: cliui.ColorNever}
			dependencies := systemGuidedInstallDependencies()
			dependencies.VerifyRelease = func(context.Context, string) (releaseverify.VerifiedRelease, error) {
				t.Fatal("release fetch ran")
				return releaseverify.VerifiedRelease{}, nil
			}
			if err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), false, dependencies); err == nil {
				t.Fatal("unattended invocation succeeded")
			}
		})
	}
}

func TestGuidedInstallDevelopmentBuildFailsBeforeReleaseFetch(t *testing.T) {
	originalVersion, originalCommit := buildinfo.Version, buildinfo.SourceCommit
	buildinfo.Version, buildinfo.SourceCommit = "0.0.0-development", "development"
	t.Cleanup(func() { buildinfo.Version, buildinfo.SourceCommit = originalVersion, originalCommit })
	capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	dependencies := systemGuidedInstallDependencies()
	dependencies.VerifyRelease = func(context.Context, string) (releaseverify.VerifiedRelease, error) {
		t.Fatal("development build attempted a release fetch")
		return releaseverify.VerifiedRelease{}, nil
	}
	err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), false, dependencies)
	if err == nil || !strings.Contains(err.Error(), "requires a published qualified release binary") || strings.Contains(err.Error(), "releases/download/v0.0.0-development") {
		t.Fatalf("development build error = %v", err)
	}
}

func TestComposeCIDRValidatorRequiresPrivateSlash24(t *testing.T) {
	if err := validateInstallerComposeCIDR("10.42.0.0/24"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"10.42.0.0/30", "198.51.100.0/24"} {
		if err := validateInstallerComposeCIDR(value); err == nil {
			t.Fatalf("Compose CIDR %q passed validation", value)
		}
	}
}

func TestGuidedInstallEOFDoesNotCreateOperationDirectory(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	created := false
	capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	dependencies := systemGuidedInstallDependencies()
	dependencies.Input = strings.NewReader("")
	dependencies.HomeDirectory = func() (string, error) { return t.TempDir(), nil }
	dependencies.AvailableBytes = func(string) (int64, error) { return 100 << 30, nil }
	dependencies.VerifyRelease = func(context.Context, string) (releaseverify.VerifiedRelease, error) { return fakeGuidedRelease(), nil }
	dependencies.OperationID = func() (string, error) { return "install_0123456789abcdef", nil }
	dependencies.MakeDirectory = func(string) error { created = true; return nil }
	if err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), false, dependencies); err == nil {
		t.Fatal("EOF succeeded")
	}
	if created {
		t.Fatal("EOF caused a persistent mutation")
	}
}

func TestGuidedInstallAdvancedReviewUsesSharedFormsAndPersistsOverrides(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	home := t.TempDir()
	capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	formCalls := 0
	dependencies := guidedInstallDependencies{
		Input: strings.NewReader(""), Now: time.Now,
		HomeDirectory:  func() (string, error) { return home, nil },
		AvailableBytes: func(string) (int64, error) { return 100 << 30, nil },
		VerifyRelease:  func(context.Context, string) (releaseverify.VerifiedRelease, error) { return fakeGuidedRelease(), nil },
		OperationID:    func() (string, error) { return "install_0123456789abcdef", nil },
		MakeDirectory:  func(path string) error { return os.Mkdir(path, 0o700) },
		WriteAccepted:  install.WriteAccepted,
		HostApply:      func(context.Context, string, string) error { return nil },
		RunForm: func(_ context.Context, form cliui.Form, _ cliui.FormHandles) error {
			formCalls++
			spec, ok := form.(cliui.HuhForm)
			if !ok {
				t.Fatalf("form %d did not use shared Huh boundary", formCalls)
			}
			for _, group := range spec.Groups {
				for _, field := range group.Fields {
					if field.StringValue != nil && field.Kind == cliui.FieldSelect {
						*field.StringValue = field.Options[0].Value
					}
					if field.BoolValue != nil {
						*field.BoolValue = true
					}
				}
			}
			return nil
		},
	}
	if err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), true, dependencies); err != nil {
		t.Fatal(err)
	}
	if formCalls != 6 {
		t.Fatalf("advanced installer form calls = %d, want workspace, standard bundles, retention, advanced, capacity, final", formCalls)
	}
	plan, _, err := install.ReadPlan(filepath.Join(home, "secondbox-install_0123456789abcdef", "install-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.APIAddress != "127.0.0.1:8080" || plan.RetentionSeconds <= 0 {
		t.Fatalf("advanced reviewed plan = %#v", plan.Network)
	}
}

func TestGuidedInstallRejectedCapacityCreatesNothing(t *testing.T) {
	usePublishedGuidedReleaseBuild(t)
	created := false
	capabilities := cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{})
	capabilities.Accessible = true
	renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	dependencies := systemGuidedInstallDependencies()
	dependencies.Input = strings.NewReader("1\ny\n1\nn\n")
	dependencies.HomeDirectory = func() (string, error) { return t.TempDir(), nil }
	dependencies.AvailableBytes = func(string) (int64, error) { return 100 << 30, nil }
	dependencies.VerifyRelease = func(context.Context, string) (releaseverify.VerifiedRelease, error) { return fakeGuidedRelease(), nil }
	dependencies.OperationID = func() (string, error) { return "install_0123456789abcdef", nil }
	dependencies.MakeDirectory = func(string) error { created = true; return nil }
	if err := runGuidedInstallWith(context.Background(), renderer, guidedFacts(), false, dependencies); err == nil {
		t.Fatal("rejected capacity succeeded")
	}
	if created {
		t.Fatal("rejected capacity created an operation directory")
	}
}

func TestPrivateHostApplyGrammarIsStrictAndAbsentFromHelp(t *testing.T) {
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: &output, Capabilities: cliui.ForWriter(&output, &output), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	helpErr := usage(renderer)
	if strings.Contains(helpErr.Error(), "_install-host-apply") || strings.Contains(helpErr.Error(), "_install-host-purge") || strings.Contains(helpErr.Error(), "_install-host-purge-validate") {
		t.Fatal("private installer command leaked into ordinary help")
	}
	for _, arguments := range [][]string{nil, {"one"}, {"one", "two", "three"}} {
		err := runPrivateHostApply(context.Background(), arguments)
		if err == nil || !strings.HasPrefix(err.Error(), "SecondBox installer private host apply:") {
			t.Fatalf("arguments %#v error = %v", arguments, err)
		}
	}
	t.Setenv("SUDO_UID", "not-a-number")
	err := runPrivateHostApply(context.Background(), []string{"/operation", "sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "SUDO_UID") {
		t.Fatalf("invalid SUDO_UID error = %v", err)
	}
	for _, arguments := range [][]string{nil, {"one"}, {"one", "two", "three"}} {
		err := runPrivateHostPurge(context.Background(), arguments)
		if err == nil || !strings.HasPrefix(err.Error(), "SecondBox installer private host purge:") {
			t.Fatalf("purge arguments %#v error = %v", arguments, err)
		}
	}
	for _, arguments := range [][]string{nil, {"one"}, {"one", "two", "three"}} {
		err := runPrivateHostPurgeValidate(arguments)
		if err == nil || !strings.HasPrefix(err.Error(), "SecondBox installer private host purge validation:") {
			t.Fatalf("purge validation arguments %#v error = %v", arguments, err)
		}
	}
}
