// Package pagination defines provider-neutral opaque traversal cursors.
package pagination

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
)

const listCursorVersion = 1

// MaximumListCursorLength is the public wire bound shared by list endpoints.
const MaximumListCursorLength = 512

// ErrInvalidListCursor identifies malformed, mismatched, or stale list traversal authority.
var ErrInvalidListCursor = errors.New("SecondBox list page cursor is invalid")

type listCursorEnvelope struct {
	Version      int    `json:"v"`
	ResourceKind string `json:"r"`
	ScopeDigest  string `json:"s"`
	LastItemKey  string `json:"k"`
}

// EncodeListCursor binds one last-seen resource key to an endpoint and authorization scope.
func EncodeListCursor(resourceKind string, scope string, lastItemKey string) (string, error) {
	if resourceKind == "" || len(resourceKind) > 32 || lastItemKey == "" || len(lastItemKey) > 128 {
		return "", ErrInvalidListCursor
	}
	envelope := listCursorEnvelope{
		Version:      listCursorVersion,
		ResourceKind: resourceKind,
		ScopeDigest:  digestListCursorScope(scope),
		LastItemKey:  lastItemKey,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", ErrInvalidListCursor
	}
	cursor := base64.RawURLEncoding.EncodeToString(encoded)
	if len(cursor) > MaximumListCursorLength {
		return "", ErrInvalidListCursor
	}
	return cursor, nil
}

// DecodeListCursor returns the last-seen key only for its exact endpoint and scope.
func DecodeListCursor(resourceKind string, scope string, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > MaximumListCursorLength {
		return "", ErrInvalidListCursor
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return "", ErrInvalidListCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var envelope listCursorEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return "", ErrInvalidListCursor
	}
	if err := ensureListCursorJSONEnd(decoder); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, decoded) {
		return "", ErrInvalidListCursor
	}
	if envelope.Version != listCursorVersion ||
		envelope.ResourceKind != resourceKind ||
		envelope.ScopeDigest != digestListCursorScope(scope) ||
		envelope.LastItemKey == "" ||
		len(envelope.LastItemKey) > 128 {
		return "", ErrInvalidListCursor
	}
	return envelope.LastItemKey, nil
}

func digestListCursorScope(scope string) string {
	digest := sha256.Sum256([]byte(scope))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func ensureListCursorJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalidListCursor
	}
	return nil
}
