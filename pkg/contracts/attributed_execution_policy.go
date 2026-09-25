package contracts

import "errors"

// AttributedExecutionConnectionLimits contains no routing or execution authority.
type AttributedExecutionConnectionLimits struct {
	MaximumConnections int64 `json:"maximumConnections"`
}

func (limits AttributedExecutionConnectionLimits) Validate() error {
	if limits.MaximumConnections < 1 || limits.MaximumConnections > 4096 {
		return errors.New("SecondBox attributed execution maximumConnections must be between 1 and 4096")
	}
	return nil
}

func (limits *AttributedExecutionConnectionLimits) UnmarshalJSON(data []byte) error {
	type wire AttributedExecutionConnectionLimits
	var value wire
	if err := decodeCompletePolicy(data, &value, "maximumConnections"); err != nil {
		return err
	}
	*limits = AttributedExecutionConnectionLimits(value)
	return limits.Validate()
}

type AttributedExecutionConnectionObservation struct {
	DefaultMaximumConnections int64 `json:"defaultMaximumConnections"`
	MaximumConnections        int64 `json:"maximumConnections"`
	MaximumConnectionsCeiling int64 `json:"maximumConnectionsCeiling"`
}

// AttributedConnectionGrant returns the numeric grant without exposing gateway
// authority. A missing ceiling restricts selection to this revision's default.
func (spec ProfileRevisionSpec) AttributedConnectionGrant() (*AttributedExecutionConnectionObservation, error) {
	if spec.AttributedExecution == nil {
		if spec.AttributedExecutionCeiling != (AttributedExecutionConnectionLimits{}) {
			return nil, errors.New("SecondBox attributed execution ceiling requires attributed permission")
		}
		return nil, nil
	}
	limit := AttributedExecutionConnectionLimits{MaximumConnections: spec.AttributedExecution.MaximumConnections}
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	ceiling := limit.MaximumConnections
	if spec.AttributedExecutionCeiling != (AttributedExecutionConnectionLimits{}) {
		if err := spec.AttributedExecutionCeiling.Validate(); err != nil {
			return nil, err
		}
		ceiling = spec.AttributedExecutionCeiling.MaximumConnections
		if ceiling < limit.MaximumConnections {
			return nil, errors.New("SecondBox attributed execution default exceeds Profile ceiling")
		}
	}
	return &AttributedExecutionConnectionObservation{
		DefaultMaximumConnections: limit.MaximumConnections,
		MaximumConnections:        limit.MaximumConnections,
		MaximumConnectionsCeiling: ceiling,
	}, nil
}

// Resolve applies the same selection rule to current-head and pinned grants.
// A stored selection survives operator tightening; only the effective value clamps.
func (grant AttributedExecutionConnectionObservation) Resolve(selection *AttributedExecutionConnectionLimits) (AttributedExecutionConnectionObservation, error) {
	grant.MaximumConnections = grant.DefaultMaximumConnections
	if selection != nil {
		if err := selection.Validate(); err != nil {
			return grant, err
		}
		grant.MaximumConnections = min(selection.MaximumConnections, grant.MaximumConnectionsCeiling)
	}
	return grant, nil
}
