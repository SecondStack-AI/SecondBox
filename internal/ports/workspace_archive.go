package ports

import (
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"time"
)

type WorkspaceArchiveInput struct {
	Principal        contracts.Principal
	AuditEvent       contracts.AuditEvent
	SandboxID        string
	Operation        contracts.Operation
	ArchiveID        string
	CommandID        string
	FencingToken     []byte
	ExpectedRevision int64
	IdempotencyKey   string
	RequestHash      string
	IdempotencyEnds  time.Time
	Now              time.Time
}
