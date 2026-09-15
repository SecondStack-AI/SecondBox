package api

import (
	"net/http"
	"strconv"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (apiHandler *handler) prepareImage(writer http.ResponseWriter, request *http.Request) {
	var body contracts.PrepareImageRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if body.Profile != "" {
		if err := authorizeApplicationProfile(request, body.Profile); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
	}
	var grants []string
	if authority, ok := request.Context().Value(applicationAuthorityContextKey{}).(ports.AuthenticatedApplicationAuthority); ok {
		grants = append([]string{}, authority.ProfileGrants...)
	}
	operation, replayed, err := apiHandler.service.PrepareImage(request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body, grants)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusAccepted, operation)
}
