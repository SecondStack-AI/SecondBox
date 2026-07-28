package lifecycle

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestGarbageCollectorDeletesBeforeRecordingCompletion(t *testing.T) {
	catalog := &fakeGarbageCatalog{objects: []ports.GarbageObject{{
		Kind: "checkpoint", ID: "chk-expired", StorageKey: "checkpoints/chk-expired",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 4,
	}}}
	objects := &fakeObjectStore{}
	collector := GarbageCollector{Catalog: catalog, Objects: objects, Grace: time.Minute, BatchSize: 10}
	count, err := collector.Sweep(t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	if err != nil || count != 1 {
		t.Fatalf("sweep = %d, %v", count, err)
	}
	if len(objects.deleted) != 1 || len(catalog.completed) != 1 ||
		objects.deleted[0] != catalog.completed[0].StorageKey {
		t.Fatalf("delete/completion evidence = %#v / %#v", objects.deleted, catalog.completed)
	}
}

type fakeGarbageCatalog struct {
	objects   []ports.GarbageObject
	completed []ports.GarbageObject
}

func (catalog *fakeGarbageCatalog) ListGarbageObjectsDue(
	context.Context,
	time.Time,
	time.Duration,
	int,
) ([]ports.GarbageObject, error) {
	return catalog.objects, nil
}

func (catalog *fakeGarbageCatalog) CompleteGarbageObject(
	_ context.Context,
	object ports.GarbageObject,
	_ time.Time,
) error {
	catalog.completed = append(catalog.completed, object)
	return nil
}

type fakeObjectStore struct {
	deleted []string
}

func (*fakeObjectStore) PutImmutable(context.Context, string, io.Reader, int64, string) (objectstore.Evidence, error) {
	return objectstore.Evidence{}, nil
}

func (*fakeObjectStore) HeadVerified(context.Context, string, objectstore.Evidence) (objectstore.Evidence, error) {
	return objectstore.Evidence{}, nil
}

func (*fakeObjectStore) GetVerified(context.Context, string, objectstore.Evidence) (io.ReadCloser, objectstore.Evidence, error) {
	return io.NopCloser(bytes.NewReader(nil)), objectstore.Evidence{}, nil
}

func (store *fakeObjectStore) Delete(_ context.Context, key string) error {
	store.deleted = append(store.deleted, key)
	return nil
}
