package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
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

func (apiHandler *handler) tenantManagementAction(writer http.ResponseWriter, request *http.Request) {
	action := request.PathValue("tenantAction")
	if !strings.HasSuffix(action, ":suspend") && !strings.HasSuffix(action, ":reactivate") {
		http.NotFound(writer, request)
		return
	}
	apiHandler.managementUnavailable(writer, request)
}

func (apiHandler *handler) tenantControllerAuthorityManagementAction(writer http.ResponseWriter, request *http.Request) {
	action := request.PathValue("authorityAction")
	if !strings.HasSuffix(action, ":rotate") && !strings.HasSuffix(action, ":revoke") {
		http.NotFound(writer, request)
		return
	}
	apiHandler.managementUnavailable(writer, request)
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
	action := request.PathValue("authorityAction")
	if !strings.HasSuffix(action, ":rotate") && !strings.HasSuffix(action, ":revoke") {
		http.NotFound(writer, request)
		return
	}
	apiHandler.managementUnavailable(writer, request)
}
