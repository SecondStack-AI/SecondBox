package contracts

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PolicyLimit is a finite nonnegative ceiling or Unlimited. Its wire and SQL
// representation for Unlimited is null; zero remains a finite ceiling.
type PolicyLimit int64

const Unlimited PolicyLimit = -1

func (limit PolicyLimit) IsUnlimited() bool { return limit == Unlimited }
func (limit PolicyLimit) Valid(minimum int64) bool {
	return limit.IsUnlimited() || int64(limit) >= minimum
}
func (limit PolicyLimit) Allows(value int64) bool {
	return limit.IsUnlimited() || value <= int64(limit)
}
func (limit PolicyLimit) Remaining(used int64) PolicyLimit {
	if limit.IsUnlimited() {
		return Unlimited
	}
	return PolicyLimit(max(0, int64(limit)-used))
}
func (limit PolicyLimit) Within(parent PolicyLimit) bool {
	return parent.IsUnlimited() || !limit.IsUnlimited() && limit <= parent
}
func MinimumPolicyLimit(left, right PolicyLimit) PolicyLimit {
	if left.IsUnlimited() {
		return right
	}
	if right.IsUnlimited() {
		return left
	}
	return min(left, right)
}
func (limit PolicyLimit) MarshalJSON() ([]byte, error) {
	if limit.IsUnlimited() {
		return []byte("null"), nil
	}
	if limit < 0 {
		return nil, errors.New("SecondBox policy limit must be nonnegative or Unlimited")
	}
	return json.Marshal(int64(limit))
}
func (limit *PolicyLimit) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*limit = Unlimited
		return nil
	}
	var value int64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value < 0 {
		return errors.New("SecondBox policy limit must be nonnegative or null")
	}
	*limit = PolicyLimit(value)
	return nil
}
func (limit PolicyLimit) Value() (driver.Value, error) {
	if limit.IsUnlimited() {
		return nil, nil
	}
	if limit < 0 {
		return nil, errors.New("SecondBox policy limit is invalid")
	}
	return int64(limit), nil
}
func (limit *PolicyLimit) Scan(value any) error {
	if value == nil {
		*limit = Unlimited
		return nil
	}
	number, ok := value.(int64)
	if !ok || number < 0 {
		return fmt.Errorf("SecondBox policy limit cannot decode %T", value)
	}
	*limit = PolicyLimit(number)
	return nil
}

func decodeCompletePolicy(data []byte, target any, fields ...string) error {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(data, &present); err != nil {
		return err
	}
	for _, field := range fields {
		if _, ok := present[field]; !ok {
			return fmt.Errorf("SecondBox policy field %s is required", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("SecondBox policy must contain one object")
	}
	return nil
}

func (quota *QuotaLimits) UnmarshalJSON(data []byte) error {
	type wire QuotaLimits
	var decoded wire
	if err := decodeCompletePolicy(data, &decoded, "maxSandboxes", "maxActiveInstances", "maxVcpuCount", "maxMemoryBytes", "maxSnapshots", "maxPortSessions", "maxConcurrentOperations"); err != nil {
		return err
	}
	*quota = QuotaLimits(decoded)
	return nil
}
func (quota *TenantQuota) UnmarshalJSON(data []byte) error {
	type wire TenantQuota
	var decoded wire
	if err := decodeCompletePolicy(data, &decoded, "maxSandboxes", "maxActiveInstances", "maxVcpuCount", "maxMemoryBytes", "maxSnapshots", "maxPortSessions", "maxConcurrentOperations", "maxActiveSubjects", "maxApplicationAuthorities"); err != nil {
		return err
	}
	*quota = TenantQuota(decoded)
	return nil
}
func (policy *LifecyclePolicy) UnmarshalJSON(data []byte) error {
	type wire LifecyclePolicy
	var decoded wire
	if err := decodeCompletePolicy(data, &decoded, "initialState", "drainGraceSeconds", "idleSeconds", "maximumDurationSeconds", "leaseSeconds"); err != nil {
		return err
	}
	*policy = LifecyclePolicy(decoded)
	return nil
}

type PositivePolicyLimit = PolicyLimit
