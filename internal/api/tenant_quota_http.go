package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (apiHandler *handler) updateTenantQuota(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		AggregateQuota *contracts.TenantQuota `json:"aggregateQuota"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if body.AggregateQuota == nil {
		apiHandler.writeError(writer, request, requestValidationError(errors.New("SecondBox Tenant aggregateQuota is required")))
		return
	}
	revision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	tenant, replayed, err := apiHandler.service.UpdateTenantQuota(request.Context(), requestPrincipal(request), request.PathValue("tenantRef"), request.Header.Get("Idempotency-Key"), revision, contracts.UpdateTenantQuotaRequest{AggregateQuota: *body.AggregateQuota})
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, tenant.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, tenant)
}
