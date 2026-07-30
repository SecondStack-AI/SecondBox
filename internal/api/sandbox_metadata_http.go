package api

import (
	"net/http"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// updateSandboxMetadata replaces application correlation metadata under If-Match.
func (apiHandler *handler) updateSandboxMetadata(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var body contracts.UpdateSandboxMetadataRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	sandbox, err := apiHandler.service.UpdateSandboxMetadata(
		request.Context(),
		requestPrincipal(request),
		request.PathValue("sandboxID"),
		expectedRevision,
		body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, sandbox.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, sandbox)
}
