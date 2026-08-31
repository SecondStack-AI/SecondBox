package networkpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const (
	EgressContextConfigSchemaVersion = "secondbox.runner-egress-contexts/v1"
	EgressContextConfigEnvironment   = "SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG"
	maximumEgressContextConfigBytes  = 1 << 20
	maximumEgressContexts            = 64
	maximumLogicalGatewaysPerContext = 128
	egressContextNameMaximumLength   = 63
)

var egressContextNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type egressContextConfigDocument struct {
	SchemaVersion string                   `json:"schemaVersion"`
	Contexts      *[]egressContextDocument `json:"contexts"`
}

type egressContextDocument struct {
	Name     string                   `json:"name"`
	Gateways []logicalGatewayDocument `json:"gateways"`
}

type logicalGatewayDocument struct {
	LogicalName string `json:"logicalName"`
	Address     string `json:"address"`
}

// EgressContextConfig is the immutable context-indexed Runner gateway
// authority loaded once before protocol registration or backend recovery.
type EgressContextConfig struct {
	contexts           map[string]map[string]netip.Addr
	protectedAddresses []netip.Addr
}

// LoadEgressContextConfig reads one strict, protected Runner-local file.
func LoadEgressContextConfig(path string) (EgressContextConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return EgressContextConfig{}, fmt.Errorf("%s must name a clean absolute path", EgressContextConfigEnvironment)
	}
	if err := rejectEgressContextConfigSymlinks(path); err != nil {
		return EgressContextConfig{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner inspect egress context config: %w", err)
	}
	if !before.Mode().IsRegular() {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config must be a regular file")
	}
	ownerUID, err := egressContextConfigOwnerUID(before)
	if err != nil {
		return EgressContextConfig{}, err
	}
	if err := validateEgressContextConfigMetadata(ownerUID, uint32(os.Geteuid()), before.Mode().Perm()); err != nil {
		return EgressContextConfig{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner open egress context config: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner inspect opened egress context config: %w", err)
	}
	if !os.SameFile(before, after) {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config changed while opening")
	}
	afterOwnerUID, err := egressContextConfigOwnerUID(after)
	if err != nil {
		return EgressContextConfig{}, err
	}
	if !after.Mode().IsRegular() {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config must remain a regular file while opening")
	}
	if err := validateEgressContextConfigMetadata(afterOwnerUID, uint32(os.Geteuid()), after.Mode().Perm()); err != nil {
		return EgressContextConfig{}, err
	}
	limited := io.LimitReader(file, maximumEgressContextConfigBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner read egress context config: %w", err)
	}
	if len(content) > maximumEgressContextConfigBytes {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config exceeds %d bytes", maximumEgressContextConfigBytes)
	}
	return decodeEgressContextConfig(content)
}

func rejectEgressContextConfigSymlinks(path string) error {
	current := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	trimmed := strings.TrimPrefix(path, current)
	for _, component := range strings.Split(trimmed, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("SecondBox Runner inspect egress context config path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SecondBox Runner egress context config path must not contain a symbolic link")
		}
	}
	return nil
}

func egressContextConfigOwnerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("SecondBox Runner egress context config ownership is unavailable")
	}
	return stat.Uid, nil
}

func validateEgressContextConfigMetadata(ownerUID, effectiveUID uint32, mode os.FileMode) error {
	if ownerUID != 0 && ownerUID != effectiveUID {
		return fmt.Errorf("SecondBox Runner egress context config must be owned by root or the Runner user")
	}
	if mode&0o133 != 0 || mode&0o400 == 0 {
		return fmt.Errorf("SecondBox Runner egress context config mode must be owner-readable and not executable, group-writable, or world-writable")
	}
	return nil
}

func decodeEgressContextConfig(content []byte) (EgressContextConfig, error) {
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var document egressContextConfigDocument
	if err := decoder.Decode(&document); err != nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner decode egress context config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config must contain exactly one JSON document")
	}
	if document.SchemaVersion != EgressContextConfigSchemaVersion {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config schemaVersion must be %q", EgressContextConfigSchemaVersion)
	}
	if document.Contexts == nil {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config contexts is required")
	}
	if len(*document.Contexts) > maximumEgressContexts {
		return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config exceeds %d contexts", maximumEgressContexts)
	}
	config := EgressContextConfig{contexts: make(map[string]map[string]netip.Addr, len(*document.Contexts))}
	protected := make(map[netip.Addr]struct{})
	for _, contextDocument := range *document.Contexts {
		if err := ValidateEgressContextName(contextDocument.Name); err != nil {
			return EgressContextConfig{}, err
		}
		if _, duplicate := config.contexts[contextDocument.Name]; duplicate {
			return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context config repeats context %q", contextDocument.Name)
		}
		if len(contextDocument.Gateways) == 0 {
			return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q must map at least one gateway", contextDocument.Name)
		}
		if len(contextDocument.Gateways) > maximumLogicalGatewaysPerContext {
			return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q exceeds %d gateways", contextDocument.Name, maximumLogicalGatewaysPerContext)
		}
		gateways := make(map[string]netip.Addr, len(contextDocument.Gateways))
		for _, gateway := range contextDocument.Gateways {
			logicalName, err := normalizeDomain(gateway.LogicalName)
			if err != nil {
				return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q logical name: %w", contextDocument.Name, err)
			}
			if logicalName != gateway.LogicalName {
				return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q logical name %q is not canonical", contextDocument.Name, gateway.LogicalName)
			}
			if _, duplicate := gateways[logicalName]; duplicate {
				return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q repeats logical name %q", contextDocument.Name, logicalName)
			}
			address, err := netip.ParseAddr(gateway.Address)
			if err != nil {
				return EgressContextConfig{}, fmt.Errorf("SecondBox Runner egress context %q logical name %q has an invalid IP", contextDocument.Name, logicalName)
			}
			address = normalizeAddress(address)
			gateways[logicalName] = address
			protected[address] = struct{}{}
		}
		config.contexts[contextDocument.Name] = gateways
	}
	config.protectedAddresses = make([]netip.Addr, 0, len(protected))
	for address := range protected {
		config.protectedAddresses = append(config.protectedAddresses, address)
	}
	slices.SortFunc(config.protectedAddresses, func(left, right netip.Addr) int {
		return left.Compare(right)
	})
	return config, nil
}

// ValidateEgressContextName is the Runner-side use of the canonical Task 1
// syntax. Every Runner configuration and protocol path calls this validator.
func ValidateEgressContextName(name string) error {
	if len(name) > egressContextNameMaximumLength || !egressContextNamePattern.MatchString(name) {
		return fmt.Errorf("SecondBox egress context name must contain 1 to 63 lowercase ASCII letters, digits, or hyphens and must begin and end with a letter or digit")
	}
	return nil
}

// ContextNames returns the immutable, sorted Runner advertisement set.
func (config EgressContextConfig) ContextNames() []string {
	names := make([]string, 0, len(config.contexts))
	for name := range config.contexts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// CompileOptionsForContext selects exactly one context mapping while retaining
// every configured gateway IP as a protected destination.
func (config EgressContextConfig) CompileOptionsForContext(contextName string, base CompileOptions) (CompileOptions, error) {
	gateways, found := config.contexts[contextName]
	if !found {
		return CompileOptions{}, fmt.Errorf("SecondBox Runner egress context %q is absent from configuration; a stopped Sandbox pinned to it cannot start until the mapping is restored or the Sandbox is retired", contextName)
	}
	base.RunnerAddresses = append([]netip.Addr(nil), base.RunnerAddresses...)
	base.ManagementPrefixes = append([]netip.Prefix(nil), base.ManagementPrefixes...)
	base.ProtectedAddresses = append([]netip.Addr(nil), config.protectedAddresses...)
	base.RunnerGateways = cloneGatewayMapping(gateways)
	return base, nil
}

func (config EgressContextConfig) compileOptionsWithoutContext(base CompileOptions) CompileOptions {
	base.RunnerAddresses = append([]netip.Addr(nil), base.RunnerAddresses...)
	base.ManagementPrefixes = append([]netip.Prefix(nil), base.ManagementPrefixes...)
	base.ProtectedAddresses = append([]netip.Addr(nil), config.protectedAddresses...)
	base.RunnerGateways = nil
	return base
}

func cloneGatewayMapping(source map[string]netip.Addr) map[string]netip.Addr {
	result := make(map[string]netip.Addr, len(source))
	for logicalName, address := range source {
		result[logicalName] = address
	}
	return result
}
