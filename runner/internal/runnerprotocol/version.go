package runnerv1

// SupportedProtocolMinimum and SupportedProtocolMaximum mirror the independently
// built control-plane protocol window. The generation verifier rejects drift.
const (
	SupportedProtocolMinimum uint32 = 8
	SupportedProtocolMaximum uint32 = 8
)
