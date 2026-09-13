package install

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// DevelopmentTenancyMaximumLifetimeSeconds is the TenantExpiryPolicy contract maximum.
const DevelopmentTenancyMaximumLifetimeSeconds int64 = 31536000

type TenancyOptions struct {
	TenantRef     string
	SubjectRef    string
	EgressContext string
	Application   bool
	Check         bool
}

type TenancyResult struct {
	Evidence map[string]string `json:"evidence"`
	// BearerToken is returned once to the explicit caller, never serialized to a journal.
	BearerToken string `json:"-"`
}

// BootstrapTenancy is an explicit post-start operation. The only platform
// credential source is the accepted operation's protected secret target.
func BootstrapTenancy(ctx context.Context, plan InstallPlan, options TenancyOptions, httpClient *http.Client) (result TenancyResult, resultErr error) {
	if httpClient == nil {
		return result, installerError("tenancy HTTP client is required", nil)
	}
	for name, value := range map[string]string{"tenant reference": options.TenantRef, "subject reference": options.SubjectRef, "egress context": options.EgressContext} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return result, installerError("tenancy "+name+" is required and must be single-line", nil)
		}
	}
	tokenPath := ""
	for _, target := range plan.SecretTargets {
		if target.Category == "platform-authority" {
			tokenPath = target.Path
			break
		}
	}
	token, err := readInstallerSecret(tokenPath, plan.HostFacts.InvokingUID)
	if err != nil {
		return result, err
	}
	endpoint := "http://" + plan.Network.APIAddress
	platform, err := secondboxclient.NewSecondBoxClient(endpoint, token, httpClient)
	if err != nil {
		return result, err
	}
	result.Evidence = map[string]string{"tenantRef": options.TenantRef, "subjectRef": options.SubjectRef, "egressContext": options.EgressContext}
	if options.Check {
		if _, err := platform.ListTenants(ctx, secondboxclient.PageOptions{Limit: 1}); err != nil {
			return result, installerError("tenancy check platform authority", err)
		}
		result.Evidence["check"] = "prerequisites verified; no changes"
		return result, nil
	}
	profiles := standardresources.BundleNames()
	scopes := []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports", "sandbox:ports:direct"}
	metadata := map[string]string{"bootstrap": "development-post-start"}
	tenant, err := platform.GetTenant(ctx, options.TenantRef)
	if tenancyNotFound(err) {
		tenant, err = platform.CreateTenant(ctx, secondboxclient.CreateTenantRequest{Ref: options.TenantRef, EgressContext: &options.EgressContext, AllowedProfileGrants: profiles, AllowedApplicationScopes: scopes, AggregateQuota: secondboxclient.TenantQuota{MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 20, MaxMemoryBytes: 20 << 30, MaxSnapshots: 20, MaxPortSessions: 20, MaxConcurrentOperations: 20, MaxActiveSubjects: 10, MaxApplicationAuthorities: 20}, ExpiryPolicy: secondboxclient.TenantExpiryPolicy{MaximumSubjectLifetimeSeconds: DevelopmentTenancyMaximumLifetimeSeconds, MaximumAuthorityLifetimeSeconds: DevelopmentTenancyMaximumLifetimeSeconds}, Metadata: metadata}, "installer-tenant-"+Digest([]byte(options.TenantRef)))
		result.Evidence["tenant"] = "created"
	} else {
		result.Evidence["tenant"] = "existing; unchanged"
	}
	if err != nil {
		return result, installerError("tenancy create or read Tenant", err)
	}
	// A short-lived controller is sufficient for the bootstrap. Subject lifetime
	// is unbounded unless explicitly requested, and application authority uses
	// the existing Tenant's ceiling (the contract maximum for a new Tenant).
	attempt, err := NewOperationID()
	if err != nil {
		return result, err
	}
	controllerExpiry := time.Now().UTC().Add(time.Duration(min(int64(600), tenant.ExpiryPolicy.MaximumAuthorityLifetimeSeconds)) * time.Second)
	if tenant.ExpiresAt != nil && tenant.ExpiresAt.Before(controllerExpiry) {
		controllerExpiry = *tenant.ExpiresAt
	}
	controller, err := platform.CreateTenantControllerAuthority(ctx, options.TenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{ExpiresAt: controllerExpiry, Metadata: metadata}, attempt+"-controller")
	if err != nil {
		return result, installerError("tenancy create transient controller", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, err := platform.RevokeTenantControllerAuthority(cleanup, options.TenantRef, controller.Authority.ID, controller.Authority.Revision, attempt+"-revoke")
		if err != nil {
			result.Evidence["controller"] = "revocation failed"
			resultErr = errors.Join(resultErr, installerError("tenancy revoke transient controller", err))
		} else {
			result.Evidence["controller"] = "revoked"
		}
	}()
	local, err := secondboxclient.NewSecondBoxTenantControllerClient(endpoint, controller.BearerToken, httpClient)
	if err != nil {
		return result, err
	}
	_, err = local.GetSubject(ctx, options.SubjectRef)
	if tenancyNotFound(err) {
		_, err = local.CreateSubject(ctx, secondboxclient.CreateSubjectRequest{Ref: options.SubjectRef, Quota: secondboxclient.SubjectQuota{MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 20, MaxMemoryBytes: 20 << 30, MaxSnapshots: 20, MaxPortSessions: 20, MaxConcurrentOperations: 20}, Metadata: metadata}, "installer-subject-"+Digest([]byte(options.TenantRef+"\x00"+options.SubjectRef)))
		result.Evidence["subject"] = "created"
	} else {
		result.Evidence["subject"] = "existing; unchanged"
	}
	if err != nil {
		return result, installerError("tenancy create or read Subject", err)
	}
	if options.Application {
		lifetime := min(DevelopmentTenancyMaximumLifetimeSeconds, tenant.ExpiryPolicy.MaximumAuthorityLifetimeSeconds)
		expires := time.Now().UTC().Add(time.Duration(lifetime) * time.Second)
		if tenant.ExpiresAt != nil && tenant.ExpiresAt.Before(expires) {
			expires = *tenant.ExpiresAt
		}
		application, err := local.CreateApplicationAuthority(ctx, secondboxclient.CreateApplicationAuthorityRequest{SubjectRef: options.SubjectRef, Scopes: scopes, ProfileGrants: profiles, ExpiresAt: expires, Metadata: metadata}, attempt+"-application")
		if err != nil {
			return result, installerError("tenancy create application authority", err)
		}
		result.BearerToken = application.BearerToken
		result.Evidence["applicationAuthorityId"] = application.Authority.ID
	}
	return result, nil
}

func tenancyNotFound(err error) bool {
	var failure *secondboxclient.APIError
	return errors.As(err, &failure) && failure.StatusCode == http.StatusNotFound
}
