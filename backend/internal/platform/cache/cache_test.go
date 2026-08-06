package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
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
	if err := c.Set(ctx, "p", want, time.Minute); err != nil {
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
		"movies:q=:p=1:s=20",
		"movies:q=foo:p=1:s=20",
		"movies:q=foo:p=2:s=20",
		"other:key",
	}
	for _, k := range keys {
		if err := c.Set(ctx, k, "v", time.Minute); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	if err := c.DeletePrefix(ctx, "movies:"); err != nil {
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