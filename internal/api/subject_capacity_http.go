package api

import "net/http"

func (apiHandler *handler) getSubjectCapacity(writer http.ResponseWriter, request *http.Request) {
	usage, err := apiHandler.service.GetSubjectCapacity(request.Context(), requestPrincipal(request))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, usage)
}
