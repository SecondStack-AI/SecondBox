package firecracker

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testSnapshotTemplateKey() SnapshotTemplateKey {
	return SnapshotTemplateKey{
		ArtifactVersion:         "2026.08.01",
		Architecture:            "amd64",
		SigningKeyFingerprint:   strings.Repeat("a", 64),
		SignedManifestDigest:    "sha256:" + strings.Repeat("b", 64),
		KernelSHA256:            strings.Repeat("c", 64),
		KernelArgs:              "console=ttyS0 root=/dev/vda rw noxsave",
		SourceRootfsSHA256:      strings.Repeat("d", 64),
		SharedImageSHA256:       strings.Repeat("e", 64),
		RuntimeBundleDigest:     "sha256:" + strings.Repeat("f", 64),
		ToolchainBundleDigest:   "sha256:" + strings.Repeat("0", 64),
		GuestBuildID:            "guest-build-1",
		GuestProtocolGeneration: 1,
		GuestFeatures:           []string{"streaming_exec", "pty_resize"},
		FirecrackerVersion:      "1.16.1",
		HostCPUFingerprint:      strings.Repeat("1", 64),
		CPUTemplate:             "",
		VCPUCount:               1,
		MemorySizeMiB:           512,
		WorkspaceSizeMiB:        64,
		ProcessLimit:            256,
		RuntimeClass:            "tool_executor",
		NetworkInterfaceID:      "eth0",
		TemplateGuestMAC:        "02:00:00:5b:7e:00",
		GuestControlVsockPort:   1024,
		GuestProtocolVsockPort:  1025,
		GuestCID:                3,
	}
}

func writeTemplateFile(t *testing.T, path string, content string) SnapshotTemplateFile {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	digest := sha256.Sum256([]byte(content))
	return SnapshotTemplateFile{
		Name:   filepath.Base(path),
		SHA256: hex.EncodeToString(digest[:]),
		Bytes:  int64(len(content)),
	}
}

func stageTestSnapshotTemplate(t *testing.T, cache *SnapshotTemplateCache, key SnapshotTemplateKey) SnapshotTemplateManifest {
	t.Helper()
	stageDir, err := cache.StageDirectory()
	if err != nil {
		t.Fatalf("stage directory: %v", err)
	}
	templateID, err := key.TemplateID()
	if err != nil {
		t.Fatalf("template id: %v", err)
	}
	manifest := SnapshotTemplateManifest{
		SchemaVersion: snapshotTemplateManifestSchemaVersion,
		TemplateID:    templateID,
		CreatedAt:     "2026-08-06T00:00:00Z",
		Key:           key,
		VMState:       writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateVMStateName), "vm-state-bytes"),
		Memory:        writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateMemoryName), "memory-bytes"),
		Rootfs:        writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateRootfsName), "rootfs-bytes"),
	}
	if _, err := cache.Publish(stageDir, manifest); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return manifest
}

func newTestSnapshotTemplateCache(t *testing.T) *SnapshotTemplateCache {
	t.Helper()
	root := filepath.Join(t.TempDir(), "templates")
	cache, err := NewSnapshotTemplateCache(root)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	return cache
}

func TestSnapshotTemplateKeyIdentityIsDeterministicAndPathIndependent(t *testing.T) {
	key := testSnapshotTemplateKey()
	first, err := key.TemplateID()
	if err != nil {
		t.Fatalf("first identity: %v", err)
	}
	reordered := testSnapshotTemplateKey()
	reordered.GuestFeatures = []string{"pty_resize", "streaming_exec"}
	second, err := reordered.TemplateID()
	if err != nil {
		t.Fatalf("second identity: %v", err)
	}
	if first != second {
		t.Fatalf("feature ordering changed the template identity: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("template identity %q is not a SHA-256 hex digest", first)
	}
}

func TestSnapshotTemplateKeyChangeProducesNewIdentity(t *testing.T) {
	base, err := testSnapshotTemplateKey().TemplateID()
	if err != nil {
		t.Fatalf("base identity: %v", err)
	}
	mutations := map[string]func(*SnapshotTemplateKey){
		"kernel args":     func(k *SnapshotTemplateKey) { k.KernelArgs += " extra=1" },
		"rootfs digest":   func(k *SnapshotTemplateKey) { k.SourceRootfsSHA256 = strings.Repeat("9", 64) },
		"firecracker":     func(k *SnapshotTemplateKey) { k.FirecrackerVersion = "1.17.0" },
		"host cpu":        func(k *SnapshotTemplateKey) { k.HostCPUFingerprint = strings.Repeat("2", 64) },
		"memory shape":    func(k *SnapshotTemplateKey) { k.MemorySizeMiB = 1024 },
		"vcpu shape":      func(k *SnapshotTemplateKey) { k.VCPUCount = 2 },
		"protocol":        func(k *SnapshotTemplateKey) { k.GuestProtocolGeneration = 2 },
		"features":        func(k *SnapshotTemplateKey) { k.GuestFeatures = []string{"streaming_exec"} },
		"control port":    func(k *SnapshotTemplateKey) { k.GuestControlVsockPort = 2048 },
		"signing key":     func(k *SnapshotTemplateKey) { k.SigningKeyFingerprint = strings.Repeat("3", 64) },
		"signed manifest": func(k *SnapshotTemplateKey) { k.SignedManifestDigest = "sha256:" + strings.Repeat("4", 64) },
		"workspace shape": func(k *SnapshotTemplateKey) { k.WorkspaceSizeMiB = 128 },
		"process limit":   func(k *SnapshotTemplateKey) { k.ProcessLimit = 512 },
		"runtime class":   func(k *SnapshotTemplateKey) { k.RuntimeClass = "other" },
		"network shape":   func(k *SnapshotTemplateKey) { k.NetworkInterfaceID = "eth1" },
		"template mac":    func(k *SnapshotTemplateKey) { k.TemplateGuestMAC = "02:00:00:5b:7e:01" },
		"no network device": func(k *SnapshotTemplateKey) {
			k.NetworkInterfaceID = ""
			k.TemplateGuestMAC = ""
		},
		"guest cid":        func(k *SnapshotTemplateKey) { k.GuestCID = 4 },
		"cpu template":     func(k *SnapshotTemplateKey) { k.CPUTemplate = "T2" },
		"toolchain bundle": func(k *SnapshotTemplateKey) { k.ToolchainBundleDigest = "sha256:" + strings.Repeat("5", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			key := testSnapshotTemplateKey()
			mutate(&key)
			mutated, err := key.TemplateID()
			if err != nil {
				t.Fatalf("mutated identity: %v", err)
			}
			if mutated == base {
				t.Fatalf("%s did not change the template identity", name)
			}
		})
	}
}

func TestSnapshotTemplateKeyRejectsIncompleteIdentity(t *testing.T) {
	mutations := map[string]func(*SnapshotTemplateKey){
		"blank artifact version": func(k *SnapshotTemplateKey) { k.ArtifactVersion = " " },
		"blank kernel args":      func(k *SnapshotTemplateKey) { k.KernelArgs = "" },
		"zero vcpu":              func(k *SnapshotTemplateKey) { k.VCPUCount = 0 },
		"zero memory":            func(k *SnapshotTemplateKey) { k.MemorySizeMiB = 0 },
		"zero generation":        func(k *SnapshotTemplateKey) { k.GuestProtocolGeneration = 0 },
		"no features":            func(k *SnapshotTemplateKey) { k.GuestFeatures = nil },
		"colliding vsock ports":  func(k *SnapshotTemplateKey) { k.GuestProtocolVsockPort = k.GuestControlVsockPort },
		"zero cid":               func(k *SnapshotTemplateKey) { k.GuestCID = 0 },
		// A network device is described by both fields or by neither. Naming an
		// interface without the MAC it was captured with, or the reverse, leaves
		// the shape a resumed Instance must replace unstated.
		"interface without template mac": func(k *SnapshotTemplateKey) { k.TemplateGuestMAC = "" },
		"template mac without interface": func(k *SnapshotTemplateKey) { k.NetworkInterfaceID = "" },
		"malformed template mac":         func(k *SnapshotTemplateKey) { k.TemplateGuestMAC = "not-a-mac" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			key := testSnapshotTemplateKey()
			mutate(&key)
			if _, err := key.TemplateID(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestSnapshotTemplateCachePublishAndResolve(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	manifest := stageTestSnapshotTemplate(t, cache, key)

	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if template.TemplateID != manifest.TemplateID {
		t.Fatalf("resolved identity %q, want %q", template.TemplateID, manifest.TemplateID)
	}
	if filepath.Dir(template.VMStatePath) != template.Directory {
		t.Fatalf("template files are outside the template directory: %+v", template)
	}
	if template.MemoryBytes != manifest.Memory.Bytes {
		t.Fatalf("memory bytes = %d, want %d", template.MemoryBytes, manifest.Memory.Bytes)
	}
	entries, err := os.ReadDir(cache.Root())
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != manifest.TemplateID {
		t.Fatalf("cache root holds %v, want only the published template", entries)
	}
}

func TestSnapshotTemplateCacheRejectsUnknownKey(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	stageTestSnapshotTemplate(t, cache, testSnapshotTemplateKey())
	other := testSnapshotTemplateKey()
	other.MemorySizeMiB = 2048
	if _, err := cache.Resolve(other); err == nil {
		t.Fatal("an incompatible key resolved to a template")
	}
}

func TestSnapshotTemplateCacheRejectsCorruptedAndTruncatedFiles(t *testing.T) {
	tests := map[string]func(t *testing.T, dir string){
		"corrupted memory": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, snapshotTemplateMemoryName), []byte("memory-bytez"), 0o600); err != nil {
				t.Fatalf("corrupt memory: %v", err)
			}
		},
		"truncated vm state": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, snapshotTemplateVMStateName), []byte("vm"), 0o600); err != nil {
				t.Fatalf("truncate vm state: %v", err)
			}
		},
		"missing rootfs": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, snapshotTemplateRootfsName)); err != nil {
				t.Fatalf("remove rootfs: %v", err)
			}
		},
		"rootfs replaced by symlink": func(t *testing.T, dir string) {
			path := filepath.Join(dir, snapshotTemplateRootfsName)
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove rootfs: %v", err)
			}
			if err := os.Symlink("/etc/hostname", path); err != nil {
				t.Fatalf("symlink rootfs: %v", err)
			}
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			cache := newTestSnapshotTemplateCache(t)
			key := testSnapshotTemplateKey()
			manifest := stageTestSnapshotTemplate(t, cache, key)
			dir := filepath.Join(cache.Root(), manifest.TemplateID)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatalf("chmod template dir: %v", err)
			}
			for _, name := range []string{snapshotTemplateVMStateName, snapshotTemplateMemoryName, snapshotTemplateRootfsName} {
				if err := os.Chmod(filepath.Join(dir, name), 0o600); err != nil {
					t.Fatalf("chmod template file: %v", err)
				}
			}
			corrupt(t, dir)
			if _, err := cache.Resolve(key); err == nil {
				t.Fatal("a damaged template was admitted")
			}
		})
	}
}

func TestSnapshotTemplateCacheRejectsRelabelledTemplate(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	manifest := stageTestSnapshotTemplate(t, cache, key)
	dir := filepath.Join(cache.Root(), manifest.TemplateID)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod template dir: %v", err)
	}
	manifestPath := filepath.Join(dir, snapshotTemplateManifestName)
	if err := os.Chmod(manifestPath, 0o600); err != nil {
		t.Fatalf("chmod manifest: %v", err)
	}
	tampered := manifest
	tampered.Key.MemorySizeMiB = 2048
	encoded, err := json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatalf("encode tampered manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}
	if _, err := cache.Resolve(key); err == nil {
		t.Fatal("a manifest whose identity disagrees with its key was admitted")
	}
}

func TestSnapshotTemplateStableIdentityFailsAfterReplacement(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	manifest := stageTestSnapshotTemplate(t, cache, key)
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := template.VerifyStableIdentity(); err != nil {
		t.Fatalf("stable identity immediately after admission: %v", err)
	}
	dir := filepath.Join(cache.Root(), manifest.TemplateID)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod template dir: %v", err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("memory-bytes"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, filepath.Join(dir, snapshotTemplateMemoryName)); err != nil {
		t.Fatalf("replace memory file: %v", err)
	}
	if err := template.VerifyStableIdentity(); err == nil {
		t.Fatal("a replaced memory file passed the per-start identity check")
	}
	if _, err := cache.Resolve(key); err == nil {
		t.Fatal("resolve returned a template whose files were replaced")
	}
}

// TestSnapshotTemplateStableIdentitySurvivesPerInstanceLinking pins the reason
// a template's identity omits ctime. Every resumed Instance hard-links the
// golden memory file into its own jail so all Instances share one inode and one
// page cache, and teardown unlinks it again. Both operations mark the inode's
// status-change time, so an identity that pinned ctime would refuse the second
// concurrent resume of a file nothing had modified.
func TestSnapshotTemplateStableIdentitySurvivesPerInstanceLinking(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	stageTestSnapshotTemplate(t, cache, key)
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	jailRoot := t.TempDir()
	var staged []string
	for index := range 3 {
		destination := filepath.Join(jailRoot, fmt.Sprintf("memory-%d.snap", index))
		if err := stageSharedTemplateFile(destination, template.MemoryPath); err != nil {
			t.Fatalf("stage golden memory file %d: %v", index, err)
		}
		staged = append(staged, destination)
		if err := template.VerifyStableIdentity(); err != nil {
			t.Fatalf("stable identity after staging %d jail links: %v", index+1, err)
		}
		shared, err := sharesInode(destination, template.MemoryPath)
		if err != nil {
			t.Fatalf("compare staged inode %d: %v", index, err)
		}
		if !shared {
			t.Fatalf("staged golden memory file %d is a different inode", index)
		}
	}
	for index, destination := range staged {
		if err := os.Remove(destination); err != nil {
			t.Fatalf("release jail link %d: %v", index, err)
		}
		if err := template.VerifyStableIdentity(); err != nil {
			t.Fatalf("stable identity after releasing %d jail links: %v", index+1, err)
		}
	}
}

// TestSnapshotTemplateStableIdentityFailsAfterRewrite proves omitting ctime did
// not open the file to in-place modification: rewriting the same byte count
// still moves the modification time, which the identity does pin.
func TestSnapshotTemplateStableIdentityFailsAfterRewrite(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	manifest := stageTestSnapshotTemplate(t, cache, key)
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	dir := filepath.Join(cache.Root(), manifest.TemplateID)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod template dir: %v", err)
	}
	memoryPath := filepath.Join(dir, snapshotTemplateMemoryName)
	if err := os.Chmod(memoryPath, 0o600); err != nil {
		t.Fatalf("chmod template memory file: %v", err)
	}
	original, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatalf("read template memory file: %v", err)
	}
	rewritten := append([]byte(nil), original...)
	rewritten[0] ^= 0xff
	file, err := os.OpenFile(memoryPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open template memory file for rewrite: %v", err)
	}
	if _, err := file.WriteAt(rewritten, 0); err != nil {
		_ = file.Close()
		t.Fatalf("rewrite template memory file in place: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rewritten template memory file: %v", err)
	}
	if err := template.VerifyStableIdentity(); err == nil {
		t.Fatal("a template memory file rewritten in place passed the per-start identity check")
	}
}

func TestSnapshotTemplateCachePublishIsAtomicUnderConcurrency(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	const publishers = 4
	var wait sync.WaitGroup
	errs := make(chan error, publishers)
	wait.Add(publishers)
	for range publishers {
		go func() {
			defer wait.Done()
			stageDir, err := cache.StageDirectory()
			if err != nil {
				errs <- err
				return
			}
			templateID, err := key.TemplateID()
			if err != nil {
				errs <- err
				return
			}
			manifest := SnapshotTemplateManifest{
				SchemaVersion: snapshotTemplateManifestSchemaVersion,
				TemplateID:    templateID,
				CreatedAt:     "2026-08-06T00:00:00Z",
				Key:           key,
			}
			manifest.VMState = writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateVMStateName), "vm-state-bytes")
			manifest.Memory = writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateMemoryName), "memory-bytes")
			manifest.Rootfs = writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateRootfsName), "rootfs-bytes")
			if _, err := cache.Publish(stageDir, manifest); err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent publish: %v", err)
	}
	entries, err := os.ReadDir(cache.Root())
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache root holds %d entries after concurrent publish, want 1", len(entries))
	}
	if _, err := cache.Resolve(key); err != nil {
		t.Fatalf("resolve after concurrent publish: %v", err)
	}
}

func TestSnapshotTemplateCacheResolveCoalescesConcurrentAdmission(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	stageTestSnapshotTemplate(t, cache, key)
	const resolvers = 8
	var wait sync.WaitGroup
	results := make(chan *AdmittedSnapshotTemplate, resolvers)
	errs := make(chan error, resolvers)
	wait.Add(resolvers)
	for range resolvers {
		go func() {
			defer wait.Done()
			template, err := cache.Resolve(key)
			if err != nil {
				errs <- err
				return
			}
			results <- template
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resolve: %v", err)
	}
	var first *AdmittedSnapshotTemplate
	count := 0
	for template := range results {
		count++
		if first == nil {
			first = template
			continue
		}
		if template != first {
			t.Fatal("concurrent resolves returned different admitted templates")
		}
	}
	if count != resolvers {
		t.Fatalf("resolved %d times, want %d", count, resolvers)
	}
}

func TestSnapshotTemplateCacheRejectsPartiallyPublishedDirectory(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	key := testSnapshotTemplateKey()
	templateID, err := key.TemplateID()
	if err != nil {
		t.Fatalf("template id: %v", err)
	}
	partial := filepath.Join(cache.Root(), templateID)
	if err := os.Mkdir(partial, 0o700); err != nil {
		t.Fatalf("create partial template: %v", err)
	}
	writeTemplateFile(t, filepath.Join(partial, snapshotTemplateVMStateName), "vm-state-bytes")
	if _, err := cache.Resolve(key); err == nil {
		t.Fatal("a template directory with no manifest was admitted")
	}
}

func TestSnapshotTemplateCacheRootMustBeCleanAbsolutePath(t *testing.T) {
	for _, root := range []string{"", "relative/path", "/tmp/../tmp/x"} {
		if _, err := NewSnapshotTemplateCache(root); err == nil {
			t.Fatalf("cache root %q was accepted", root)
		}
	}
}

func TestSnapshotTemplateCachePublishRejectsForeignStagingDirectory(t *testing.T) {
	cache := newTestSnapshotTemplateCache(t)
	foreign := t.TempDir()
	key := testSnapshotTemplateKey()
	templateID, err := key.TemplateID()
	if err != nil {
		t.Fatalf("template id: %v", err)
	}
	manifest := SnapshotTemplateManifest{
		SchemaVersion: snapshotTemplateManifestSchemaVersion,
		TemplateID:    templateID,
		CreatedAt:     "2026-08-06T00:00:00Z",
		Key:           key,
		VMState:       writeTemplateFile(t, filepath.Join(foreign, snapshotTemplateVMStateName), "vm-state-bytes"),
		Memory:        writeTemplateFile(t, filepath.Join(foreign, snapshotTemplateMemoryName), "memory-bytes"),
		Rootfs:        writeTemplateFile(t, filepath.Join(foreign, snapshotTemplateRootfsName), "rootfs-bytes"),
	}
	if _, err := cache.Publish(foreign, manifest); err == nil {
		t.Fatal("a staging directory outside the cache root was published")
	}
}

func TestHostCPUCompatibilityFingerprintFrom(t *testing.T) {
	cpuinfo := "processor\t: 0\n" +
		"vendor_id\t: GenuineIntel\n" +
		"cpu family\t: 6\n" +
		"model\t\t: 158\n" +
		"model name\t: Test CPU\n" +
		"stepping\t: 10\n" +
		"flags\t\t: fpu vme de\n" +
		"\n" +
		"processor\t: 1\n" +
		"vendor_id\t: GenuineIntel\n" +
		"cpu family\t: 6\n" +
		"model\t\t: 158\n" +
		"stepping\t: 10\n" +
		"flags\t\t: fpu vme de\n"
	first, err := hostCPUCompatibilityFingerprintFrom(bufio.NewScanner(strings.NewReader(cpuinfo)))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	second, err := hostCPUCompatibilityFingerprintFrom(bufio.NewScanner(strings.NewReader(cpuinfo)))
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint is not deterministic: %q vs %q", first, second)
	}
	changed, err := hostCPUCompatibilityFingerprintFrom(
		bufio.NewScanner(strings.NewReader(strings.Replace(cpuinfo, "stepping\t: 10", "stepping\t: 11", 1))),
	)
	if err != nil {
		t.Fatalf("changed fingerprint: %v", err)
	}
	if changed == first {
		t.Fatal("a different stepping produced the same fingerprint")
	}
	if _, err := hostCPUCompatibilityFingerprintFrom(
		bufio.NewScanner(strings.NewReader("processor\t: 0\nvendor_id\t: GenuineIntel\n")),
	); err == nil {
		t.Fatal("an incomplete cpuinfo produced a fingerprint")
	}
}

func TestHostCPUCompatibilityFingerprintReadsHost(t *testing.T) {
	fingerprint, err := hostCPUCompatibilityFingerprint()
	if err != nil {
		t.Fatalf("host fingerprint: %v", err)
	}
	if len(fingerprint) != 64 {
		t.Fatalf("host fingerprint %q is not a SHA-256 hex digest", fingerprint)
	}
}

// TestSnapshotTemplateBundleLookupGatesRunnerResumeCapacity pins the condition a
// runner advertises snapshot-resume capacity on. An empty cache, or a cache
// holding only templates built from a different signed bundle, must report no
// capacity: resume has no cold-boot fallback, so advertising it without a usable
// template would attract Profiles that could never start.
func TestSnapshotTemplateBundleLookupGatesRunnerResumeCapacity(t *testing.T) {
	cache, err := NewSnapshotTemplateCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := testSnapshotTemplateKey()
	identity := SnapshotTemplateBundleIdentity{
		ArtifactVersion:       key.ArtifactVersion,
		Architecture:          key.Architecture,
		RuntimeBundleDigest:   key.RuntimeBundleDigest,
		ToolchainBundleDigest: key.ToolchainBundleDigest,
	}

	ready, err := cache.HasTemplateForBundle(identity)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("an empty cache reported snapshot-resume capacity")
	}

	// A template from a different toolchain bundle is not this runner's template.
	otherKey := key
	otherKey.ToolchainBundleDigest = "sha256:" + strings.Repeat("9", 64)
	stageTestSnapshotTemplate(t, cache, otherKey)
	ready, err = cache.HasTemplateForBundle(identity)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("a template from another signed bundle reported snapshot-resume capacity")
	}

	stageTestSnapshotTemplate(t, cache, key)
	ready, err = cache.HasTemplateForBundle(identity)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("a template built from this bundle did not report snapshot-resume capacity")
	}

	// An incomplete bundle identity can never be proved, so it is refused rather
	// than silently matching whatever the cache happens to hold.
	if _, err := cache.HasTemplateForBundle(SnapshotTemplateBundleIdentity{
		ArtifactVersion: key.ArtifactVersion,
	}); err == nil {
		t.Fatal("an incomplete bundle identity was accepted")
	}
}

// TestSnapshotTemplateBundleLookupIgnoresStagingAndUnreadableDirectories proves
// the lookup answers from published templates only. A staging directory is a
// build in progress and a directory without a valid manifest is not a template.
func TestSnapshotTemplateBundleLookupIgnoresStagingAndUnreadableDirectories(t *testing.T) {
	root := t.TempDir()
	cache, err := NewSnapshotTemplateCache(root)
	if err != nil {
		t.Fatal(err)
	}
	key := testSnapshotTemplateKey()
	identity := SnapshotTemplateBundleIdentity{
		ArtifactVersion:       key.ArtifactVersion,
		Architecture:          key.Architecture,
		RuntimeBundleDigest:   key.RuntimeBundleDigest,
		ToolchainBundleDigest: key.ToolchainBundleDigest,
	}
	stageDir, err := cache.StageDirectory()
	if err != nil {
		t.Fatal(err)
	}
	writeTemplateFile(t, filepath.Join(stageDir, snapshotTemplateVMStateName), "vm state")
	if err := os.MkdirAll(filepath.Join(root, "not-a-template"), 0o700); err != nil {
		t.Fatal(err)
	}
	ready, err := cache.HasTemplateForBundle(identity)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("staging and non-template directories reported snapshot-resume capacity")
	}
}
