package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestS3StorePublishesAndVerifiesImmutableObject(t *testing.T) {
	var mutex sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		switch request.Method {
		case http.MethodPut:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			if _, exists := objects[request.URL.Path]; exists {
				writer.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			objects[request.URL.Path] = body
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodHead:
			body, exists := objects[request.URL.Path]
			if !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			sum := sha256.Sum256(body)
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
			writer.Header().Set("X-Amz-Meta-Sha256", hex.EncodeToString(sum[:]))
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, exists := objects[request.URL.Path]
			if !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = writer.Write(body)
		case http.MethodDelete:
			delete(objects, request.URL.Path)
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	store, err := NewS3Store(t.Context(), S3Config{
		Endpoint: server.URL, Region: "us-east-1", Bucket: "secondbox",
		AccessKeyID: "test-access", SecretAccessKey: "test-secret", UsePathStyle: true,
		RetryMaxAttempts: 1, HTTPTimeout: time.Second, TempDirectory: t.TempDir(),
		MaxObjectBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("immutable-application-artifact")
	sum := sha256.Sum256(content)
	evidence, err := store.PutImmutable(
		t.Context(), "artifacts/artifact-1", bytes.NewReader(content),
		int64(len(content)), hex.EncodeToString(sum[:]),
	)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SizeBytes != int64(len(content)) || evidence.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("publication evidence = %#v", evidence)
	}
	body, verified, err := store.GetVerified(t.Context(), "artifacts/artifact-1", evidence)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil || !bytes.Equal(got, content) || verified.SHA256 != evidence.SHA256 {
		t.Fatalf("verified content = %q evidence=%#v err=%v", got, verified, err)
	}
	if closeErr != nil {
		t.Fatalf("verified staging cleanup failed: %v", closeErr)
	}
}

func TestS3StoreRejectsHashAndSizeMismatchBeforePublication(t *testing.T) {
	store, err := NewS3Store(t.Context(), S3Config{
		Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "secondbox",
		AccessKeyID: "test-access", SecretAccessKey: "test-secret", UsePathStyle: true,
		RetryMaxAttempts: 1, HTTPTimeout: time.Second, TempDirectory: t.TempDir(),
		MaxObjectBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutImmutable(t.Context(), "bad", bytes.NewReader([]byte("bytes")), 4, string(make([]byte, 64))); err == nil {
		t.Fatal("size/hash mismatch was accepted")
	}
}
