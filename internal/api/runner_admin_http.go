package api

import (
	"net/http"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

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
	writeJSON(writer, http.StatusCreated, pool)
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
	writeJSON(writer, http.StatusOK, page)
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
	writeJSON(writer, http.StatusOK, pool)
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
	writeJSON(writer, http.StatusOK, pool)
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
	writeJSON(writer, http.StatusOK, page)
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
	writeJSON(writer, http.StatusOK, runner)
}
