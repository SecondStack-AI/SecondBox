package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPGatewayIssuer struct {
	BaseURL string
	Client  *http.Client
}

func (issuer HTTPGatewayIssuer) Issue(ctx context.Context, id, csr string, lifetime time.Duration) (string, string, error) {
	var result struct {
		Certificate string `json:"certificatePem"`
		CA          string `json:"certificateAuthorityPem"`
	}
	err := issuer.call(ctx, id, "issue", map[string]any{"csrPem": csr, "lifetimeSeconds": int64(lifetime / time.Second)}, &result)
	return result.Certificate, result.CA, err
}

func (issuer HTTPGatewayIssuer) Retire(ctx context.Context, id string) error {
	return issuer.call(ctx, id, "retire", nil, nil)
}

func (issuer HTTPGatewayIssuer) call(ctx context.Context, id, action string, payload, result any) error {
	if !ValidEnrollmentEndpoint(issuer.BaseURL) || issuer.Client == nil {
		return errors.New("SecondBox fleet gateway issuer HTTPS endpoint and mTLS client are required")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(issuer.BaseURL, "/")+"/v1/runner-gateways/"+url.PathEscape(id)+":"+action, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := issuer.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("SecondBox fleet gateway %s refused: HTTP %d", action, response.StatusCode)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(result)
}
