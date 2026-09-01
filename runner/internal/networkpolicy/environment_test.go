package networkpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRunnerConfigFromEnvironment(t *testing.T) {
	contextConfigPath := writeEgressContextConfig(t, validEgressContextConfig, 0o600)
	complete := map[string]string{
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS":     "23",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL":      "41s",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES": "10.210.2.1, 2001:db8::1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS": "10.211.7.9/16, 2001:db8:1::1/64",
		EgressContextConfigEnvironment:                     contextConfigPath,
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM":     "10.0.0.53:53",
	}
	for name, value := range complete {
		t.Setenv(name, value)
	}
	config, err := LoadRunnerConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config.CompileOptions.MaximumPins != 23 || config.CompileOptions.MaximumTTL != 41*time.Second ||
		len(config.CompileOptions.RunnerAddresses) != 2 ||
		config.CompileOptions.ManagementPrefixes[0].String() != "10.211.0.0/16" ||
		config.CompileOptions.ManagementPrefixes[1].String() != "2001:db8:1::/64" ||
		strings.Join(config.ContextNames(), ",") != "installation-a,installation-b" ||
		config.DNSUpstream.String() != "10.0.0.53:53" {
		t.Fatalf("network policy config = %#v", config)
	}

	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS", value: "0", want: "positive integer"},
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL", value: "never", want: "positive duration"},
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES", value: "hostname", want: "invalid address"},
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS", value: "10.0.0.1", want: "invalid CIDR"},
		{name: EgressContextConfigEnvironment, value: "contexts.json", want: "absolute"},
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM", value: "dns.test:53", want: "IP:port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			_, err := LoadRunnerConfigFromEnvironment()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		t.Setenv(EgressContextConfigEnvironment, "")
		_, err := LoadRunnerConfigFromEnvironment()
		if err == nil || !strings.Contains(err.Error(), "missing required") {
			t.Fatalf("missing configuration error = %v", err)
		}
	})
}

func TestLoadRunnerConfigFromEnvironmentAcceptsNoContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS":     "1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL":      "1s",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES": "10.0.0.1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS": "10.1.0.0/16",
		EgressContextConfigEnvironment:                     path,
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM":     "1.1.1.1:53",
	} {
		t.Setenv(name, value)
	}
	config, err := LoadRunnerConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ContextNames()) != 0 {
		t.Fatalf("runner contexts = %#v", config.ContextNames())
	}
}
