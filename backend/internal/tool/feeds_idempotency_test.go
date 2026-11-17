package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nyx/internal/feed"
	"nyx/internal/idempotency"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type idempotentFeed struct {
	stubFeedService
	createCalls, updateCalls, deleteCalls int
	lastKey                               string
	lastID                                int
	err                                   error
}

func (f *idempotentFeed) CreateFeedIdempotent(_ context.Context, m *feed.Feed, key string) error {
	f.createCalls++
	f.lastKey = key
	if f.err != nil {
		return f.err
	}
	m.ID = 88
	return nil
}

func (f *idempotentFeed) UpdateFeedIdempotent(_ context.Context, id int, m *feed.Feed, key string) error {
	f.updateCalls++
	f.lastKey = key
	f.lastID = id
	m.ID = id
	return nil
}

func (f *idempotentFeed) DeleteFeedIdempotent(_ context.Context, id int, key string) (feed.DeleteReceipt, error) {
	f.deleteCalls++
	f.lastKey = key
	f.lastID = id
	return feed.DeleteReceipt{Deleted: true, ID: id}, nil
}

func TestFeedTools_IdempotentCreateRoutesWithKey(t *testing.T) {
	feeds := &idempotentFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	got, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{
		Title:          "t",
		IdempotencyKey: "run-1:step-1:create_feed:abc",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, feeds.createCalls)
	assert.Equal(t, "run-1:step-1:create_feed:abc", feeds.lastKey)
	created, ok := got.(feed.Feed)
	require.True(t, ok)
	assert.Equal(t, 88, created.ID)
	assert.Equal(t, 7, created.UserID)
}

func TestFeedTools_IdempotentUpdateRoutesWithKey(t *testing.T) {
	feeds := &idempotentFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	got, err := r.Dispatch(ctxWithUser(7), ToolNameUpdateFeed, UpdateFeedArgs{
		ID:             5,
		Title:          "new",
		IdempotencyKey: "k-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, feeds.updateCalls)
	assert.Equal(t, 5, feeds.lastID)
	assert.Equal(t, "k-1", feeds.lastKey)
	updated, ok := got.(feed.Feed)
	require.True(t, ok)
	assert.Equal(t, 5, updated.ID)
}

func TestFeedTools_IdempotentDeleteRoutesWithKey(t *testing.T) {
	feeds := &idempotentFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	got, err := r.Dispatch(ctxWithUser(7), ToolNameDeleteFeed, DeleteFeedArgs{
		ID:             5,
		IdempotencyKey: "k-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, feeds.deleteCalls)
	assert.Equal(t, "k-1", feeds.lastKey)

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"deleted": true, "id": 5}`, string(raw))
}

func TestFeedTools_NoKeyUsesLegacyPath(t *testing.T) {
	feeds := &idempotentFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{Title: "t"})
	require.NoError(t, err)
	assert.Zero(t, feeds.createCalls)
	assert.Empty(t, feeds.lastKey)
}

func TestFeedTools_InvalidIdempotencyKeyFailsBeforeBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "space", key: "bad key"},
		{name: "slash", key: "bad/key"},
		{name: "too long", key: strings.Repeat("a", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureLogger(t)
			feeds := &idempotentFeed{}
			r := New()
			RegisterFeedTools(r, feeds)

			_, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{
				Title:          "t",
				IdempotencyKey: tc.key,
			})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidArgs), "got %v", err)
			var fieldErr *FieldError
			require.True(t, errors.As(err, &fieldErr))
			assert.Equal(t, "IdempotencyKey", fieldErr.Field)
			assert.Zero(t, feeds.createCalls, "tool body must not run on an invalid key")
		})
	}
}

func TestFeedTools_KeyCharsetAcceptsAllowedCharacters(t *testing.T) {
	r := New()
	RegisterFeedTools(r, &idempotentFeed{})

	for _, key := range []string{
		"AZaz09",
		"a-b_c:d",
		"018f4e3c-7a1b-5c2d-9e0f-123456789abc",
	} {
		_, err := r.Dispatch(ctxWithUser(1), ToolNameCreateFeed, CreateFeedArgs{
			Title:          "t",
			IdempotencyKey: key,
		})
		assert.NoError(t, err, "key %q must be accepted", key)
	}
}

func TestFeedTools_KeyReusedMapsToInvalidArgs(t *testing.T) {
	captureLogger(t)
	feeds := &idempotentFeed{err: idempotency.ErrKeyReused}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{
		Title:          "t",
		IdempotencyKey: "k-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidArgs), "got %v", err)
	assert.Equal(t, 1, feeds.createCalls)
}

func TestFeedTools_IdempotencyUnsupportedWithoutStore(t *testing.T) {
	captureLogger(t)
	feeds := stubFeedService{}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{
		Title:          "t",
		IdempotencyKey: "k-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIdempotencyUnsupported), "got %v", err)
}
