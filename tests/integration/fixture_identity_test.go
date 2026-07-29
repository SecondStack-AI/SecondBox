package integration_test

import "time"

// The standalone control plane no longer models Projects, ServiceAccounts, or
// APIKeys: ownership is one platform token plus caller-asserted tenant and
// subject references. These shapes exist only so the integration suite can keep
// expressing "some tenant, some subject, some credential" in the vocabulary its
// fixtures were written in. They are test scaffolding and deliberately do not
// live in pkg/contracts.

const (
	fixtureProjectStateActive        = "active"
	fixtureServiceAccountStateActive = "active"
	fixtureAPIKeyStateActive         = "active"
)

// fixtureProject stands in for one tenant reference.
type fixtureProject struct {
	ID        string
	Name      string
	State     string
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// fixtureServiceAccount stands in for one subject reference and its grants.
type fixtureServiceAccount struct {
	ID            string
	TenantRef     string
	Name          string
	State         string
	Scopes        []string
	ProfileGrants []string
	Revision      int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// fixtureAPIKey stands in for non-secret credential metadata.
type fixtureAPIKey struct {
	ID         string
	SubjectRef string
	Name       string
	Prefix     string
	State      string
	Scopes     []string
	Revision   int64
	CreatedAt  time.Time
}

type fixtureCreateProjectRequest struct {
	Name string
}

type fixtureCreateServiceAccountRequest struct {
	Name          string
	Scopes        []string
	ProfileGrants []string
}

type fixtureUpdateServiceAccountRequest struct {
	Name          *string
	Scopes        *[]string
	ProfileGrants *[]string
}

type fixtureCreateAPIKeyRequest struct {
	Name   string
	Scopes []string
}

type fixtureCreateAPIKeyResponse struct {
	APIKey     fixtureAPIKey
	Credential string
}
