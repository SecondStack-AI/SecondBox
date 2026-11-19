package api

import (
	"net/http"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func (apiHandler *handler) readRunnerFleet(writer http.ResponseWriter, request *http.Request) {
	fleet, err := apiHandler.service.ReadRunnerFleet(request.Context(), requestPrincipal(request), request.PathValue("runnerPoolName"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, fleet)
}

func (apiHandler *handler) mutateRunnerFleet(writer http.ResponseWriter, request *http.Request) {
	runner, action, ok := splitAction(request.PathValue("runnerAction"), "evacuate", "retire")
	if !ok {
		apiHandler.writeError(writer, request, ports.ErrRunnerNotFound)
		return
	}
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	revision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, err := apiHandler.service.ChangeRunnerFleetState(request.Context(), requestPrincipal(request), request.PathValue("runnerPoolName"), runner, action, revision)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, result.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}
