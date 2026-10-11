// Package cache provides a small abstraction over a key-value cache.
// The interface is satisfied by both Redis (production) and a no-op
// implementation (when caching is disabled). All methods are safe for
// concurrent use; serialization is JSON.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

// cacheOps records the outcome of every Cache operation. The result
// label distinguishes the three useful states ("hit", "miss", "error")
// so dashboards can compute hit ratio without scraping logs.
var cacheOps = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cache_operations_total",
		Help: "Cache Get/Set/Delete outcomes, labeled by op and result.",
	},
	[]string{"op", "result"},
)

// Cache is the interface used by service-layer code. Get returns
// (false, nil) on a cache miss — callers must distinguish a miss from
// an error so they can fall through to the source of truth.
//
// IncrBy atomically increments the integer value at key by delta and
// returns the new value. A missing key is treated as zero, so the
// first call with delta=N yields N. The noop implementation returns
// (0, nil) — counter call-sites that fail closed on a real-Redis
// error treat (0, noopErr) as "no counter backend" rather than
// "backend down". IncrBy is the counter primitive the agent budget
// guard relies on; today's chat handlers don't use it.
//
// Expire sets/updates the TTL on key without touching the value. The
// noop implementation is a no-op. Expire is paired with IncrBy by the
// budget guard so the per-hour counter resets at the hour boundary.
type Cache interface {
	Get(ctx context.Context, key string, dst any) (bool, error)
	Set(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	DeletePrefix(ctx context.Context, prefix string) error
	Ping(ctx context.Context) error
	Close() error
	IncrBy(ctx context.Context, key string, delta int64) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

// noopCache satisfies Cache without doing anything. It is used when
// REDIS_ENABLED=false so service code can stay cache-aware unconditionally.
type noopCache struct{}

func NewNoop() Cache { return &noopCache{} }

func (noopCache) Get(context.Context, string, any) (bool, error) {
	cacheOps.WithLabelValues("get", "miss").Inc()
	return false, nil
}
func (noopCache) Set(context.Context, string, any, time.Duration) error { return nil }
func (noopCache) Delete(context.Context, ...string) error               { return nil }
func (noopCache) DeletePrefix(context.Context, string) error            { return nil }
func (noopCache) Ping(context.Context) error                            { return nil }
func (noopCache) Close() error                                          { return nil }
func (noopCache) IncrBy(context.Context, string, int64) (int64, error)  { return 0, nil }
func (noopCache) Expire(context.Context, string, time.Duration) error   { return nil }

// redisCache wraps a *redis.Client.
type redisCache struct {
	client *redis.Client
}

// NewRedis connects to the given URL and returns a Cache implementation.
// It pings the server with a 5s timeout so configuration errors surface
// at startup rather than on the first request.
func NewRedis(url string) (Cache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &redisCache{client: client}, nil
}

// newRedisFromClient is used by tests that wire a real *redis.Client
// (e.g. against miniredis).
func newRedisFromClient(c *redis.Client) Cache { return &redisCache{client: c} }

func (c *redisCache) Get(ctx context.Context, key string, dst any) (bool, error) {
	raw, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		cacheOps.WithLabelValues("get", "miss").Inc()
		return false, nil
	}
	if err != nil {
		cacheOps.WithLabelValues("get", "error").Inc()
		return false, err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		cacheOps.WithLabelValues("get", "error").Inc()
		return false, fmt.Errorf("decode cached value: %w", err)
	}
	cacheOps.WithLabelValues("get", "hit").Inc()
	return true, nil
}

func (c *redisCache) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode cached value: %w", err)
	}
	return c.client.Set(ctx, key, raw, ttl).Err()
}

func (c *redisCache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

// DeletePrefix removes every key starting with prefix. It uses SCAN +
// UNLINK so large invalidations do not block Redis the way KEYS would.
func (c *redisCache) DeletePrefix(ctx context.Context, prefix string) error {
	pattern := prefix + "*"
	iter := c.client.Scan(ctx, 0, pattern, 100).Iterator()
	batch := make([]string, 0, 100)
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 100 {
			if err := c.client.Unlink(ctx, batch...).Err(); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		if err := c.client.Unlink(ctx, batch...).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (c *redisCache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// IncrBy atomically increments the integer value at key by delta.
// Used by the agent budget guard to charge per-user token usage.
// Returns the new counter value (post-increment); missing keys
// behave as zero, so the first call with delta=N yields N.
func (c *redisCache) IncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	return c.client.IncrBy(ctx, key, delta).Result()
}

// Expire sets the TTL on key without changing its value. The agent
// budget guard calls it once after the counter's first increment so
// the per-hour key naturally expires and resets at the hour boundary.
func (c *redisCache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.client.Expire(ctx, key, ttl).Err()
}

func (c *redisCache) Close() error {
	return c.client.Close()
}
