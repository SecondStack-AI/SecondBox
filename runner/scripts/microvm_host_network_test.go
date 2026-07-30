package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerHostNetworkIsIdempotentDefaultDenyAndRecoverable(t *testing.T) {
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
  "-4 addr show dev sbx0") echo "inet 172.30.0.1/24" ;;
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
if [[ "$args" == *" -F "* ]]; then
  chain="$(printf '%s' "$args" | sed -E 's/.* -F ([^ ]+).*/\1/')"
  grep -Fv " -C $chain " "$state" > "$state.tmp" || true
  mv "$state.tmp" "$state"
  exit 0
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
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME=sbx0",
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR=172.30.0.1/24",
		"SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR=172.30.0.0/24",
		"SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX=sbx",
		"SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR="+runtimeDir,
		"SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE=false",
	)
	expectedState := "previous_ip_forward=0\nbridge_created=0\nbridge_address_added=0\nstatus=active\nconfig=sbx0|172.30.0.1/24|172.30.0.0/24|sbx+|false"
	runNetworkScript(t, env, "apply", true)
	assertFileText(t, sysctlState, "1")
	assertFileText(t, filepath.Join(runtimeDir, "host-network.state"), expectedState)
	assertFileContains(t, firewallState+".iptables", "-C SBX_INPUT_sbx0 -m comment --comment secondbox-runner-host-input-deny -j DROP")
	assertFileContains(t, firewallState+".iptables", "-C SBX_INPUT_sbx0 -d 172.30.0.1 -p udp --dport 53 -m comment --comment secondbox-runner-dns -j ACCEPT")
	assertFileContains(t, firewallState+".iptables", "-C SBX_FORWARD_sbx0 -m connmark --mark 0x53425801/0xffffffff -m comment --comment secondbox-sandbox-policy-allow -j ACCEPT")
	assertFileContains(t, firewallState+".iptables", "-C SBX_FORWARD_sbx0 -m comment --comment secondbox-sandbox-forward-deny -j DROP")
	assertFileContains(t, firewallState+".iptables", "-t nat -C POSTROUTING -s 172.30.0.0/24 ! -d 172.30.0.0/24 -m connmark --mark 0x53425801/0xffffffff -m comment --comment secondbox-sandbox-policy-nat -j MASQUERADE")
	assertFileContains(t, firewallState+".ip6tables", "-C SBX_IPV6_sbx0 -m comment --comment secondbox-sandbox-ipv6-deny -j DROP")

	secondRuntimeDir := filepath.Join(dir, "runtime-second")
	secondEnv := envWith(
		env,
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME=sbx1",
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR=172.31.0.1/24",
		"SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR=172.31.0.0/24",
		"SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX=sby",
		"SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR="+secondRuntimeDir,
	)
	runNetworkScript(t, secondEnv, "apply", true)
	assertFileContains(t, firewallState+".iptables", "-C SBX_FORWARD_sbx1 -m connmark --mark 0x53425801/0xffffffff -m comment --comment secondbox-sandbox-policy-allow -j ACCEPT")
	runNetworkScript(t, secondEnv, "remove", true)
	assertFileContains(t, firewallState+".iptables", "-C SBX_FORWARD_sbx0 -m connmark --mark 0x53425801/0xffffffff -m comment --comment secondbox-sandbox-policy-allow -j ACCEPT")
	assertFileContains(t, firewallState+".iptables", "-C FORWARD -i sbx0 -m comment --comment secondbox-sandbox-forward-out -j SBX_FORWARD_sbx0")

	if err := os.WriteFile(firewallState+".iptables", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firewallState+".ip6tables", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runNetworkScript(t, env, "apply", true)
	assertFileContains(t, firewallState+".iptables", "-C INPUT -i sbx0 -m comment --comment secondbox-runner-host-input -j SBX_INPUT_sbx0")

	logBefore := firewallLogSize(t, firewallLog)
	changedEnv := envWith(env, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR=172.31.0.1/24")
	runNetworkScript(t, changedEnv, "apply", false)
	if got := firewallLogSize(t, firewallLog); got != logBefore {
		t.Fatalf("changed active config mutated firewall: log size %d -> %d", logBefore, got)
	}

	runNetworkScript(t, env, "remove", true)
	assertFileText(t, sysctlState, "0")
	if _, err := os.Stat(filepath.Join(runtimeDir, "host-network.state")); !os.IsNotExist(err) {
		t.Fatalf("state remains after remove: %v", err)
	}

	logBefore = firewallLogSize(t, firewallLog)
	invalidEnv := envWith(env, "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX=prefix-too-long")
	runNetworkScript(t, invalidEnv, "apply", false)
	if got := firewallLogSize(t, firewallLog); got != logBefore {
		t.Fatalf("invalid config mutated firewall: log size %d -> %d", logBefore, got)
	}

	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	interruptedState := strings.Replace(expectedState, "status=active", "status=applying", 1)
	if err := os.WriteFile(filepath.Join(runtimeDir, "host-network.state"), []byte(interruptedState+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runNetworkScript(t, env, "apply", true)
	assertFileText(t, filepath.Join(runtimeDir, "host-network.state"), expectedState)
	runNetworkScript(t, env, "remove", true)
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
