package dedup

import (
	"context"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/sync/singleflight"
)

// Deduplicator deduplicates concurrent calls for the same key and caches results.
type Deduplicator[T any] struct {
	group singleflight.Group
	cache *ttlcache.Cache[string, T]
	ttl   time.Duration
}

// NewDeduplicator creates a Deduplicator with the given result TTL.
func NewDeduplicator[T any](ttl time.Duration) *Deduplicator[T] {
	c := ttlcache.New[string, T](
		ttlcache.WithTTL[string, T](ttl),
	)
	go c.Start()
	return &Deduplicator[T]{cache: c, ttl: ttl}
}

// Do executes fn for the given key, deduplicating concurrent calls and caching the result.
// The computation uses context.WithoutCancel so that the result is cached even if the
// original caller disconnects — matching the original Node.js behavior where the Promise
// runs to completion regardless of caller lifecycle.
func (d *Deduplicator[T]) Do(ctx context.Context, key string, fn func(ctx context.Context) (T, error)) (T, error) {
	if item := d.cache.Get(key); item != nil {
		return item.Value(), nil
	}

	v, err, _ := d.group.Do(key, func() (any, error) {
		detached := context.WithoutCancel(ctx)
		result, err := fn(detached)
		if err != nil {
			return nil, err
		}
		d.cache.Set(key, result, d.ttl)
		return result, nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil
}

// Get returns a cached result for the given key without executing any function.
func (d *Deduplicator[T]) Get(key string) (T, bool) {
	item := d.cache.Get(key)
	if item == nil {
		var zero T
		return zero, false
	}
	return item.Value(), true
}

// Invalidate removes a cached result for the given key.
func (d *Deduplicator[T]) Invalidate(key string) {
	d.cache.Delete(key)
}
