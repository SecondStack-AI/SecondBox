package install

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func plannerFacts(t *testing.T) HostFacts {
	t.Helper()
	facts := validPlan(t).HostFacts
	facts.BtrfsSupported = true
	facts.DNSUpstreams = []string{"192.0.2.53"}
	facts.Devices = []DeviceFact{
		{Path: "/dev/sdb", Identity: "8:16", Filesystem: "xfs", SizeBytes: 300 << 30, AvailableBytes: 240 << 30, Mountpoint: "/srv/secondbox-workspace", JailerCompatible: true},
		{Path: "/dev/sdc", Identity: "8:32", Filesystem: "ext4", SizeBytes: 300 << 30, AvailableBytes: 240 << 30, Mountpoint: "/srv/not-supported"},
		{Path: "/dev/root", Identity: "8:1", Filesystem: "btrfs", SizeBytes: 500 << 30, AvailableBytes: 300 << 30, Mountpoint: "/"},
	}
	facts.Routes = []RouteFact{{Destination: "172.30.0.0/24", Interface: "eth0"}, {Destination: "default", Interface: "eth0", Gateway: "192.0.2.1"}}
	return facts
}

func plannerInput(t *testing.T, choice StorageChoice) ProposalInput {
	t.Helper()
	return ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), DeploymentDirectory: "/home/operator/.local/share/secondbox", BinaryDirectory: "/home/operator/.local/bin", CLIConfigPath: "/home/operator/.config/secondbox/config.json", CLITenantRef: "tenant-reviewed", CLISubjectRef: "subject-reviewed", BackingAvailableBytes: 105 << 30, DeploymentAvailableBytes: 100 << 30, Release: validPlan(t).Release, StorageChoice: choice, ExistingMountpoint: "/srv/secondbox-workspace", StandardBundles: []string{"agent-compartment", "durable-coding"}, RetentionSeconds: 86400}
}

func TestProposalRequiresExplicitCLIIdentity(t *testing.T) {
	input := plannerInput(t, StorageBtrfsImage)
	input.CLISubjectRef = ""
	if _, err := ProposePlan(plannerFacts(t), input); err == nil || !strings.Contains(err.Error(), "explicit CLI") {
		t.Fatalf("implicit CLI identity proposal error = %v", err)
	}
}

func TestProposalRequiresExplicitStandardBundles(t *testing.T) {
	input := plannerInput(t, StorageBtrfsImage)
	input.StandardBundles = nil
	if _, err := ProposePlan(plannerFacts(t), input); err == nil || !strings.Contains(err.Error(), "standard bundles") {
		t.Fatalf("implicit standard bundle proposal error = %v", err)
	}
}

func TestStorageOptionsOfferOnlyDedicatedFilesystemOrBoundedImage(t *testing.T) {
	options := StorageOptions(plannerFacts(t), 100<<30, ExecutionBundleEstimateBytes)
	if len(options) != 2 {
		t.Fatalf("storage options = %#v", options)
	}
	if options[0].Mountpoint != "/srv/secondbox-workspace" || options[0].Filesystem != "xfs" || options[1].Choice != StorageBtrfsImage {
		t.Fatalf("unsafe or unexpected options = %#v", options)
	}
	if options[1].AvailableBytes != 69<<30 {
		t.Fatalf("image proposal = %d, want fully allocated capacity minus backing and release reserves", options[1].AvailableBytes)
	}
}

func TestStorageOptionsRejectMountOnRootDevice(t *testing.T) {
	facts := plannerFacts(t)
	facts.Devices = append(facts.Devices, DeviceFact{Path: "/dev/root", Identity: "8:1", Filesystem: "btrfs", SizeBytes: 300 << 30, AvailableBytes: 240 << 30, Mountpoint: "/srv/root-subvolume"})
	for _, option := range StorageOptions(facts, 100<<30, ExecutionBundleEstimateBytes) {
		if option.Mountpoint == "/srv/root-subvolume" {
			t.Fatal("a non-root mount on the root filesystem device was offered as dedicated storage")
		}
	}
}

func TestStorageOptionsRejectMountThatCannotHostJailer(t *testing.T) {
	facts := plannerFacts(t)
	facts.Devices[0].JailerCompatible = false
	for _, option := range StorageOptions(facts, 100<<30, ExecutionBundleEstimateBytes) {
		if option.Choice == StorageExistingMount {
			t.Fatalf("jailer-incompatible mount was offered: %#v", option)
		}
	}
}

func TestStorageOptionsRequireBackingForControlServicesAndReleaseAssets(t *testing.T) {
	if options := StorageOptions(plannerFacts(t), MinimumControlBackingBytes+ExecutionBundleEstimateBytes-1, ExecutionBundleEstimateBytes); len(options) != 0 {
		t.Fatalf("storage options with insufficient control backing = %#v", options)
	}
}

func TestStorageOptionsUseDistinctExistingMountAndImageMinimums(t *testing.T) {
	facts := plannerFacts(t)
	facts.Devices[0].AvailableBytes = MinimumRunnerStorageBytes
	options := StorageOptions(facts, 81<<30, ExecutionBundleEstimateBytes)
	if len(options) != 1 || options[0].Choice != StorageExistingMount {
		t.Fatalf("storage options at exact thresholds = %#v", options)
	}
}

func TestProposeExistingFilesystemPlanIsCompleteAndExplicit(t *testing.T) {
	plan, err := ProposePlan(plannerFacts(t), plannerInput(t, StorageExistingMount))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Storage.ExistingDeviceIdentity != "8:16" || plan.Storage.WorkspacePath != "/srv/secondbox-workspace/secondbox-install_0123456789abcdef/storage/workspaces" || plan.Capacity.MaxWorkspaceBytes != 225<<30 {
		t.Fatalf("storage = %#v", plan.Storage)
	}
	if len(plan.Capacity.SubjectQuotas) != 7 || len(plan.Network.Gateways) != 2 || plan.Network.GuestBridgeCIDR != "172.31.0.0/24" || plan.Network.ComposeBackendCIDR != "172.16.0.0/24" {
		t.Fatalf("capacity/network incomplete: %#v %#v", plan.Capacity, plan.Network)
	}
	if plan.Compute.FirecrackerCPUTemplate != SingleHostFirecrackerCPUTemplate {
		t.Fatalf("compute plan = %#v", plan.Compute)
	}
	if len(plan.SecretTargets) != 7 {
		t.Fatalf("secret targets = %#v", plan.SecretTargets)
	}
	retiredCategory := "application-" + "authority"
	for _, target := range plan.SecretTargets {
		if target.Category == retiredCategory {
			t.Fatalf("retired application secret target remains: %#v", target)
		}
	}
	if slices.Contains(plan.GeneratedAuthorityCategories, retiredCategory) {
		t.Fatalf("retired generated authority category remains: %#v", plan.GeneratedAuthorityCategories)
	}
	workspace, found := plannedPathByName(plan.Paths, "workspace")
	if !found || workspace.OwnerUID != runnerContainerUID || workspace.OwnerGID != runnerContainerGID {
		t.Fatalf("workspace ownership = %#v", workspace)
	}
	runnerStorage, _ := plannedPathByName(plan.Paths, "runner-storage")
	runnerRoot, _ := plannedPathByName(plan.Paths, "runner-root")
	artifactParent, _ := plannedPathByName(plan.Paths, "artifacts-parent")
	artifacts, _ := plannedPathByName(plan.Paths, "artifacts")
	run, _ := plannedPathByName(plan.Paths, "run")
	if filepath.Dir(workspace.Path) != runnerStorage.Path || !strings.HasPrefix(artifacts.Path, runnerStorage.Path+string(filepath.Separator)) || !strings.HasPrefix(run.Path, runnerStorage.Path+string(filepath.Separator)) {
		t.Fatalf("Runner assets, run state, and Workspaces are not colocated: storage=%#v artifacts=%#v run=%#v workspace=%#v", runnerStorage, artifacts, run, workspace)
	}
	if runnerRoot.Mode != 0o711 || runnerRoot.OwnerUID != 0 || runnerStorage.Mode != 0o711 || artifactParent.Mode != 0o700 || artifactParent.OwnerUID != plan.HostFacts.InvokingUID || artifacts.OwnerUID != plan.HostFacts.InvokingUID {
		t.Fatalf("artifact publication path is not traversable without exposing privileged Runner storage: root=%#v storage=%#v parent=%#v artifacts=%#v", runnerRoot, runnerStorage, artifactParent, artifacts)
	}
	state, _ := plannedPathByName(plan.Paths, "state")
	jail, _ := plannedPathByName(plan.Paths, "jail")
	if jail.Path != filepath.Join(runnerStorage.Path, "jail") {
		t.Fatalf("Runner jail is not the reviewed storage child: %#v", jail)
	}
	for _, name := range []string{"run", "network", "snapshot-template-cache", "firecracker-logs", "logs"} {
		planned, found := plannedPathByName(plan.Paths, name)
		if !found || !strings.HasPrefix(planned.Path, state.Path+string(filepath.Separator)) {
			t.Fatalf("runner path %s is outside state bind mount: %#v", name, planned)
		}
	}
	logs, _ := plannedPathByName(plan.Paths, "logs")
	if logs.OwnerUID != runnerContainerUID || logs.OwnerGID != runnerContainerGID {
		t.Fatalf("runner log ownership = %#v", logs)
	}
	firecrackerLogs, _ := plannedPathByName(plan.Paths, "firecracker-logs")
	if firecrackerLogs.OwnerUID != 0 || firecrackerLogs.OwnerGID != 0 || firecrackerLogs.Mode != 0o700 || firecrackerLogs.Path == logs.Path {
		t.Fatalf("Firecracker log isolation = Runner %#v Firecracker %#v", logs, firecrackerLogs)
	}
	encoded, err := Canonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secretValue", "privateKeyPem", "bearerToken"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("plan contains secret-bearing field %q", forbidden)
		}
	}
}

func TestProposeImagePlanBoundsAllocationAndReplacesPortCollisions(t *testing.T) {
	facts := plannerFacts(t)
	facts.ListeningPorts = []PortFact{{Address: "127.0.0.1", Port: 8080}, {Address: "127.0.0.1", Port: 9443}, {Address: "127.0.0.1", Port: 9444}}
	input := plannerInput(t, StorageBtrfsImage)
	plan, err := ProposePlan(facts, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Storage.ImageSizeBytes != 72<<30 || plan.Capacity.MaxWorkspaceBytes != 57<<30 {
		t.Fatalf("image capacity = %#v", plan.Storage)
	}
	if plan.Network.APIAddress != "127.0.0.1:8081" || plan.Network.RunnerAddress != "127.0.0.1:9445" || plan.Network.DataPlaneAddress != "127.0.0.1:9446" {
		t.Fatalf("replacement ports = %#v", plan.Network)
	}
	if !strings.Contains(RenderPlanReview(plan), "Compute: Firecracker CPU template None") || !strings.Contains(RenderPlanReview(plan), "Existing SecondBox CLIs and CLI configuration") || !strings.Contains(RenderPlanReview(plan), "Ordinary uninstall preserves") || !strings.Contains(RenderPlanReview(plan), "Paths requiring sudo") {
		t.Fatalf("review omitted durable or privilege boundary:\n%s", RenderPlanReview(plan))
	}
	if plan.Capacity.MaxCPUMillis < DurableCodingCPUMillis || plan.Capacity.MaxMemoryBytes < DurableCodingMemoryBytes || plan.Capacity.MaxWorkspaceBytes < MinimumWorkspaceBytes || plan.Capacity.ConcurrentOperations < plan.Capacity.MaxSandboxes*DurableCodingConcurrentOperations || plan.Capacity.SubjectQuotas["maxConcurrentOperations"] < plan.Capacity.SubjectQuotas["maxActiveInstances"]*DurableCodingConcurrentOperations {
		t.Fatalf("proposal cannot run durable-coding: %#v", plan.Capacity)
	}
}

func TestProposePlanReplacesBundledServicePortCollisions(t *testing.T) {
	facts := plannerFacts(t)
	facts.ListeningPorts = []PortFact{{Port: 5432}}
	plan, err := ProposePlan(facts, plannerInput(t, StorageExistingMount))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.DatabaseAddress != "127.0.0.1:5433" {
		t.Fatalf("bundled service replacements = %#v", plan.Network)
	}
}

func TestPlannerRejectsUnsafeOrUnreviewedChoices(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HostFacts, *ProposalInput)
	}{
		{"physical device", func(_ *HostFacts, input *ProposalInput) { input.ExistingMountpoint = "/dev/sdb" }},
		{"oversized image", func(_ *HostFacts, input *ProposalInput) {
			input.StorageChoice = StorageBtrfsImage
			input.FilesystemImageBytes = 99 << 30
		}},
		{"occupied advanced port", func(facts *HostFacts, input *ProposalInput) {
			facts.ListeningPorts = []PortFact{{Port: 10000}}
			input.NetworkOverrides.APIPort = 10000
		}},
		{"route collision", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.GuestCIDR = "172.30.0.0/24" }},
		{"Compose and guest collision", func(_ *HostFacts, input *ProposalInput) {
			input.NetworkOverrides.GuestCIDR = "10.10.0.0/24"
			input.NetworkOverrides.ComposeCIDR = "10.10.0.0/24"
		}},
		{"Compose and Docker collision", func(facts *HostFacts, input *ProposalInput) {
			facts.DockerNetworkSubnets = []string{"10.20.0.0/16"}
			input.NetworkOverrides.ComposeCIDR = "10.20.1.0/24"
		}},
		{"loopback DNS", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.DNSUpstream = "127.0.0.53" }},
		{"unsafe deployment", func(_ *HostFacts, input *ProposalInput) { input.DeploymentDirectory = "/" }},
		{"small deployment filesystem", func(_ *HostFacts, input *ProposalInput) { input.DeploymentAvailableBytes = MinimumDeploymentBytes - 1 }},
		{"IPv6 guest network", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.GuestCIDR = "fd00::/64" }},
		{"public Compose network", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.ComposeCIDR = "198.51.100.0/24" }},
		{"undersized Compose network", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.ComposeCIDR = "10.42.0.0/30" }},
		{"guest network without usable addresses", func(_ *HostFacts, input *ProposalInput) { input.NetworkOverrides.GuestCIDR = "192.0.2.0/31" }},
		{"overflowing jailer range", func(_ *HostFacts, input *ProposalInput) {
			input.NetworkOverrides.JailerUID = UIDRange{Start: int64(^uint32(0)), Count: 2}
		}},
		{"missing retention decision", func(_ *HostFacts, input *ProposalInput) { input.RetentionSeconds = 0 }},
		{"subordinate ID collision", func(facts *HostFacts, input *ProposalInput) {
			facts.ReservedIDRanges = []UIDRange{{Start: 200000, Count: 65536}}
			input.NetworkOverrides.JailerUID = UIDRange{Start: 200000, Count: 64}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, input := plannerFacts(t), plannerInput(t, StorageExistingMount)
			test.mutate(&facts, &input)
			if _, err := ProposePlan(facts, input); err == nil {
				t.Fatal("unsafe proposal succeeded")
			}
		})
	}
}

func TestAutomaticGuestCIDRSearchesBeyondPreferredSubnets(t *testing.T) {
	routes := []RouteFact{{Destination: "172.30.0.0/24"}, {Destination: "172.31.0.0/16"}, {Destination: "0.0.0.0/0"}}
	if candidate := freeRFC1918CIDR(observedIPv4RoutePrefixes(routes)); candidate != "172.16.0.0/24" {
		t.Fatalf("automatic CIDR did not continue through RFC1918 space: %s", candidate)
	}
}

func TestAutomaticGuestCIDRFallsBackAcrossRFC1918Pools(t *testing.T) {
	routes := []RouteFact{
		{Destination: "172.16.0.0/12"},
		{Destination: "10.0.0.0/8"},
		{Destination: "192.168.0.0/25"},
	}
	if candidate := freeRFC1918CIDR(observedIPv4RoutePrefixes(routes)); candidate != "192.168.1.0/24" {
		t.Fatalf("automatic CIDR did not reach the remaining RFC1918 pool: %s", candidate)
	}
}

func TestAutomaticGuestCIDRRejectsExhaustedRFC1918Space(t *testing.T) {
	routes := []RouteFact{
		{Destination: "172.16.0.0/12"},
		{Destination: "10.0.0.0/8"},
		{Destination: "192.168.0.0/16"},
	}
	if candidate := freeRFC1918CIDR(observedIPv4RoutePrefixes(routes)); candidate != "" {
		t.Fatalf("automatic CIDR escaped RFC1918 space: %s", candidate)
	}
}

func TestPlanChoosesDistinctCIDRsBeyondDockerAllocations(t *testing.T) {
	facts := plannerFacts(t)
	facts.Routes = []RouteFact{{Destination: "172.30.0.0/24"}, {Destination: "192.168.240.0/20"}}
	facts.DockerNetworkSubnets = []string{"172.16.0.0/12", "192.168.0.0/16", "10.0.0.0/24"}
	plan, err := ProposePlan(facts, plannerInput(t, StorageExistingMount))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.GuestBridgeCIDR != "10.0.1.0/24" || plan.Network.ComposeBackendCIDR != "10.0.2.0/24" {
		t.Fatalf("selected networks = guest %s, Compose %s", plan.Network.GuestBridgeCIDR, plan.Network.ComposeBackendCIDR)
	}
	if review := RenderPlanReview(plan); !strings.Contains(review, "guests 10.0.1.0/24, Compose backend 10.0.2.0/24") {
		t.Fatalf("plan review omitted explicit networks:\n%s", review)
	}
}

func TestProposalReportsExhaustedRFC1918Space(t *testing.T) {
	facts := plannerFacts(t)
	facts.Routes = []RouteFact{
		{Destination: "172.16.0.0/12"},
		{Destination: "10.0.0.0/8"},
		{Destination: "192.168.0.0/16"},
	}
	_, err := ProposePlan(facts, plannerInput(t, StorageExistingMount))
	if err == nil || !strings.Contains(err.Error(), "no collision-free RFC1918 guest /24") {
		t.Fatalf("RFC1918 exhaustion error = %v", err)
	}
}

func TestOperationIDsAreRandomAndValid(t *testing.T) {
	one, err := NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !operationPattern.MatchString(one) || !operationPattern.MatchString(two) {
		t.Fatalf("operation IDs = %q, %q", one, two)
	}
}
