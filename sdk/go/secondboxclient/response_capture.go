package secondboxclient

import "context"

type responseCaptureKey struct{}
type responseCapture struct {
	operation string
	receive   func([]byte)
}

// CaptureJSONResponse retains the exact successful API representation for a
// presentation client while SDK composition still decodes the typed response.
// The callback runs synchronously for each matching request and owns its bytes.
func CaptureJSONResponse(ctx context.Context, operation string, receive func([]byte)) context.Context {
	return context.WithValue(ctx, responseCaptureKey{}, responseCapture{operation, receive})
}
