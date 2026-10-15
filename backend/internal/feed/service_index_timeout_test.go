package feed

import (
	"context"
	"testing"
	"time"

	"nyx/internal/platform/cache"

	"go.opentelemetry.io/otel/trace/noop"
)

type ctxBlockingIndexer struct {
	indexEntered  bool
	deleteEntered bool
}

func (b *ctxBlockingIndexer) Index(ctx context.Context, _ int, _ *Feed) error {
	b.indexEntered = true
	<-ctx.Done()
	return ctx.Err()
}

func (b *ctxBlockingIndexer) Delete(ctx context.Context, _ int) error {
	b.deleteEntered = true
	<-ctx.Done()
	return ctx.Err()
}

func TestWrites_BoundTheIndexCall(t *testing.T) {
	const indexCap = 100 * time.Millisecond
	cases := []struct {
		name string
		run  func(svc Service, ctx context.Context) error
		saw  func(b *ctxBlockingIndexer) bool
	}{
		{
			name: "create",
			run: func(svc Service, ctx context.Context) error {
				return svc.CreateFeed(ctx, &Feed{ID: 11, Title: "t"})
			},
			saw: func(b *ctxBlockingIndexer) bool { return b.indexEntered },
		},
		{
			name: "update",
			run: func(svc Service, ctx context.Context) error {
				return svc.UpdateFeed(ctx, 11, &Feed{ID: 11, Title: "t2"})
			},
			saw: func(b *ctxBlockingIndexer) bool { return b.indexEntered },
		},
		{
			name: "delete",
			run: func(svc Service, ctx context.Context) error {
				return svc.DeleteFeed(ctx, 11)
			},
			saw: func(b *ctxBlockingIndexer) bool { return b.deleteEntered },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := &ctxBlockingIndexer{}
			repo := &stubRepo{}
			svc := NewService(repo, cache.NewNoop(), idx, time.Minute, indexCap, noop.NewTracerProvider().Tracer("test"))
			ctx := ctxWithUser(t, 1)

			start := time.Now()
			if err := tc.run(svc, ctx); err != nil {
				t.Fatalf("write must succeed despite a hanging indexer: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("write took %v; the index cap did not apply", elapsed)
			}
			if !tc.saw(idx) {
				t.Fatal("indexer was never invoked")
			}
			if !idx.deleteEntered {
				t.Fatal("a skipped index must drop any stale embedding row")
			}
			if repo.lastUserID != 1 {
				t.Fatalf("repo saw user %d, want 1", repo.lastUserID)
			}
		})
	}
}

func TestNewService_ClampsNonPositiveIndexTimeout(t *testing.T) {
	for _, in := range []time.Duration{0, -time.Second} {
		svc := NewService(&stubRepo{}, cache.NewNoop(), NoopEmbeddingIndexer{}, time.Minute, in, noop.NewTracerProvider().Tracer("test"))
		fs, ok := svc.(*feedService)
		if !ok {
			t.Fatal("unexpected Service implementation")
		}
		if fs.indexTimeout != DefaultIndexTimeout {
			t.Errorf("indexTimeout(%v) = %v, want the %v default", in, fs.indexTimeout, DefaultIndexTimeout)
		}
	}
}
