package contracts

import "time"

// SubjectCapacity reveals only the caller's quota and admission headroom.
// Available is capped by both Subject and tenant quota, not a reservation.
type SubjectCapacity struct {
	SubjectRef         string                  `json:"subjectRef"`
	Limits             QuotaLimits             `json:"limits"`
	ConstrainingScopes QuotaConstrainingScopes `json:"constrainingScopes"`
	Usage              QuotaUsage              `json:"usage"`
	Available          QuotaHeadroom           `json:"available"`
	ObservedAt         time.Time               `json:"observedAt"`
}

type QuotaHeadroom struct {
	Sandboxes            PolicyLimit `json:"sandboxes"`
	ActiveInstances      PolicyLimit `json:"activeInstances"`
	VCPUCount            PolicyLimit `json:"vcpuCount"`
	MemoryBytes          PolicyLimit `json:"memoryBytes"`
	Snapshots            PolicyLimit `json:"snapshots"`
	PortSessions         PolicyLimit `json:"portSessions"`
	ConcurrentOperations PolicyLimit `json:"concurrentOperations"`
}

type QuotaConstrainingScopes struct {
	Sandboxes            string `json:"sandboxes"`
	ActiveInstances      string `json:"activeInstances"`
	VCPUCount            string `json:"vcpuCount"`
	MemoryBytes          string `json:"memoryBytes"`
	Snapshots            string `json:"snapshots"`
	PortSessions         string `json:"portSessions"`
	ConcurrentOperations string `json:"concurrentOperations"`
}
