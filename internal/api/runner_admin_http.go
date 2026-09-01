package api

import (
	"net/http"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// Runner administration is authorized by the platform-token HTTP boundary.
func (apiHandler *handler) createRunnerPool(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateRunnerPoolRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	pool, err := apiHandler.service.CreateRunnerPool(
		request.Context(),
		requestPrincipal(request),
		body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, pool.Revision)
	apiHandler.writeJSON(writer, request, http.StatusCreated, pool)
}

func (apiHandler *handler) listRunnerPools(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListRunnerPools(
		request.Context(),
		requestPrincipal(request),
		limit,
		request.URL.Query().Get("cursor"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) getRunnerPool(writer http.ResponseWriter, request *http.Request) {
	pool, err := apiHandler.service.GetRunnerPool(
		request.Context(),
		requestPrincipal(request),
		request.PathValue("runnerPoolName"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, pool.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, pool)
}

func (apiHandler *handler) updateRunnerPool(writer http.ResponseWriter, request *http.Request) {
	var body contracts.UpdateRunnerPoolRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	pool, err := apiHandler.service.UpdateRunnerPool(
		request.Context(),
		requestPrincipal(request),
		request.PathValue("runnerPoolName"),
		body,
		expectedRevision,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, pool.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, pool)
}

func (apiHandler *handler) listRunners(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListRunners(
		request.Context(),
		requestPrincipal(request),
		request.URL.Query().Get("pool"),
		limit,
		request.URL.Query().Get("cursor"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) getRunner(writer http.ResponseWriter, request *http.Request) {
	runner, err := apiHandler.service.GetRunner(
		request.Context(),
		requestPrincipal(request),
		request.PathValue("runnerID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, runner.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, runner)
}

func (apiHandler *handler) readEgressContextPreflight(writer http.ResponseWriter, request *http.Request) {
	preflight, err := apiHandler.service.ReadEgressContextPreflight(request.Context(), requestPrincipal(request))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, preflight)
}
