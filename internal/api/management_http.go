package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (apiHandler *handler) authenticatePlatformManagement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		credential, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || credential == "" {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		presentedHash := sha256.Sum256([]byte(credential))
		if subtle.ConstantTimeCompare(presentedHash[:], apiHandler.platformTokenHash[:]) != 1 {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		principal := contracts.Principal{Kind: "platform", ID: "platform"}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)))
	})
}

func (apiHandler *handler) authenticateTenantControllerManagement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		credential, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || credential == "" || !isTenantControllerBearerToken(credential) ||
			apiHandler.persistedAuthorities == nil {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		if request.Header.Get("X-SecondBox-Tenant-Ref") != "" ||
			request.Header.Get("X-SecondBox-Subject-Ref") != "" {
			apiHandler.writeError(writer, request, ports.ErrAuthorizationDenied)
			return
		}
		principal, err := apiHandler.persistedAuthorities.AuthenticateTenantControllerAuthority(
			request.Context(), credential, time.Now().UTC(),
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)))
	})
}

func (apiHandler *handler) managementUnavailable(writer http.ResponseWriter, request *http.Request) {
	apiHandler.writeError(writer, request, ports.ErrManagementUnavailable)
}

func (apiHandler *handler) createTenant(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateTenantRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	tenant, replayed, err := apiHandler.service.CreateTenant(request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, tenant.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusCreated, tenant)
}

func (apiHandler *handler) getTenant(writer http.ResponseWriter, request *http.Request) {
	tenant, err := apiHandler.service.GetTenant(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, tenant.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, tenant)
}

func (apiHandler *handler) listTenants(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListTenants(request.Context(), requestPrincipal(request), limit, request.URL.Query().Get("cursor"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) tenantManagementAction(writer http.ResponseWriter, request *http.Request) {
	tenantRef, action, ok := splitAction(request.PathValue("tenantAction"))
	if !ok || action != "suspend" && action != "reactivate" {
		http.NotFound(writer, request)
		return
	}
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	var tenant contracts.Tenant
	var replayed bool
	if action == "suspend" {
		tenant, replayed, err = apiHandler.service.SuspendTenant(request.Context(), requestPrincipal(request), tenantRef, request.Header.Get("Idempotency-Key"), expectedRevision)
	} else {
		tenant, replayed, err = apiHandler.service.ReactivateTenant(request.Context(), requestPrincipal(request), tenantRef, request.Header.Get("Idempotency-Key"), expectedRevision)
	}
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, tenant.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, tenant)
}

func (apiHandler *handler) createTenantControllerAuthority(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateTenantControllerAuthorityRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, replayed, err := apiHandler.service.CreateTenantControllerAuthority(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), request.Header.Get("Idempotency-Key"), body)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, response.Authority.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusCreated, response)
}

func (apiHandler *handler) getTenantControllerAuthority(writer http.ResponseWriter, request *http.Request) {
	authority, err := apiHandler.service.GetTenantControllerAuthority(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), request.PathValue("authorityID"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, authority.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, authority)
}

func (apiHandler *handler) listTenantControllerAuthorities(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListTenantControllerAuthorities(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), limit, request.URL.Query().Get("cursor"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) tenantControllerAuthorityManagementAction(writer http.ResponseWriter, request *http.Request) {
	authorityID, action, ok := splitAction(request.PathValue("authorityAction"))
	if !ok || action != "rotate" && action != "revoke" {
		http.NotFound(writer, request)
		return
	}
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if action == "rotate" {
		response, replayed, err := apiHandler.service.RotateTenantControllerAuthority(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), authorityID, request.Header.Get("Idempotency-Key"), expectedRevision)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		setRevisionETag(writer, response.Authority.Revision)
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		apiHandler.writeJSON(writer, request, http.StatusOK, response)
		return
	}
	authority, replayed, err := apiHandler.service.RevokeTenantControllerAuthority(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), authorityID, request.Header.Get("Idempotency-Key"), expectedRevision)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, authority.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, authority)
}

func (apiHandler *handler) createSubject(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateSubjectRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	subject, replayed, err := apiHandler.service.CreateSubject(request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, subject.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusCreated, subject)
}

func (apiHandler *handler) getSubject(writer http.ResponseWriter, request *http.Request) {
	subject, err := apiHandler.service.GetSubject(request.Context(), requestPrincipal(request), request.PathValue("subjectRef"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, subject.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, subject)
}

func (apiHandler *handler) listSubjects(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListSubjects(request.Context(), requestPrincipal(request), limit, request.URL.Query().Get("cursor"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) subjectManagementAction(writer http.ResponseWriter, request *http.Request) {
	action := request.PathValue("subjectAction")
	if !strings.HasSuffix(action, ":close") && !strings.HasSuffix(action, ":cleanup") {
		http.NotFound(writer, request)
		return
	}
	apiHandler.managementUnavailable(writer, request)
}

func (apiHandler *handler) applicationAuthorityManagementAction(writer http.ResponseWriter, request *http.Request) {
	authorityID, action, ok := splitAction(request.PathValue("authorityAction"))
	if !ok || action != "rotate" && action != "revoke" {
		http.NotFound(writer, request)
		return
	}
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if action == "rotate" {
		response, replayed, err := apiHandler.service.RotateApplicationAuthority(request.Context(), requestPrincipal(request), authorityID, request.Header.Get("Idempotency-Key"), expectedRevision)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		setRevisionETag(writer, response.Authority.Revision)
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		apiHandler.writeJSON(writer, request, http.StatusOK, response)
		return
	}
	authority, replayed, err := apiHandler.service.RevokeApplicationAuthority(request.Context(), requestPrincipal(request), authorityID, request.Header.Get("Idempotency-Key"), expectedRevision)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, authority.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, authority)
}

func (apiHandler *handler) createApplicationAuthority(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateApplicationAuthorityRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, replayed, err := apiHandler.service.CreateApplicationAuthority(request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, response.Authority.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusCreated, response)
}

func (apiHandler *handler) getApplicationAuthority(writer http.ResponseWriter, request *http.Request) {
	authority, err := apiHandler.service.GetApplicationAuthority(request.Context(), requestPrincipal(request), request.PathValue("authorityID"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, authority.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, authority)
}

func (apiHandler *handler) listApplicationAuthorities(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListApplicationAuthorities(request.Context(), requestPrincipal(request), request.URL.Query().Get("subjectRef"), limit, request.URL.Query().Get("cursor"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}
