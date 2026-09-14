package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPolicyLimitsPreserveExplicitUnlimitedAndZero(t *testing.T) {
	for _, test := range []struct {
		name     string
		target   any
		document string
	}{
		{"runtime", &LifecyclePolicy{}, `{"initialState":"running","drainGraceSeconds":10,"idleSeconds":60,"maximumDurationSeconds":null,"leaseSeconds":60}`},
		{"idle", &LifecyclePolicy{}, `{"initialState":"running","drainGraceSeconds":10,"idleSeconds":null,"maximumDurationSeconds":900,"leaseSeconds":60}`},
		{"subject quota", &QuotaLimits{}, `{"maxSandboxes":null,"maxActiveInstances":null,"maxVcpuCount":null,"maxMemoryBytes":null,"maxSnapshots":0,"maxPortSessions":null,"maxConcurrentOperations":null}`},
		{"tenant quota", &TenantQuota{}, `{"maxSandboxes":null,"maxActiveInstances":null,"maxVcpuCount":null,"maxMemoryBytes":null,"maxSnapshots":0,"maxPortSessions":null,"maxConcurrentOperations":null,"maxActiveSubjects":null,"maxApplicationAuthorities":null}`},
		{"snapshots", &RetentionPolicy{}, `{"snapshotLimit":null,"snapshotRetentionSeconds":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(test.document), test.target); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(test.target)
			if err != nil {
				t.Fatal(err)
			}
			var want, got map[string]any
			if err := json.Unmarshal([]byte(test.document), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			for field, value := range want {
				if got[field] != value {
					t.Errorf("%s = %v, want %v", field, got[field], value)
				}
			}
		})
	}
}

func TestPolicyLimitObjectsRequireEveryDimension(t *testing.T) {
	for _, test := range []struct {
		name     string
		target   any
		document string
	}{
		{"subject quota", &QuotaLimits{}, `{"maxSandboxes":null}`},
		{"tenant quota", &TenantQuota{}, `{"maxSandboxes":null}`},
		{"lifecycle", &LifecyclePolicy{}, `{"initialState":"running","drainGraceSeconds":10,"idleSeconds":60,"leaseSeconds":60}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := json.Unmarshal([]byte(test.document), test.target)
			if err == nil || !strings.Contains(err.Error(), "required") {
				t.Fatalf("incomplete policy error = %v", err)
			}
		})
	}
}
