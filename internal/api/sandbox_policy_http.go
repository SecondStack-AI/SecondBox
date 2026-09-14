package api

import (
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"net/http"
	"strconv"
)

func (apiHandler *handler) getSubjectSandboxPolicy(writer http.ResponseWriter, request *http.Request) {
	result, err := apiHandler.service.GetSubjectSandboxPolicy(request.Context(), requestPrincipal(request), request.PathValue("subjectRef"), request.URL.Query().Get("profile"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, result.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}
func (apiHandler *handler) updateSubjectSandboxPolicy(writer http.ResponseWriter, request *http.Request) {
	var body contracts.SubjectSandboxPolicy
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	revision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, replayed, err := apiHandler.service.UpdateSubjectSandboxPolicy(request.Context(), requestPrincipal(request), request.PathValue("subjectRef"), request.Header.Get("Idempotency-Key"), revision, body)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, result.Revision)
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}

func (apiHandler *handler) getApplicationSandboxPolicy(writer http.ResponseWriter, request *http.Request) {
	profile := request.URL.Query().Get("profile")
	if err := authorizeApplicationProfile(request, profile); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, err := apiHandler.service.GetApplicationSandboxPolicy(request.Context(), requestPrincipal(request), profile)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}
