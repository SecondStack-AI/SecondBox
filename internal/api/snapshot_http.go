package api

import (
	"net/http"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (apiHandler *handler) listSandboxSnapshots(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListSandboxSnapshots(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		limit, request.URL.Query().Get("cursor"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (apiHandler *handler) createSandboxSnapshot(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateSnapshotRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	snapshot, err := apiHandler.service.CreateSandboxSnapshot(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.Header.Get("Idempotency-Key"), expectedRevision, body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, snapshot)
}

func (apiHandler *handler) getSnapshot(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := apiHandler.service.GetSnapshot(
		request.Context(), requestPrincipal(request), request.PathValue("snapshotID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (apiHandler *handler) deleteSnapshot(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if err := apiHandler.service.DeleteSnapshot(
		request.Context(), requestPrincipal(request), request.PathValue("snapshotID"),
		request.Header.Get("Idempotency-Key"),
	); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
