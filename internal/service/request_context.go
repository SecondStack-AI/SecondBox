package service

import (
	"context"
	"strings"
)

type requestIDContextKey struct{}

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
