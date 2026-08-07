package firecracker

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// A resume template is three immutable runner-local files plus a manifest. The
// names are part of the compatibility contract: a jailed Instance restores
// drives by the chroot-relative name recorded in the VM state, so the sealed
// post-boot rootfs must be staged under exactly the name the template booted
// with.
const (
	snapshotTemplateManifestName = "manifest.json"
	snapshotTemplateVMStateName  = "vmstate.snap"
	snapshotTemplateMemoryName   = "memory.snap"
	snapshotTemplateRootfsName   = rootfsName

	snapshotTemplateManifestSchemaVersion = 1
	snapshotTemplateStagingPrefix         = ".staging-"
)

// SnapshotTemplateKey is the complete compatibility identity of a resume
// template. Every field that can change what a restored guest is, or what the
// VMM will accept, belongs here. A mismatch in any field is a cache miss, never
// a best-effort restore.
type SnapshotTemplateKey struct {
	ArtifactVersion         string   `json:"artifactVersion"`
	Architecture            string   `json:"architecture"`
	SigningKeyFingerprint   string   `json:"signingKeyFingerprint"`
	SignedManifestDigest    string   `json:"signedManifestDigest"`
	KernelSHA256            string   `json:"kernelSha256"`
	KernelArgs              string   `json:"kernelArgs"`
	SourceRootfsSHA256      string   `json:"sourceRootfsSha256"`
	SharedImageSHA256       string   `json:"sharedImageSha256"`
	RuntimeBundleDigest     string   `json:"runtimeBundleDigest"`
	ToolchainBundleDigest   string   `json:"toolchainBundleDigest"`
	GuestBuildID            string   `json:"guestBuildId"`
	GuestProtocolGeneration uint32   `json:"guestProtocolGeneration"`
	GuestFeatures           []string `json:"guestFeatures"`
	FirecrackerVersion      string   `json:"firecrackerVersion"`
	HostCPUFingerprint      string   `json:"hostCpuFingerprint"`
	CPUTemplate             string   `json:"cpuTemplate"`
	VCPUCount               int      `json:"vcpuCount"`
	MemorySizeMiB           int      `json:"memorySizeMiB"`
	WorkspaceSizeMiB        int      `json:"workspaceSizeMiB"`
	ProcessLimit            int      `json:"processLimit"`
	RuntimeClass            string   `json:"runtimeClass"`
	NetworkInterfaceID      string   `json:"networkInterfaceId"`
	GuestControlVsockPort   uint32   `json:"guestControlVsockPort"`
	GuestProtocolVsockPort  uint32   `json:"guestProtocolVsockPort"`
	GuestCID                uint32   `json:"guestCid"`
}

// Validate rejects a key that cannot identify a template. Every field is
// required because a blank field would silently collapse two incompatible
// templates onto one identity.
func (k SnapshotTemplateKey) Validate() error {
	required := []struct {
		name  string
		value string
	}{
		{"artifactVersion", k.ArtifactVersion},
		{"architecture", k.Architecture},
		{"signingKeyFingerprint", k.SigningKeyFingerprint},
		{"signedManifestDigest", k.SignedManifestDigest},
		{"kernelSha256", k.KernelSHA256},
		{"kernelArgs", k.KernelArgs},
		{"sourceRootfsSha256", k.SourceRootfsSHA256},
		{"sharedImageSha256", k.SharedImageSHA256},
		{"runtimeBundleDigest", k.RuntimeBundleDigest},
		{"toolchainBundleDigest", k.ToolchainBundleDigest},
		{"guestBuildId", k.GuestBuildID},
		{"firecrackerVersion", k.FirecrackerVersion},
		{"hostCpuFingerprint", k.HostCPUFingerprint},
		{"runtimeClass", k.RuntimeClass},
		{"networkInterfaceId", k.NetworkInterfaceID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("SecondBox snapshot template key field %q is required", field.name)
		}
	}
	positive := []struct {
		name  string
		value int
	}{
		{"vcpuCount", k.VCPUCount},
		{"memorySizeMiB", k.MemorySizeMiB},
		{"workspaceSizeMiB", k.WorkspaceSizeMiB},
		{"processLimit", k.ProcessLimit},
	}
	for _, field := range positive {
		if field.value < 1 {
			return fmt.Errorf("SecondBox snapshot template key field %q must be positive", field.name)
		}
	}
	if k.GuestProtocolGeneration == 0 {
		return fmt.Errorf("SecondBox snapshot template key field \"guestProtocolGeneration\" is required")
	}
	if k.GuestControlVsockPort == 0 || k.GuestProtocolVsockPort == 0 {
		return fmt.Errorf("SecondBox snapshot template key guest vsock ports are required")
	}
	if k.GuestControlVsockPort == k.GuestProtocolVsockPort {
		return fmt.Errorf("SecondBox snapshot template key guest control and protocol vsock ports must differ")
	}
	if k.GuestCID == 0 {
		return fmt.Errorf("SecondBox snapshot template key field \"guestCid\" is required")
	}
	if len(k.GuestFeatures) == 0 {
		return fmt.Errorf("SecondBox snapshot template key field \"guestFeatures\" is required")
	}
	return nil
}

// TemplateID is the stable identity of the compatibility key. It is derived
// only from the key's own values, never from output paths or runner-local
// names, so two runners building the same template agree on its identity.
func (k SnapshotTemplateKey) TemplateID() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	normalized := k
	features := append([]string(nil), k.GuestFeatures...)
	sort.Strings(features)
	normalized.GuestFeatures = features
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode snapshot template key: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

type SnapshotTemplateFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// SnapshotTemplateManifest is written beside the template files and is the only
// description of them a runner trusts. It carries no host path: a template is
// relocatable within a runner's cache root and its identity does not depend on
// where it landed.
type SnapshotTemplateManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	TemplateID    string               `json:"templateId"`
	CreatedAt     string               `json:"createdAt"`
	Key           SnapshotTemplateKey  `json:"key"`
	VMState       SnapshotTemplateFile `json:"vmState"`
	Memory        SnapshotTemplateFile `json:"memory"`
	Rootfs        SnapshotTemplateFile `json:"rootfs"`
}

func (m SnapshotTemplateManifest) validate() error {
	if m.SchemaVersion != snapshotTemplateManifestSchemaVersion {
		return fmt.Errorf(
			"SecondBox snapshot template manifest schema version %d is unsupported, want %d",
			m.SchemaVersion,
			snapshotTemplateManifestSchemaVersion,
		)
	}
	derived, err := m.Key.TemplateID()
	if err != nil {
		return err
	}
	if derived != m.TemplateID {
		return fmt.Errorf("SecondBox snapshot template manifest identity does not match its compatibility key")
	}
	files := []struct {
		label string
		want  string
		file  SnapshotTemplateFile
	}{
		{"VM state", snapshotTemplateVMStateName, m.VMState},
		{"memory", snapshotTemplateMemoryName, m.Memory},
		{"rootfs", snapshotTemplateRootfsName, m.Rootfs},
	}
	for _, entry := range files {
		if entry.file.Name != entry.want {
			return fmt.Errorf(
				"SecondBox snapshot template %s file is named %q, want %q",
				entry.label,
				entry.file.Name,
				entry.want,
			)
		}
		if len(entry.file.SHA256) != 64 {
			return fmt.Errorf("SecondBox snapshot template %s file digest is not a SHA-256 hex digest", entry.label)
		}
		if entry.file.Bytes < 1 {
			return fmt.Errorf("SecondBox snapshot template %s file records no bytes", entry.label)
		}
	}
	return nil
}

// AdmittedSnapshotTemplate is a template whose digests have been verified once,
// at cache admission. Every later start proves only that the exact files are
// still the ones that were verified, exactly as launch artifacts are protected
// today; it never rehashes multi-gigabyte files on the start path.
type AdmittedSnapshotTemplate struct {
	TemplateID   string
	Directory    string
	VMStatePath  string
	MemoryPath   string
	RootfsPath   string
	MemoryBytes  int64
	Manifest     SnapshotTemplateManifest
	pinnedFiles  []trustedMicroVMArtifactFile
	admittedOnce bool
}

// VerifyStableIdentity proves the admitted files have not been replaced,
// truncated, or rewritten in place since admission. It is the per-start check.
func (t *AdmittedSnapshotTemplate) VerifyStableIdentity() error {
	if t == nil || !t.admittedOnce {
		return fmt.Errorf("SecondBox snapshot template is not admitted")
	}
	unchanged, err := trustedMicroVMArtifactsUnchanged(&trustedMicroVMArtifacts{files: t.pinnedFiles})
	if err != nil {
		return err
	}
	if !unchanged {
		return fmt.Errorf("SecondBox snapshot template %q changed after cache admission", t.TemplateID)
	}
	return nil
}

// SnapshotTemplateCache is the runner-owned immutable execution-asset cache for
// resume templates. PostgreSQL never learns a path in it.
type SnapshotTemplateCache struct {
	root string

	mu        sync.Mutex
	admitting map[string]*sync.Mutex
	admitted  map[string]*AdmittedSnapshotTemplate
}

func NewSnapshotTemplateCache(root string) (*SnapshotTemplateCache, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("SecondBox snapshot template cache root is required")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("SecondBox snapshot template cache root %q must be a clean absolute path", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create SecondBox snapshot template cache root: %w", err)
	}
	return &SnapshotTemplateCache{
		root:      root,
		admitting: map[string]*sync.Mutex{},
		admitted:  map[string]*AdmittedSnapshotTemplate{},
	}, nil
}

func (c *SnapshotTemplateCache) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// StageDirectory returns a private directory a template build writes into. It
// is never visible under a template identity until Publish renames it.
func (c *SnapshotTemplateCache) StageDirectory() (string, error) {
	if c == nil {
		return "", fmt.Errorf("SecondBox snapshot template cache is required")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate snapshot template staging name: %w", err)
	}
	dir := filepath.Join(c.root, snapshotTemplateStagingPrefix+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot template staging directory: %w", err)
	}
	return dir, nil
}

// Publish seals a staged template and makes it visible under its compatibility
// identity in one rename. A reader therefore never opens a partial generation.
// Publishing a template that already exists is not an error: templates are
// immutable and identity-addressed, so the existing generation is retained.
func (c *SnapshotTemplateCache) Publish(stageDir string, manifest SnapshotTemplateManifest) (string, error) {
	if c == nil {
		return "", fmt.Errorf("SecondBox snapshot template cache is required")
	}
	if err := manifest.validate(); err != nil {
		return "", err
	}
	if filepath.Dir(stageDir) != c.root {
		return "", fmt.Errorf("SecondBox snapshot template staging directory %q is outside the cache root", stageDir)
	}
	for _, file := range []SnapshotTemplateFile{manifest.VMState, manifest.Memory, manifest.Rootfs} {
		if err := verifySnapshotTemplateFile(filepath.Join(stageDir, file.Name), file); err != nil {
			return "", err
		}
	}
	manifestPath := filepath.Join(stageDir, snapshotTemplateManifestName)
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode snapshot template manifest: %w", err)
	}
	if err := writeSyncedFile(manifestPath, append(encoded, '\n'), 0o400); err != nil {
		return "", err
	}
	for _, name := range []string{snapshotTemplateVMStateName, snapshotTemplateMemoryName, snapshotTemplateRootfsName} {
		if err := syncSnapshotTemplateFile(filepath.Join(stageDir, name)); err != nil {
			return "", err
		}
	}
	if err := syncDirectory(stageDir); err != nil {
		return "", fmt.Errorf("sync snapshot template staging directory: %w", err)
	}
	destination := filepath.Join(c.root, manifest.TemplateID)
	if err := os.Rename(stageDir, destination); err != nil {
		// Templates are immutable and identity-addressed, so losing the rename
		// to a concurrent publisher of the same identity is the expected
		// outcome, not a failure. Linux reports it as ENOTEMPTY or EEXIST.
		if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish snapshot template %q: %w", manifest.TemplateID, err)
		}
		if removeErr := os.RemoveAll(stageDir); removeErr != nil {
			return "", fmt.Errorf("discard duplicate snapshot template staging directory: %w", removeErr)
		}
		return destination, nil
	}
	if err := syncDirectory(c.root); err != nil {
		return "", fmt.Errorf("sync snapshot template cache root: %w", err)
	}
	return destination, nil
}

// Resolve returns the admitted template for a compatibility key. Admission
// verifies signature-anchored identity and every recorded digest exactly once;
// afterwards the per-start cost is a stat of three files. Concurrent callers
// for the same identity coalesce and never observe a partially verified result.
func (c *SnapshotTemplateCache) Resolve(key SnapshotTemplateKey) (*AdmittedSnapshotTemplate, error) {
	if c == nil {
		return nil, fmt.Errorf("SecondBox snapshot template cache is required")
	}
	templateID, err := key.TemplateID()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if template, ok := c.admitted[templateID]; ok {
		c.mu.Unlock()
		if err := template.VerifyStableIdentity(); err != nil {
			return nil, err
		}
		return template, nil
	}
	gate, ok := c.admitting[templateID]
	if !ok {
		gate = &sync.Mutex{}
		c.admitting[templateID] = gate
	}
	c.mu.Unlock()

	gate.Lock()
	defer gate.Unlock()
	c.mu.Lock()
	template, ok := c.admitted[templateID]
	c.mu.Unlock()
	if ok {
		if err := template.VerifyStableIdentity(); err != nil {
			return nil, err
		}
		return template, nil
	}
	template, err = admitSnapshotTemplate(filepath.Join(c.root, templateID), key, templateID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.admitted[templateID] = template
	c.mu.Unlock()
	return template, nil
}

func admitSnapshotTemplate(dir string, key SnapshotTemplateKey, templateID string) (*AdmittedSnapshotTemplate, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, snapshotTemplateManifestName))
	if err != nil {
		return nil, fmt.Errorf("read snapshot template %q manifest: %w", templateID, err)
	}
	var manifest SnapshotTemplateManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode snapshot template %q manifest: %w", templateID, err)
	}
	if err := manifest.validate(); err != nil {
		return nil, err
	}
	if manifest.TemplateID != templateID {
		return nil, fmt.Errorf("snapshot template %q manifest claims identity %q", templateID, manifest.TemplateID)
	}
	requestedID, err := key.TemplateID()
	if err != nil {
		return nil, err
	}
	if requestedID != manifest.TemplateID {
		return nil, fmt.Errorf("snapshot template %q does not match the requested compatibility key", templateID)
	}
	template := &AdmittedSnapshotTemplate{
		TemplateID:  templateID,
		Directory:   dir,
		VMStatePath: filepath.Join(dir, snapshotTemplateVMStateName),
		MemoryPath:  filepath.Join(dir, snapshotTemplateMemoryName),
		RootfsPath:  filepath.Join(dir, snapshotTemplateRootfsName),
		MemoryBytes: manifest.Memory.Bytes,
		Manifest:    manifest,
	}
	files := []struct {
		label string
		path  string
		file  SnapshotTemplateFile
	}{
		{"VM state", template.VMStatePath, manifest.VMState},
		{"memory", template.MemoryPath, manifest.Memory},
		{"rootfs", template.RootfsPath, manifest.Rootfs},
	}
	for _, entry := range files {
		if err := verifySnapshotTemplateFile(entry.path, entry.file); err != nil {
			return nil, err
		}
		identity, err := trustedMicroVMArtifactIdentityFor(entry.path)
		if err != nil {
			return nil, fmt.Errorf("stat snapshot template %s: %w", entry.label, err)
		}
		template.pinnedFiles = append(template.pinnedFiles, trustedMicroVMArtifactFile{
			label:    "snapshot template " + entry.label,
			path:     entry.path,
			identity: identity,
		})
	}
	template.admittedOnce = true
	return template, nil
}

func verifySnapshotTemplateFile(path string, want SnapshotTemplateFile) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat snapshot template file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot template file %q is not a regular file", path)
	}
	if info.Size() != want.Bytes {
		return fmt.Errorf(
			"snapshot template file %q is %d bytes, manifest records %d",
			path,
			info.Size(),
			want.Bytes,
		)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return fmt.Errorf("digest snapshot template file %q: %w", path, err)
	}
	if digest != want.SHA256 {
		return fmt.Errorf("snapshot template file %q does not match its recorded digest", path)
	}
	return nil
}

func writeSyncedFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %q: %w", path, err)
	}
	return os.Chmod(path, mode)
}

func syncSnapshotTemplateFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open snapshot template file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync snapshot template file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close snapshot template file %q: %w", path, err)
	}
	return os.Chmod(path, 0o444)
}

// hostCPUCompatibilityFingerprint identifies the host CPU closely enough that a
// template captured on an incompatible processor is a cache miss rather than a
// restore that faults in the guest. Firecracker restores the exact guest CPUID
// it captured, so vendor, family, model, and stepping are the compatibility
// boundary.
func hostCPUCompatibilityFingerprint() (string, error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("read host CPU compatibility fingerprint: %w", err)
	}
	defer file.Close()
	return hostCPUCompatibilityFingerprintFrom(bufio.NewScanner(file))
}

func hostCPUCompatibilityFingerprintFrom(scanner *bufio.Scanner) (string, error) {
	wanted := map[string]string{
		"vendor_id":  "",
		"cpu family": "",
		"model":      "",
		"stepping":   "",
		"flags":      "",
	}
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if _, ok := wanted[key]; !ok {
			continue
		}
		if wanted[key] != "" {
			continue
		}
		wanted[key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read host CPU compatibility fingerprint: %w", err)
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		if wanted[name] == "" {
			return "", fmt.Errorf("host CPU compatibility fingerprint field %q is absent", name)
		}
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(wanted[name])
		builder.WriteByte('\n')
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(digest[:]), nil
}
