package cache

import (
	"strings"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

const defaultTTL = 30 * time.Minute

// KVCache is a TTL-based key-value cache.
type KVCache struct {
	cache *ttlcache.Cache[string, string]
}

// NewKVCache creates a new KVCache with a 30-minute TTL.
func NewKVCache() *KVCache {
	c := ttlcache.New[string, string](
		ttlcache.WithTTL[string, string](defaultTTL),
	)
	go c.Start()
	return &KVCache{cache: c}
}

// Get retrieves a value by key.
func (kv *KVCache) Get(key string) (string, bool) {
	item := kv.cache.Get(key)
	if item == nil {
		return "", false
	}
	return item.Value(), true
}

// Set stores a key-value pair.
func (kv *KVCache) Set(key, value string) {
	kv.cache.Set(key, value, defaultTTL)
}

// Delete removes a key from the cache.
func (kv *KVCache) Delete(key string) {
	kv.cache.Delete(key)
}

// RangeByPrefix returns all entries whose keys start with the given prefix.
func (kv *KVCache) RangeByPrefix(prefix string) map[string]string {
	result := make(map[string]string)
	kv.cache.Range(func(item *ttlcache.Item[string, string]) bool {
		if strings.HasPrefix(item.Key(), prefix) {
			result[item.Key()] = item.Value()
		}
		return true
	})
	return result
}

// Stop terminates the cache's background eviction goroutine.
func (kv *KVCache) Stop() {
	kv.cache.Stop()
}
