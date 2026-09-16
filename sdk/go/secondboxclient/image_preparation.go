package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

func (client *Client) PrepareImage(ctx context.Context, request PrepareImageRequest, idempotencyKey string) (Operation, error) {
	key, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return Operation{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Operation{}, err
	}
	var operation Operation
	err = client.RequestJSON(ctx, "prepareImage", CallOptions{Headers: http.Header{"Idempotency-Key": []string{key}}, ContentType: "application/json", Body: bytes.NewReader(body)}, &operation)
	return operation, err
}
