package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

// GarbageCatalog publishes only objects that have passed a durable reachability grace.
type GarbageCatalog interface {
	ListGarbageObjectsDue(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]ports.GarbageObject, error)
	CompleteGarbageObject(ctx context.Context, object ports.GarbageObject, now time.Time) error
}

// GarbageCollector removes unreachable immutable bytes and records terminal evidence.
type GarbageCollector struct {
	Catalog   GarbageCatalog
	Objects   objectstore.Store
	Grace     time.Duration
	BatchSize int
}

// Sweep performs one bounded mark/recheck/delete batch.
func (collector GarbageCollector) Sweep(ctx context.Context, now time.Time) (int, error) {
	if collector.Catalog == nil || collector.Objects == nil || collector.Grace <= 0 || collector.BatchSize < 1 {
		return 0, errors.New("SecondBox garbage collector dependencies and bounds are required")
	}
	candidates, err := collector.Catalog.ListGarbageObjectsDue(ctx, now.UTC(), collector.Grace, collector.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("SecondBox garbage candidate listing failed: %w", err)
	}
	completed := 0
	for _, candidate := range candidates {
		if err := collector.Objects.Delete(ctx, candidate.StorageKey); err != nil {
			return completed, err
		}
		if err := collector.Catalog.CompleteGarbageObject(ctx, candidate, now.UTC()); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
