package networkpolicy

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// RunnerConfig is the complete generic runner network-policy environment
// contract shared by every network-enforcing compute backend.
type RunnerConfig struct {
	CompileOptions CompileOptions
	DNSUpstream    netip.AddrPort
	EgressContexts EgressContextConfig
}

// ContextNames returns the exact static set advertised by this Runner.
func (config RunnerConfig) ContextNames() []string {
	return config.EgressContexts.ContextNames()
}

// CompileOptionsForAssignment binds one required context or produces an
// isolated mapping-free policy while protecting every configured gateway.
func (config RunnerConfig) CompileOptionsForAssignment(contextName string, required bool) (CompileOptions, error) {
	if required {
		if err := ValidateEgressContextName(contextName); err != nil {
			return CompileOptions{}, err
		}
		return config.EgressContexts.CompileOptionsForContext(contextName, config.CompileOptions)
	}
	if contextName != "" {
		return CompileOptions{}, fmt.Errorf("SecondBox Runner network policy received an unexpected egress context")
	}
	return config.EgressContexts.compileOptionsWithoutContext(config.CompileOptions), nil
}

// LoadRunnerConfigFromEnvironment requires and parses every generic runner
// network-policy setting. Backend-specific composition must not replace any of
// these values with local defaults.
func LoadRunnerConfigFromEnvironment() (RunnerConfig, error) {
	required := func(name string) (string, error) {
		value, present := os.LookupEnv(name)
		value = strings.TrimSpace(value)
		if !present || value == "" {
			return "", fmt.Errorf("SecondBox runner network policy config missing required %s", name)
		}
		return value, nil
	}

	maximumPinsRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS")
	if err != nil {
		return RunnerConfig{}, err
	}
	maximumPins, err := strconv.Atoi(maximumPinsRaw)
	if err != nil || maximumPins < 1 {
		return RunnerConfig{}, fmt.Errorf("SecondBox runner network policy config requires positive integer SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS")
	}
	maximumTTLRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL")
	if err != nil {
		return RunnerConfig{}, err
	}
	maximumTTL, err := time.ParseDuration(maximumTTLRaw)
	if err != nil || maximumTTL <= 0 {
		return RunnerConfig{}, fmt.Errorf("SecondBox runner network policy config requires positive duration SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL")
	}

	runnerAddressesRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES")
	if err != nil {
		return RunnerConfig{}, err
	}
	runnerAddresses := make([]netip.Addr, 0, strings.Count(runnerAddressesRaw, ",")+1)
	for _, part := range strings.Split(runnerAddressesRaw, ",") {
		address, parseErr := netip.ParseAddr(strings.TrimSpace(part))
		if parseErr != nil {
			return RunnerConfig{}, fmt.Errorf("SecondBox runner network policy config SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES contains invalid address %q", part)
		}
		runnerAddresses = append(runnerAddresses, address)
	}

	managementCIDRsRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS")
	if err != nil {
		return RunnerConfig{}, err
	}
	managementPrefixes := make([]netip.Prefix, 0, strings.Count(managementCIDRsRaw, ",")+1)
	for _, part := range strings.Split(managementCIDRsRaw, ",") {
		prefix, parseErr := netip.ParsePrefix(strings.TrimSpace(part))
		if parseErr != nil {
			return RunnerConfig{}, fmt.Errorf("SecondBox runner network policy config SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS contains invalid CIDR %q", part)
		}
		managementPrefixes = append(managementPrefixes, prefix.Masked())
	}

	egressContextConfigPath, err := required(EgressContextConfigEnvironment)
	if err != nil {
		return RunnerConfig{}, err
	}
	egressContexts, err := LoadEgressContextConfig(egressContextConfigPath)
	if err != nil {
		return RunnerConfig{}, err
	}

	dnsUpstreamRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM")
	if err != nil {
		return RunnerConfig{}, err
	}
	dnsUpstream, err := netip.ParseAddrPort(dnsUpstreamRaw)
	if err != nil || dnsUpstream.Port() == 0 {
		return RunnerConfig{}, fmt.Errorf("SecondBox runner network policy config SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM must be an IP:port")
	}

	return RunnerConfig{
		CompileOptions: CompileOptions{
			MaximumPins:        maximumPins,
			MaximumTTL:         maximumTTL,
			RunnerAddresses:    runnerAddresses,
			ManagementPrefixes: managementPrefixes,
		},
		DNSUpstream:    dnsUpstream,
		EgressContexts: egressContexts,
	}, nil
}
