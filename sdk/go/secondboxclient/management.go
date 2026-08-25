package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

func (client *Client) CreateTenant(ctx context.Context, request CreateTenantRequest, idempotencyKey string) (Tenant, error) {
	var tenant Tenant
	err := client.mutateManagementJSON(ctx, "createTenant", nil, 0, idempotencyKey, request, &tenant)
	return tenant, err
}

func (client *Client) GetTenant(ctx context.Context, tenantRef OwnershipRef) (Tenant, error) {
	var tenant Tenant
	err := client.RequestJSON(ctx, "getTenant", CallOptions{PathParameters: map[string]string{"tenantRef": tenantRef}}, &tenant)
	return tenant, err
}

func (client *Client) ListTenants(ctx context.Context, options PageOptions) (TenantPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return TenantPage{}, err
	}
	var page TenantPage
	err = client.RequestJSON(ctx, "listTenants", CallOptions{QueryParameters: query}, &page)
	return page, err
}

func (client *Client) SuspendTenant(ctx context.Context, tenantRef OwnershipRef, expectedRevision int64, idempotencyKey string) (Tenant, error) {
	return client.mutateTenantLifecycle(ctx, "suspendTenant", tenantRef, expectedRevision, idempotencyKey)
}

func (client *Client) ReactivateTenant(ctx context.Context, tenantRef OwnershipRef, expectedRevision int64, idempotencyKey string) (Tenant, error) {
	return client.mutateTenantLifecycle(ctx, "reactivateTenant", tenantRef, expectedRevision, idempotencyKey)
}

func (client *Client) mutateTenantLifecycle(ctx context.Context, operationID string, tenantRef OwnershipRef, expectedRevision int64, idempotencyKey string) (Tenant, error) {
	var tenant Tenant
	err := client.mutateManagementJSON(ctx, operationID, map[string]string{"tenantRef": tenantRef}, expectedRevision, idempotencyKey, nil, &tenant)
	return tenant, err
}

func (client *Client) CreateTenantControllerAuthority(ctx context.Context, tenantRef OwnershipRef, request CreateTenantControllerAuthorityRequest, idempotencyKey string) (TenantControllerCredentialResponse, error) {
	var response TenantControllerCredentialResponse
	err := client.mutateManagementJSON(ctx, "createTenantControllerAuthority", map[string]string{"tenantRef": tenantRef}, 0, idempotencyKey, request, &response)
	return response, err
}

func (client *Client) GetTenantControllerAuthority(ctx context.Context, tenantRef OwnershipRef, authorityID AuthorityID) (TenantControllerAuthority, error) {
	var authority TenantControllerAuthority
	err := client.RequestJSON(ctx, "getTenantControllerAuthority", CallOptions{PathParameters: map[string]string{"tenantRef": tenantRef, "authorityId": authorityID}}, &authority)
	return authority, err
}

func (client *Client) ListTenantControllerAuthorities(ctx context.Context, tenantRef OwnershipRef, options PageOptions) (TenantControllerAuthorityPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return TenantControllerAuthorityPage{}, err
	}
	var page TenantControllerAuthorityPage
	err = client.RequestJSON(ctx, "listTenantControllerAuthorities", CallOptions{PathParameters: map[string]string{"tenantRef": tenantRef}, QueryParameters: query}, &page)
	return page, err
}

func (client *Client) RotateTenantControllerAuthority(ctx context.Context, tenantRef OwnershipRef, authorityID AuthorityID, expectedRevision int64, idempotencyKey string) (TenantControllerCredentialResponse, error) {
	var response TenantControllerCredentialResponse
	err := client.mutateManagementJSON(ctx, "rotateTenantControllerAuthority", map[string]string{"tenantRef": tenantRef, "authorityId": authorityID}, expectedRevision, idempotencyKey, nil, &response)
	return response, err
}

func (client *Client) RevokeTenantControllerAuthority(ctx context.Context, tenantRef OwnershipRef, authorityID AuthorityID, expectedRevision int64, idempotencyKey string) (TenantControllerAuthority, error) {
	var authority TenantControllerAuthority
	err := client.mutateManagementJSON(ctx, "revokeTenantControllerAuthority", map[string]string{"tenantRef": tenantRef, "authorityId": authorityID}, expectedRevision, idempotencyKey, nil, &authority)
	return authority, err
}

func (client *Client) CreateSubject(ctx context.Context, request CreateSubjectRequest, idempotencyKey string) (Subject, error) {
	var subject Subject
	err := client.mutateManagementJSON(ctx, "createSubject", nil, 0, idempotencyKey, request, &subject)
	return subject, err
}

func (client *Client) GetSubject(ctx context.Context, subjectRef OwnershipRef) (Subject, error) {
	var subject Subject
	err := client.RequestJSON(ctx, "getSubject", CallOptions{PathParameters: map[string]string{"subjectRef": subjectRef}}, &subject)
	return subject, err
}

func (client *Client) ListSubjects(ctx context.Context, options PageOptions) (SubjectPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return SubjectPage{}, err
	}
	var page SubjectPage
	err = client.RequestJSON(ctx, "listSubjects", CallOptions{QueryParameters: query}, &page)
	return page, err
}

func (client *Client) CreateApplicationAuthority(ctx context.Context, request CreateApplicationAuthorityRequest, idempotencyKey string) (ApplicationCredentialResponse, error) {
	var response ApplicationCredentialResponse
	err := client.mutateManagementJSON(ctx, "createApplicationAuthority", nil, 0, idempotencyKey, request, &response)
	return response, err
}

func (client *Client) GetApplicationAuthority(ctx context.Context, authorityID AuthorityID) (ApplicationAuthority, error) {
	var authority ApplicationAuthority
	err := client.RequestJSON(ctx, "getApplicationAuthority", CallOptions{PathParameters: map[string]string{"authorityId": authorityID}}, &authority)
	return authority, err
}

func (client *Client) ListApplicationAuthorities(ctx context.Context, subjectRef OwnershipRef, options PageOptions) (ApplicationAuthorityPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return ApplicationAuthorityPage{}, err
	}
	if subjectRef != "" {
		query.Set("subjectRef", subjectRef)
	}
	var page ApplicationAuthorityPage
	err = client.RequestJSON(ctx, "listApplicationAuthorities", CallOptions{QueryParameters: query}, &page)
	return page, err
}

func (client *Client) RotateApplicationAuthority(ctx context.Context, authorityID AuthorityID, expectedRevision int64, idempotencyKey string) (ApplicationCredentialResponse, error) {
	var response ApplicationCredentialResponse
	err := client.mutateManagementJSON(ctx, "rotateApplicationAuthority", map[string]string{"authorityId": authorityID}, expectedRevision, idempotencyKey, nil, &response)
	return response, err
}

func (client *Client) RevokeApplicationAuthority(ctx context.Context, authorityID AuthorityID, expectedRevision int64, idempotencyKey string) (ApplicationAuthority, error) {
	var authority ApplicationAuthority
	err := client.mutateManagementJSON(ctx, "revokeApplicationAuthority", map[string]string{"authorityId": authorityID}, expectedRevision, idempotencyKey, nil, &authority)
	return authority, err
}

func (client *Client) GetTenantUsage(ctx context.Context) (TenantUsage, error) {
	var usage TenantUsage
	err := client.RequestJSON(ctx, "getTenantUsage", CallOptions{}, &usage)
	return usage, err
}

func (client *Client) mutateManagementJSON(ctx context.Context, operationID string, path map[string]string, expectedRevision int64, idempotencyKey string, request any, target any) error {
	if idempotencyKey == "" {
		return errors.New("SecondBox management Idempotency-Key is required")
	}
	resolved, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", resolved)
	if expectedRevision != 0 {
		if expectedRevision < 1 {
			return errors.New("SecondBox management expected revision must be positive")
		}
		headers.Set("If-Match", RevisionETag(expectedRevision))
	}
	options := CallOptions{PathParameters: path, Headers: headers}
	if request != nil {
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		options.Body = bytes.NewReader(body)
		options.ContentType = "application/json"
	}
	return client.RequestJSON(ctx, operationID, options, target)
}
