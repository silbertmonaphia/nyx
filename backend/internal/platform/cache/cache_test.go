package cache

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

func newTestCache(t *testing.T) (Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return newRedisFromClient(client), mr
}

func TestNoopCache(t *testing.T) {
	c := NewNoop()
	ctx := context.Background()

	hit, err := c.Get(ctx, "k", nil)
	if err != nil || hit {
		t.Errorf("noop Get: got hit=%v err=%v, want false/nil", hit, err)
	}
	if err := c.Set(ctx, "k", "v", time.Minute); err != nil {
		t.Errorf("noop Set err=%v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("noop Delete err=%v", err)
	}
	if err := c.DeletePrefix(ctx, "k"); err != nil {
		t.Errorf("noop DeletePrefix err=%v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("noop Ping err=%v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("noop Close err=%v", err)
	}
	// Counter primitives — no-op returns succeed without error so
	// callers that treat (0, nil) as "backend down" see the same
	// shape for both noop and a real Redis that errored.
	if v, err := c.IncrBy(ctx, "k", 5); err != nil || v != 0 {
		t.Errorf("noop IncrBy: got v=%d err=%v, want 0/nil", v, err)
	}
	if err := c.Expire(ctx, "k", time.Minute); err != nil {
		t.Errorf("noop Expire err=%v", err)
	}
	if mode := c.Mode(); mode != "noop" {
		t.Errorf("noop Mode: got %q, want %q", mode, "noop")
	}
}

func TestRedisCacheHitMiss(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	type payload struct{ Name string }

	var got payload
	hit, err := c.Get(ctx, "missing", &got)
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if hit {
		t.Errorf("expected miss on missing key")
	}

	want := payload{Name: "neo"}
	if err = c.Set(ctx, "p", want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	hit, err = c.Get(ctx, "p", &got)
	if err != nil {
		t.Fatalf("Get present: %v", err)
	}
	if !hit {
		t.Errorf("expected hit")
	}
	if got.Name != "neo" {
		t.Errorf("got=%q want=neo", got.Name)
	}
}

func TestRedisCacheTTLExpiry(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	type p struct{ V int }
	if err := c.Set(ctx, "ttl", p{V: 1}, 10*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	mr.FastForward(11 * time.Second)

	var got p
	hit, _ := c.Get(ctx, "ttl", &got)
	if hit {
		t.Errorf("expected miss after TTL expiry")
	}
}

func TestRedisCacheDelete(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	if err := c.Set(ctx, "k1", "v", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Set(ctx, "k2", "v", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := c.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	hit1, _ := c.Get(ctx, "k1", new(string))
	hit2, _ := c.Get(ctx, "k2", new(string))
	if hit1 || !hit2 {
		t.Errorf("expected k1 miss / k2 hit, got k1=%v k2=%v", hit1, hit2)
	}
}

func TestRedisCacheDeletePrefix(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	keys := []string{
		"feeds:q=:p=1:s=20",
		"feeds:q=foo:p=1:s=20",
		"feeds:q=foo:p=2:s=20",
		"other:key",
	}
	for _, k := range keys {
		if err := c.Set(ctx, k, "v", time.Minute); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	if err := c.DeletePrefix(ctx, "feeds:"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}

	for _, k := range keys[:3] {
		hit, _ := c.Get(ctx, k, new(string))
		if hit {
			t.Errorf("expected %s to be deleted", k)
		}
	}
	hit, _ := c.Get(ctx, "other:key", new(string))
	if !hit {
		t.Errorf("expected other:key to survive")
	}
}

func TestRedisCachePing(t *testing.T) {
	c, _ := newTestCache(t)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// TestRedisCacheIncrBy exercises the INCRBY primitive the agent
// budget guard relies on. Counter semantics: missing key is treated
// as zero, INCRBY adds, the new value is returned. A second INCRBY
// stacks. Expire sets a TTL without touching the value.
func TestRedisCacheIncrBy(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	v, err := c.IncrBy(ctx, "agent:budget:u=1:h=2026010900", 100)
	if err != nil {
		t.Fatalf("IncrBy: %v", err)
	}
	if v != 100 {
		t.Errorf("first IncrBy: got %d, want 100", v)
	}

	v, err = c.IncrBy(ctx, "agent:budget:u=1:h=2026010900", 50)
	if err != nil {
		t.Fatalf("IncrBy: %v", err)
	}
	if v != 150 {
		t.Errorf("second IncrBy: got %d, want 150", v)
	}

	if err := c.Expire(ctx, "agent:budget:u=1:h=2026010900", time.Minute); err != nil {
		t.Fatalf("Expire: %v", err)
	}
}

// TestCacheMetrics verifies that every Get outcome increments the
// cache_operations_total counter with the right op/result labels so
// dashboards can compute hit ratio.
func TestCacheMetrics(t *testing.T) {
	cacheOps.Reset()

	c, _ := newTestCache(t)
	ctx := context.Background()

	var v string

	// 1st Get("k") — miss (key not set).
	if hit, err := c.Get(ctx, "k", &v); err != nil || hit {
		t.Fatalf("Get before Set: hit=%v err=%v, want false/nil", hit, err)
	}

	// Set, then 2nd Get("k") — hit.
	if err := c.Set(ctx, "k", "v", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if hit, err := c.Get(ctx, "k", &v); err != nil || !hit {
		t.Fatalf("Get after Set: hit=%v err=%v, want true/nil", hit, err)
	}

	// 2nd miss.
	if hit, err := c.Get(ctx, "missing", &v); err != nil || hit {
		t.Fatalf("Get missing: hit=%v err=%v, want false/nil", hit, err)
	}

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		if mf.GetName() != "cache_operations_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels = append(labels, lp.GetName()+`="`+lp.GetValue()+`"`)
			}
			b.WriteString("cache_operations_total")
			b.WriteByte('{')
			b.WriteString(strings.Join(labels, ","))
			b.WriteString("} ")
			b.WriteString(strconv.FormatFloat(m.GetCounter().GetValue(), 'f', -1, 64))
			b.WriteByte('\n')
		}
	}
	gathered := b.String()

	for _, want := range []string{
		`cache_operations_total{op="get",result="hit"} 1`,
		`cache_operations_total{op="get",result="miss"} 2`,
	} {
		if !strings.Contains(gathered, want) {
			t.Errorf("missing %q in metrics:\n%s", want, gathered)
		}
	}
}
