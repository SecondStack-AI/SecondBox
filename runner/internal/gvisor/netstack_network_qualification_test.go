//go:build linux && listener_qualification

package gvisor

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
)

// Run only in an isolated network namespace with NET_ADMIN and SYS_ADMIN.
// This qualifies 1024 concurrent kernel links, not 1024 running sentries.
func TestNetworkSlotExpansionQualification(t *testing.T) {
	ctx := t.Context()
	neighbors := []instanceNetwork{networkForIndex(14, 62), networkForIndex(14, 255), networkForIndex(0, 256), networkForIndex(14, 1023)}
	create := func(network instanceNetwork) {
		t.Helper()
		t.Cleanup(func() {
			if err := destroyInstanceNetwork(context.Background(), network); err != nil {
				t.Error(err)
			}
		})
		if err := createInstanceNetwork(ctx, network); err != nil {
			t.Fatal(err)
		}
		// Both namespace routing and the host's connected route must select
		// this exact link, including across banks and other live profiles.
		for _, args := range [][]string{
			{"route", "get", network.guestAddress},
			{"-n", network.namespaceName, "route", "get", "93.184.216.11"},
		} {
			output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
			want := network.hostVeth
			if args[0] == "-n" {
				want = network.guestVeth
			}
			if err != nil || !strings.Contains(string(output), "dev "+want+" ") {
				t.Fatalf("route for %+v: %v: %s", network, err, output)
			}
		}
	}
	install := func(network instanceNetwork) string {
		t.Helper()
		table := fmt.Sprintf("secondbox_slot_%s", strings.ReplaceAll(network.hostVeth, "-", "_"))
		script := renderInetPolicy(table, network.hostVeth, network.guestAddress,
			netip.Addr{}, false, nil, nil, nil, nil)
		t.Cleanup(func() {
			if err := removeResidualPolicyTables(context.Background(), table); err != nil {
				t.Error(err)
			}
		})
		command := exec.CommandContext(ctx, "nft", "-f", "-")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("install slot policy: %v: %s", err, output)
		}
		return table
	}
	var neighborTables []string
	for _, neighbor := range neighbors {
		create(neighbor)
		neighborTables = append(neighborTables, install(neighbor))
	}
	for index := uint32(0); index < maximumNetworkSlots; index++ {
		create(networkForIndex(15, index))
	}
	for _, index := range []uint32{62, 63, 64, 127, 128, 191, 192, 255, 256, 319, 320, 959, 960, 999, 1023} {
		network := networkForIndex(15, index)
		table := install(network)
		if index == maximumNetworkSlots-1 {
			// Reconciliation must also find a NAT-only crash leftover.
			if output, err := exec.CommandContext(ctx, "nft", "delete", "table", "inet", table).CombinedOutput(); err != nil {
				t.Fatalf("leave orphaned NAT: %v: %s", err, output)
			}
		}
	}
	if err := reconcileStaleNetworks(ctx, 15); err != nil {
		t.Fatal(err)
	}
	for _, neighbor := range neighbors {
		for _, args := range [][]string{{"link", "show", neighbor.hostVeth}, {"-n", neighbor.namespaceName, "link", "show", neighbor.guestVeth}} {
			if output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
				t.Fatalf("reconciliation damaged other profile: %v: %s", err, output)
			}
		}
	}
	for _, family := range []string{"inet", "ip"} {
		output, err := exec.CommandContext(ctx, "nft", "list", "tables", family).CombinedOutput()
		if err != nil || strings.Contains(string(output), "secondbox_slot_gvh15_") {
			t.Fatalf("profile policy sweep (%s): %v: %s", family, err, output)
		}
		for _, table := range neighborTables {
			if !strings.Contains(string(output), table) {
				t.Fatalf("profile sweep removed neighboring policy %s (%s)", table, family)
			}
		}
	}
	for index := uint32(0); index < maximumNetworkSlots; index++ {
		network := networkForIndex(15, index)
		if exec.CommandContext(ctx, "ip", "link", "show", network.hostVeth).Run() == nil ||
			exec.CommandContext(ctx, "ip", "-n", network.namespaceName, "link", "show").Run() == nil {
			t.Fatalf("reconciliation left slot %d resources", index)
		}
	}
	// Startup leaves the highest slot ready for a replacement generation.
	create(networkForIndex(15, maximumNetworkSlots-1))
}
