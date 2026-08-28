package networkpolicy

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRunnerConfigFromEnvironment(t *testing.T) {
	complete := map[string]string{
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS":     "23",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL":      "41s",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES": "10.210.2.1, 2001:db8::1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS": "10.211.7.9/16, 2001:db8:1::1/64",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS":  "agent-gateway.secondbox.internal=10.210.2.2",
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
		config.CompileOptions.RunnerGateways["agent-gateway.secondbox.internal"].String() != "10.210.2.2" ||
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
		{name: "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS", value: "gateway.test", want: "domain=IP"},
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
		t.Setenv("SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS", "")
		_, err := LoadRunnerConfigFromEnvironment()
		if err == nil || !strings.Contains(err.Error(), "missing required") {
			t.Fatalf("missing configuration error = %v", err)
		}
	})
}

func TestLoadRunnerConfigFromEnvironmentAcceptsNoGateways(t *testing.T) {
	for name, value := range map[string]string{
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS":     "1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL":      "1s",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES": "10.0.0.1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS": "10.1.0.0/16",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS":  "none",
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM":     "1.1.1.1:53",
	} {
		t.Setenv(name, value)
	}
	config, err := LoadRunnerConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.CompileOptions.RunnerGateways) != 0 {
		t.Fatalf("runner gateways = %#v", config.CompileOptions.RunnerGateways)
	}
}
