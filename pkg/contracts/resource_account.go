package contracts

import (
	"regexp"
	"time"
)

var resourceAccountKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:@._-]{0,255}$`)

func ValidResourceAccountKey(key string) bool { return resourceAccountKeyPattern.MatchString(key) }

// ResourceAccount carries trusted application policy for one compute admission.
type ResourceAccount struct {
	Key    string                `json:"key"`
	Limits ResourceAccountLimits `json:"limits"`
}

func (account *ResourceAccount) UnmarshalJSON(data []byte) error {
	type wire ResourceAccount
	var decoded wire
	if err := decodeCompletePolicy(data, &decoded, "key", "limits"); err != nil {
		return err
	}
	if !ValidResourceAccountKey(decoded.Key) {
		return &RequestFieldError{Field: "key", Reason: "must be a URL-safe resource account key of at most 256 characters"}
	}
	*account = ResourceAccount(decoded)
	return nil
}

func (account ResourceAccount) optional() *ResourceAccount {
	if account.Key == "" {
		return nil
	}
	return &account
}

func (request *CreateSandboxRequest) UnmarshalJSON(data []byte) error {
	type wire CreateSandboxRequest
	var decoded struct {
		wire
		ResourceAccount ResourceAccount `json:"resourceAccount"`
	}
	if err := decodeCompletePolicy(data, &decoded); err != nil {
		return err
	}
	*request = CreateSandboxRequest(decoded.wire)
	request.ResourceAccount = decoded.ResourceAccount.optional()
	return nil
}

func (account *ResourceAccount) AccountKey() string {
	if account == nil {
		return ""
	}
	return account.Key
}

type ResourceAccountLimits struct {
	MaxActiveInstances PolicyLimit `json:"maxActiveInstances"`
	MaxVCPUCount       PolicyLimit `json:"maxVcpuCount"`
	MaxMemoryBytes     PolicyLimit `json:"maxMemoryBytes"`
}

func (limits *ResourceAccountLimits) UnmarshalJSON(data []byte) error {
	type wire ResourceAccountLimits
	var decoded wire
	if err := decodeCompletePolicy(data, &decoded, "maxActiveInstances", "maxVcpuCount", "maxMemoryBytes"); err != nil {
		return err
	}
	*limits = ResourceAccountLimits(decoded)
	return nil
}

type ResourceAccountAllocation struct {
	SandboxID    string `json:"sandboxId"`
	State        string `json:"state"`
	DesiredState string `json:"desiredState"`
	VCPUCount    int64  `json:"vcpuCount"`
	MemoryBytes  int64  `json:"memoryBytes"`
}

type ResourceAccountUsage struct {
	AccountKey      string                      `json:"accountKey"`
	ObservedAt      time.Time                   `json:"observedAt"`
	ActiveInstances int64                       `json:"activeInstances"`
	VCPUCount       int64                       `json:"vcpuCount"`
	MemoryBytes     int64                       `json:"memoryBytes"`
	Allocations     []ResourceAccountAllocation `json:"allocations"`
}

type ResourceAccountQuotaRefusal struct {
	AccountKey string `json:"accountKey"`
	Axis       string `json:"axis"`
	Used       int64  `json:"used"`
	Requested  int64  `json:"requested"`
	Limit      int64  `json:"limit"`
}
