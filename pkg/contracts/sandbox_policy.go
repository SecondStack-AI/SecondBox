package contracts

import (
	"fmt"
	"time"
)

type SandboxLifecycleLimits struct {
	IdleSeconds            PolicyLimit `json:"idleSeconds"`
	MaximumDurationSeconds PolicyLimit `json:"maximumDurationSeconds"`
}

func (limits *SandboxLifecycleLimits) UnmarshalJSON(data []byte) error {
	type wire SandboxLifecycleLimits
	var value wire
	if err := decodeCompletePolicy(data, &value, "idleSeconds", "maximumDurationSeconds"); err != nil {
		return err
	}
	*limits = SandboxLifecycleLimits(value)
	return nil
}

// SubjectSandboxPolicy selects lifecycle for future Sandboxes and connection
// policy for future attributed Assignments of one Profile.
type SubjectSandboxPolicy struct {
	Profile             string                               `json:"profile"`
	Lifecycle           SandboxLifecycleLimits               `json:"lifecycle"`
	AttributedExecution *AttributedExecutionConnectionLimits `json:"attributedExecution,omitempty"`
}

type SubjectSandboxPolicyObservation struct {
	SubjectRef          string                                    `json:"subjectRef"`
	Revision            int64                                     `json:"revision"`
	Profile             string                                    `json:"profile"`
	ProfileRevisionID   string                                    `json:"profileRevisionId"`
	Desired             *SubjectSandboxPolicy                     `json:"desired"`
	Effective           LifecyclePolicy                           `json:"effective"`
	Ceiling             SandboxLifecycleLimits                    `json:"ceiling"`
	Resources           ResourcePolicy                            `json:"resources"`
	ResourceCeiling     ProfileResourceCeiling                    `json:"resourceCeiling,omitzero"`
	Execution           ExecutionPolicy                           `json:"execution"`
	Retention           RetentionPolicy                           `json:"retention"`
	Quota               QuotaLimits                               `json:"quota"`
	TenantQuota         TenantQuota                               `json:"tenantQuota"`
	ObservedAt          time.Time                                 `json:"observedAt"`
	AttributedExecution *AttributedExecutionConnectionObservation `json:"attributedExecution"`
}

func (limits SandboxLifecycleLimits) Validate() error {
	for _, axis := range []struct {
		name  string
		limit PolicyLimit
	}{{"idleSeconds", limits.IdleSeconds}, {"maximumDurationSeconds", limits.MaximumDurationSeconds}} {
		if !axis.limit.Valid(1) || !axis.limit.IsUnlimited() && int64(axis.limit) > int64((1<<63-1)/time.Second) {
			return fmt.Errorf("SecondBox lifecycle %s must be null or positive representable seconds", axis.name)
		}
	}
	return nil
}

func (spec ProfileRevisionSpec) LifecycleCeilings() SandboxLifecycleLimits {
	if spec.LifecycleCeiling != nil {
		return *spec.LifecycleCeiling
	}
	return SandboxLifecycleLimits{IdleSeconds: spec.Lifecycle.IdleSeconds, MaximumDurationSeconds: spec.Lifecycle.MaximumDurationSeconds}
}

func (spec ProfileRevisionSpec) ResolveLifecycle(selection *SandboxLifecycleLimits) (LifecyclePolicy, error) {
	policy := spec.Lifecycle
	if selection == nil {
		return policy, nil
	}
	if err := selection.Validate(); err != nil {
		return LifecyclePolicy{}, err
	}
	ceiling := spec.LifecycleCeilings()
	policy.IdleSeconds = MinimumPolicyLimit(selection.IdleSeconds, ceiling.IdleSeconds)
	policy.MaximumDurationSeconds = MinimumPolicyLimit(selection.MaximumDurationSeconds, ceiling.MaximumDurationSeconds)
	return policy, nil
}

// ValidateLifecycleSelection rejects new finite requests above the current grant.
// Resolution still applies the current ceiling if an operator later tightens it.
func (spec ProfileRevisionSpec) ValidateLifecycleSelection(selection SandboxLifecycleLimits) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	ceiling := spec.LifecycleCeilings()
	for _, axis := range []struct {
		name           string
		value, ceiling PolicyLimit
	}{{"idleSeconds", selection.IdleSeconds, ceiling.IdleSeconds}, {"maximumDurationSeconds", selection.MaximumDurationSeconds, ceiling.MaximumDurationSeconds}} {
		if !axis.value.IsUnlimited() && !axis.value.Within(axis.ceiling) {
			return fmt.Errorf("SecondBox lifecycle %s exceeds Profile ceiling", axis.name)
		}
	}
	return nil
}
