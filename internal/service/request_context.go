package service

import (
	"context"
	"strings"
)

type requestIDContextKey struct{}

type applicationProfileGrantsContextKey struct{}

// ContextWithRequestID binds one validated transport correlation identifier to service work.
func ContextWithRequestID(ctx context.Context, requestID string) context.Context {
	if strings.TrimSpace(requestID) == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

func (service *ControlPlaneService) requestID(ctx context.Context) string {
	if requestID, ok := ctx.Value(requestIDContextKey{}).(string); ok && requestID != "" {
		return requestID
	}
	return service.newID("req")
}

// ContextWithApplicationProfileGrants binds an authenticated application
// authority's Profile grants to service work. Admissions evaluate them against
// the Sandbox's Profile under their own row locks, so a concurrent Profile
// switch cannot be admitted on the strength of an earlier read.
func ContextWithApplicationProfileGrants(ctx context.Context, grants []string) context.Context {
	return context.WithValue(ctx, applicationProfileGrantsContextKey{}, append([]string{}, grants...))
}

// applicationProfileGrants returns nil for a caller without an application
// authority and otherwise a non-nil, possibly empty, grant list.
func applicationProfileGrants(ctx context.Context) []string {
	grants, _ := ctx.Value(applicationProfileGrantsContextKey{}).([]string)
	return grants
}
