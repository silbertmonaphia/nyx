package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/kb"
	"nyx/internal/llm"
	"nyx/internal/rag"
	"nyx/internal/reqctx"
)

type chatKBRetriever struct {
	passages   []kb.Passage
	err        error
	calls      int
	lastUserID int64
	catchUps   atomic.Int32
	resets     atomic.Int32
}

func (r *chatKBRetriever) Retrieve(_ context.Context, userID int64, _ []float32, _ string, _ int, _ *int64, _ string) ([]kb.Passage, error) {
	r.calls++
	r.lastUserID = userID
	return r.passages, r.err
}

func (r *chatKBRetriever) CatchUpOnce(_ context.Context, _ int64) {
	r.catchUps.Add(1)
}

func (r *chatKBRetriever) ResetCatchUp(_ int64) {
	r.resets.Add(1)
}

func buildServiceWithKnowledge(t *testing.T) (*Service, *chatRAGRetriever, *chatKBRetriever) {
	t.Helper()
	feedRetriever := &chatRAGRetriever{}
	ragService := buildRAGService(feedRetriever, &chatRAGEmbedder{vecs: [][]float32{{0.5, 0.5}}})
	kbRetriever := &chatKBRetriever{}
	ragService.SetKnowledgeRetriever(kbRetriever, 5, 4000)
	return newTestServiceWithRAG(&stubProvider{}, ragService), feedRetriever, kbRetriever
}

func TestChat_KBOn_InjectsKnowledgeContext(t *testing.T) {
	service, feedRetriever, kbRetriever := buildServiceWithKnowledge(t)
	kbRetriever.passages = []kb.Passage{{
		DocID:       7,
		SourceName:  "shared",
		RelPath:     "B-Cook/diet.md",
		HeadingPath: []string{"煮食记"},
		Content:     "一份碳水 + 一份蛋白",
		Score:       0.9,
	}}
	provider := service.provider.(*stubProvider)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "我的饮食笔记说什么？"}},
		KB:       true,
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, kbRetriever.calls)
	assert.Equal(t, int64(42), kbRetriever.lastUserID)
	assert.Equal(t, 0, feedRetriever.calls)
	require.Len(t, provider.lastReq.Messages, 3)
	injected := provider.lastReq.Messages[2]
	assert.Contains(t, injected.Content, "my knowledge base")
	assert.Contains(t, injected.Content, "B-Cook/diet.md")
	assert.Contains(t, injected.Content, "一份碳水 + 一份蛋白")
	assert.Contains(t, injected.Content, "我的饮食笔记说什么？")
	require.Eventually(t, func() bool { return kbRetriever.catchUps.Load() == 1 }, time.Second, 10*time.Millisecond)
}

func TestChat_KBOff_NoKnowledgeRetrieval(t *testing.T) {
	service, _, kbRetriever := buildServiceWithKnowledge(t)
	provider := service.provider.(*stubProvider)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, kbRetriever.calls)
	assert.Len(t, provider.lastReq.Messages, 2)
}

func TestChat_KBEmptyResult_NoInjection(t *testing.T) {
	service, _, _ := buildServiceWithKnowledge(t)
	provider := service.provider.(*stubProvider)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		KB:       true,
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)
	assert.Len(t, provider.lastReq.Messages, 2)
}

func TestChat_KBRetrievalErrorDoesNotFailChat(t *testing.T) {
	service, _, kbRetriever := buildServiceWithKnowledge(t)
	kbRetriever.err = errors.New("kb unavailable")
	provider := service.provider.(*stubProvider)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		KB:       true,
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)
	assert.Len(t, provider.lastReq.Messages, 2)
}

func TestChat_RAGUnchangedWhenKBAlsoOn(t *testing.T) {
	service, feedRetriever, kbRetriever := buildServiceWithKnowledge(t)
	feedRetriever.passages = []rag.Passage{{FeedID: 1, Title: "Feed A", Description: "desc"}}
	kbRetriever.passages = []kb.Passage{{DocID: 2, SourceName: "shared", RelPath: "a.md", Content: "doc body"}}
	provider := service.provider.(*stubProvider)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "question"}},
		RAG:      true,
		KB:       true,
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, feedRetriever.calls)
	assert.Equal(t, 1, kbRetriever.calls)
	require.Len(t, provider.lastReq.Messages, 4)
	assert.Contains(t, provider.lastReq.Messages[2].Content, "[Feed 1] Title: Feed A")
	assert.Contains(t, provider.lastReq.Messages[3].Content, "a.md")
}

func TestChat_KBOffDoesNotCatchUp(t *testing.T) {
	service, _, kbRetriever := buildServiceWithKnowledge(t)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := service.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil }, nil)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), kbRetriever.catchUps.Load())
}
