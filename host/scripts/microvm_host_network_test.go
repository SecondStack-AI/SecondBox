package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMicroVMHostNetworkApplyTwiceRemoveAndFailedReapply(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "id"), `#!/bin/sh
if [ "${1:-}" = "-u" ]; then echo 0; else /usr/bin/id "$@"; fi
`)
	writeExecutable(t, filepath.Join(binDir, "sysctl"), `#!/bin/sh
set -eu
if [ "$1" = "-n" ]; then cat "$FAKE_SYSCTL_STATE"; exit 0; fi
if [ "$1" = "-w" ]; then
  value="${2#*=}"
  printf '%s\n' "$value" > "$FAKE_SYSCTL_STATE"
  printf '%s = %s\n' "${2%%=*}" "$value"
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(binDir, "ip"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_IP_LOG"
case "$*" in
  "route show default 0.0.0.0/0") echo "default via 192.0.2.1 dev eth0" ;;
  "-4 addr show dev agfc0") echo "inet 172.30.0.1/24" ;;
esac
exit 0
`)
	firewall := `#!/bin/bash
set -euo pipefail
name="${0##*/}"
state="${FAKE_FIREWALL_STATE}.${name}"
touch "$state"
printf '%s\n' "$*" >> "${FAKE_FIREWALL_LOG}.${name}"
args=" $* "
if [[ "$args" == *" -C "* ]]; then
  key="$(printf '%s' "$args" | xargs)"
  grep -Fxq "$key" "$state"
  exit
fi
if [[ "$args" == *" -I "* ]]; then
  key="$(printf '%s' "$args" | sed -E 's/ -I ([^ ]+) 1 / -C \1 /' | xargs)"
  grep -Fxq "$key" "$state" || printf '%s\n' "$key" >> "$state"
  exit 0
fi
if [[ "$args" == *" -A "* ]]; then
  key="$(printf '%s' "$args" | sed 's/ -A / -C /' | xargs)"
  grep -Fxq "$key" "$state" || printf '%s\n' "$key" >> "$state"
  exit 0
fi
if [[ "$args" == *" -D "* ]]; then
  key="$(printf '%s' "$args" | sed 's/ -D / -C /' | xargs)"
  grep -Fxv "$key" "$state" > "$state.tmp" || true
  mv "$state.tmp" "$state"
  exit 0
fi
exit 0
`
	writeExecutable(t, filepath.Join(binDir, "iptables"), firewall)
	writeExecutable(t, filepath.Join(binDir, "ip6tables"), firewall)

	sysctlState := filepath.Join(dir, "ip-forward")
	if err := os.WriteFile(sysctlState, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(dir, "runtime")
	firewallState := filepath.Join(dir, "firewall")
	firewallLog := filepath.Join(dir, "firewall-log")
	ipLog := filepath.Join(dir, "ip-log")
	env := append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"FAKE_SYSCTL_STATE="+sysctlState,
		"FAKE_FIREWALL_STATE="+firewallState,
		"FAKE_FIREWALL_LOG="+firewallLog,
		"FAKE_IP_LOG="+ipLog,
		"SANDBOX_HOST_MICROVM_BRIDGE_NAME=agfc0",
		"SANDBOX_HOST_MICROVM_BRIDGE_CIDR=172.30.0.1/24",
		"SANDBOX_HOST_MICROVM_GUEST_CIDR=172.30.0.0/24",
		"SANDBOX_HOST_MICROVM_TAP_PREFIX=agfc",
		"SANDBOX_HOST_EGRESS_PROXY_LISTEN_PORT=3128",
		"SANDBOX_HOST_MICROVM_NETWORK_STATE_DIR="+runtimeDir,
		"SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT=18080",
		"SANDBOX_HOST_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP=false",
		"SANDBOX_HOST_MICROVM_ENABLE_DIRECT_NAT=false",
		"SANDBOX_HOST_MICROVM_EGRESS_IFACE=",
		"SANDBOX_HOST_MICROVM_DELETE_BRIDGE=false",
		"SANDBOX_HOST_AGENT_SERVICE_PRIVATE_LISTEN_PORT=8081",
		"SANDBOX_HOST_AGENT_SERVICE_PRIVATE_IFACES=lo,agfc0,agh+",
	)
	expectedState := "previous=0\nstatus=active\nbridge_created=0\nbridge_address_added=0\nconfig=agfc0|172.30.0.1/24|172.30.0.0/24|agfc+|3128|18080|false|false||8081|lo,agfc0,agh+"
	runNetworkScript(t, env, "apply", true)
	assertFileText(t, sysctlState, "1")
	assertFileText(t, filepath.Join(runtimeDir, "ip-forward.previous"), expectedState)
	iptablesLog, err := os.ReadFile(firewallLog + ".iptables")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(iptablesLog), "SANDBOX_HOST_HARNESS_IN 1 -p tcp --dport 3128") ||
		strings.Contains(string(iptablesLog), "SANDBOX_HOST_HARNESS_IN 1 -p tcp --dport 18080") ||
		!strings.Contains(string(iptablesLog), "SANDBOX_HOST_GUEST_IN 1 -p tcp --dport 18080") {
		t.Fatalf("harness/VM proxy input policy is not least privilege:\n%s", iptablesLog)
	}

	if err := os.WriteFile(firewallState+".iptables", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firewallState+".ip6tables", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runNetworkScript(t, env, "apply", true)
	assertFileContains(t, firewallState+".iptables", "-C INPUT -i agfc0 -m comment --comment sandbox-host-guest-input -j SANDBOX_HOST_GUEST_IN")
	assertFileContains(t, firewallState+".ip6tables", "-C INPUT -i agfc0 -m comment --comment sandbox-host-guest-ipv6-input -j SANDBOX_HOST_GUEST6_IN")
	assertFileText(t, filepath.Join(runtimeDir, "ip-forward.previous"), expectedState)

	logBefore := firewallLogSize(t, firewallLog)
	changedEnv := envWith(env, "SANDBOX_HOST_AGENT_SERVICE_PRIVATE_LISTEN_PORT=9090")
	runNetworkScript(t, changedEnv, "apply", false)
	if got := firewallLogSize(t, firewallLog); got != logBefore {
		t.Fatalf("changed active config mutated firewall: log size %d -> %d", logBefore, got)
	}

	runNetworkScript(t, changedEnv, "remove", true)
	assertFileText(t, sysctlState, "0")
	if _, err := os.Stat(filepath.Join(runtimeDir, "ip-forward.previous")); !os.IsNotExist(err) {
		t.Fatalf("rollback state remains after remove: %v", err)
	}

	logBefore = firewallLogSize(t, firewallLog)
	badEnv := envWith(env,
		"SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT=",
		"SANDBOX_HOST_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP=true",
	)
	runNetworkScript(t, badEnv, "apply", false)
	if got := firewallLogSize(t, firewallLog); got != logBefore {
		t.Fatalf("invalid preflight config mutated firewall: log size %d -> %d", logBefore, got)
	}
	assertFileText(t, sysctlState, "0")
	if _, err := os.Stat(filepath.Join(runtimeDir, "ip-forward.previous")); !os.IsNotExist(err) {
		t.Fatalf("rollback state created for invalid preflight config: %v", err)
	}

	interruptedState := strings.Replace(expectedState, "status=active", "status=applying", 1)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "ip-forward.previous"), []byte(interruptedState+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectedChangedState := strings.Replace(expectedState, "||8081|", "||9090|", 1)
	runNetworkScript(t, changedEnv, "apply", true)
	assertFileText(t, filepath.Join(runtimeDir, "ip-forward.previous"), expectedChangedState)
	runNetworkScript(t, changedEnv, "remove", true)

	runNetworkScript(t, env, "apply", true)
	logBefore = firewallLogSize(t, firewallLog)
	if err := os.WriteFile(sysctlState, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runNetworkScript(t, env, "apply", false)
	if got := firewallLogSize(t, firewallLog); got != logBefore {
		t.Fatalf("failed active reapply tore down firewall: log size %d -> %d", logBefore, got)
	}
	assertFileText(t, filepath.Join(runtimeDir, "ip-forward.previous"), expectedState)
}

func runNetworkScript(t *testing.T, env []string, mode string, wantOK bool) {
	t.Helper()
	cmd := exec.Command("bash", "microvm-host-network-setup.sh", mode)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if wantOK && err != nil {
		t.Fatalf("network script %s: %v\n%s", mode, err, output)
	}
	if !wantOK && err == nil {
		t.Fatalf("network script %s unexpectedly succeeded:\n%s", mode, output)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func envWith(env []string, overrides ...string) []string {
	result := append([]string{}, env...)
	for _, override := range overrides {
		name := strings.SplitN(override, "=", 2)[0]
		prefix := name + "="
		filtered := result[:0]
		for _, item := range result {
			if !strings.HasPrefix(item, prefix) {
				filtered = append(filtered, item)
			}
		}
		result = append(filtered, override)
	}
	return result
}

func assertFileText(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, data)
	}
}

func firewallLogSize(t *testing.T, prefix string) int {
	t.Helper()
	total := 0
	for _, suffix := range []string{".iptables", ".ip6tables"} {
		data, err := os.ReadFile(prefix + suffix)
		if err != nil {
			t.Fatal(err)
		}
		total += len(data)
	}
	return total
}
