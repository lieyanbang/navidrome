package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
)

const (
	DefaultCacheEntries = 64
	DefaultCacheValue   = 32 * 1024
	cacheIndexTTL       = int64(30 * 24 * 60 * 60)
)

// CacheBackend is the small part of Navidrome's Cache host service used here.
// Cache keys are already namespaced by Navidrome to the calling plugin.
type CacheBackend interface {
	GetString(key string) (string, bool, error)
	SetString(key, value string, ttlSeconds int64) error
	Remove(key string) error
	GetInt(key string) (int64, bool, error)
	SetInt(key string, value, ttlSeconds int64) error
}

type HostCache struct{}

func (HostCache) GetString(key string) (string, bool, error) {
	return host.CacheGetString(key)
}

func (HostCache) SetString(key, value string, ttlSeconds int64) error {
	return host.CacheSetString(key, value, ttlSeconds)
}

func (HostCache) Remove(key string) error {
	return host.CacheRemove(key)
}

func (HostCache) GetInt(key string) (int64, bool, error) {
	return host.CacheGetInt(key)
}

func (HostCache) SetInt(key string, value, ttlSeconds int64) error {
	return host.CacheSetInt(key, value, ttlSeconds)
}

// BoundedCache applies an entry-count and per-value limit on top of the host's
// TTL cache, which does not expose a capacity setting or key enumeration.
type BoundedCache struct {
	backend      CacheBackend
	prefix       string
	maxEntries   int
	maxValueSize int
}

func NewBoundedCache(backend CacheBackend, prefix string, maxEntries, maxValueSize int) *BoundedCache {
	if maxEntries < 1 {
		maxEntries = DefaultCacheEntries
	}
	if maxValueSize < 1 {
		maxValueSize = DefaultCacheValue
	}
	return &BoundedCache{
		backend:      backend,
		prefix:       prefix,
		maxEntries:   maxEntries,
		maxValueSize: maxValueSize,
	}
}

func (c *BoundedCache) storageKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return c.prefix + ":v1:" + hex.EncodeToString(sum[:])
}

func (c *BoundedCache) indexKey() string {
	return c.prefix + ":v1:index"
}

func (c *BoundedCache) GetJSON(key string, destination any) bool {
	if c == nil || c.backend == nil {
		return false
	}
	storageKey := c.storageKey(key)
	raw, exists, err := c.backend.GetString(storageKey)
	if err != nil || !exists {
		return false
	}
	if len(raw) > c.maxValueSize || json.Unmarshal([]byte(raw), destination) != nil {
		_ = c.backend.Remove(storageKey)
		return false
	}
	return true
}

func (c *BoundedCache) SetJSON(key string, value any, ttlSeconds int64) error {
	if c == nil || c.backend == nil {
		return errors.New("cache backend is nil")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > c.maxValueSize {
		return fmt.Errorf("cache value exceeds %d bytes", c.maxValueSize)
	}
	storageKey := c.storageKey(key)
	entries := c.readIndex()
	entries = slices.DeleteFunc(entries, func(entry string) bool {
		return entry == storageKey
	})
	entries = append(entries, storageKey)

	var evicted []string
	if len(entries) > c.maxEntries {
		evicted = append(evicted, entries[:len(entries)-c.maxEntries]...)
		entries = entries[len(entries)-c.maxEntries:]
	}
	index, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := c.backend.SetString(c.indexKey(), string(index), cacheIndexTTL); err != nil {
		return err
	}
	if err := c.backend.SetString(storageKey, string(raw), ttlSeconds); err != nil {
		return err
	}
	for _, key := range evicted {
		_ = c.backend.Remove(key)
	}
	return nil
}

func (c *BoundedCache) readIndex() []string {
	if c == nil || c.backend == nil {
		return nil
	}
	raw, exists, err := c.backend.GetString(c.indexKey())
	if err != nil || !exists || len(raw) > c.maxEntries*96+2 {
		return nil
	}
	var entries []string
	if json.Unmarshal([]byte(raw), &entries) != nil {
		return nil
	}
	if len(entries) > c.maxEntries {
		entries = entries[len(entries)-c.maxEntries:]
	}
	return entries
}

// Throttle spaces sequential requests across separate WASM invocations by using
// a fixed host-cache timestamp. The host API has no atomic compare-and-swap, so
// simultaneous instances can still race while reserving the same interval.
func (c *BoundedCache) Throttle(key string, interval time.Duration, deadline time.Time, now func() time.Time, sleep func(time.Duration)) error {
	if c == nil || c.backend == nil {
		return &RetryError{After: interval, Cause: errors.New("cache backend unavailable for provider pacing")}
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	throttleKey := c.prefix + ":v1:throttle:" + c.storageKey(key)[len(c.prefix)+4:]
	lastMillis, exists, err := c.backend.GetInt(throttleKey)
	if err != nil {
		return &RetryError{After: interval, Cause: errors.New("provider pacing state unavailable")}
	}
	if exists {
		wait := interval - now().Sub(time.UnixMilli(lastMillis))
		if wait > 0 {
			if !deadline.IsZero() && now().Add(wait).After(deadline) {
				return &RetryError{After: wait, Cause: errors.New("provider request rate limited")}
			}
			sleep(wait)
		}
	}
	if err := c.backend.SetInt(throttleKey, now().UnixMilli(), int64(time.Hour.Seconds())); err != nil {
		return &RetryError{After: interval, Cause: errors.New("provider pacing state could not be saved")}
	}
	return nil
}
