package tool

import (
	"encoding/json"
	"os"
	"testing"

	"nyx/internal/feed"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const idempotencyWireFixture = "../../agent_py/tests/fixtures/tool_call_idempotency.json"

func TestAgentToolCallIdempotencyWireContract(t *testing.T) {
	raw, err := os.ReadFile(idempotencyWireFixture)
	require.NoError(t, err, "shared agent->Go fixture missing")

	var args CreateFeedArgs
	require.NoError(t, json.Unmarshal(raw, &args), "agent payload must parse into the Go argument struct")
	assert.Equal(t, "Wire contract", args.Title)
	assert.Equal(t, "dbd8dc1a-2753-51a9-a7c6-62fdc76f5fa0", args.IdempotencyKey)

	feeds := &idempotentFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	got, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, args)
	require.NoError(t, err)
	assert.Equal(t, 1, feeds.createCalls)
	assert.Equal(t, args.IdempotencyKey, feeds.lastKey)

	created, ok := got.(feed.Feed)
	require.True(t, ok, "create result must be feed.Feed, got %T", got)
	assert.Equal(t, 88, created.ID)
	assert.Equal(t, 7, created.UserID)
}
