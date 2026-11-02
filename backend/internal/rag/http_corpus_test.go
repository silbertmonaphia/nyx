package rag

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/kb"
)

func decodeEnvelope(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var envelope struct {
		ContextBlock string           `json:"context_block"`
		Citations    []map[string]any `json:"citations"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.NotNil(t, envelope.Citations)
	return envelope.Citations
}

func TestBridgeFeedsCorpusIsDefault(t *testing.T) {
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, []Passage{
		{FeedID: 5, Title: "A", Description: "desc"},
	}, nil)
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	citations := decodeEnvelope(t, rr.Body.Bytes())
	require.Len(t, citations, 1)
	assert.Equal(t, CitationKindFeed, citations[0]["kind"])
	assert.EqualValues(t, 5, citations[0]["feed_id"])
	assert.NotContains(t, citations[0], "doc_id")
	assert.Equal(t, 1, feedRetriever.calls)
	assert.Equal(t, 0, knowledgeRetriever.calls)
}

func TestBridgeKnowledgeCorpusShape(t *testing.T) {
	knowledge := []kb.Passage{{
		DocID:       7,
		SourceID:    3,
		SourceName:  "shared",
		RelPath:     "B-Cook/diet.md",
		HeadingPath: []string{"煮食记", "碳水列表"},
		Content:     "一份碳水 + 一份蛋白",
		Locator:     kb.Locator{Line: 12, EndLine: 14},
		Score:       0.9,
	}}
	service, feedRetriever, _ := newCorpusService(t, nil, knowledge)
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=kb", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	citations := decodeEnvelope(t, rr.Body.Bytes())
	require.Len(t, citations, 1)
	citation := citations[0]
	assert.Equal(t, CitationKindDoc, citation["kind"])
	assert.EqualValues(t, 0, citation["feed_id"])
	assert.EqualValues(t, 7, citation["doc_id"])
	assert.Equal(t, "shared", citation["source_name"])
	assert.Equal(t, "B-Cook/diet.md", citation["rel_path"])
	assert.Equal(t, []any{"煮食记", "碳水列表"}, citation["heading_path"])
	assert.Equal(t, "一份碳水 + 一份蛋白", citation["snippet"])
	locator, ok := citation["locator"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 12, locator["line"])
	assert.Equal(t, 0, feedRetriever.calls)
}

func TestBridgeBothCorpusCarriesBothKinds(t *testing.T) {
	service, _, _ := newCorpusService(t,
		[]Passage{{FeedID: 1, Title: "Feed"}},
		[]kb.Passage{{DocID: 2, SourceName: "shared", RelPath: "a.md", Content: "doc"}},
	)
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=both", 42)
	require.Equal(t, http.StatusOK, rr.Code)
	citations := decodeEnvelope(t, rr.Body.Bytes())
	require.Len(t, citations, 2)
	kinds := []any{citations[0]["kind"], citations[1]["kind"]}
	assert.ElementsMatch(t, []any{CitationKindFeed, CitationKindDoc}, kinds)
}

func TestBridgeUnknownCorpusRefusedWithoutSearch(t *testing.T) {
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, []Passage{{FeedID: 1}}, nil)
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=documents", 42)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "corpus")
	assert.Equal(t, 0, feedRetriever.calls)
	assert.Equal(t, 0, knowledgeRetriever.calls)
}

func TestBridgeNarrowingParametersReachKnowledgeSearch(t *testing.T) {
	service, _, knowledgeRetriever := newCorpusService(t, nil, []kb.Passage{{DocID: 1}})
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=kb&source=3&path=B-Sport/&top_k=7", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, knowledgeRetriever.lastSource)
	assert.Equal(t, int64(3), *knowledgeRetriever.lastSource)
	assert.Equal(t, "B-Sport/", knowledgeRetriever.lastPath)
	assert.Equal(t, 7, knowledgeRetriever.lastK)
}

func TestBridgeRejectsInvalidSourceAndTopK(t *testing.T) {
	service, _, _ := newCorpusService(t, nil, nil)
	for _, target := range []string{
		"/internal/rag/retrieve?query=hello&source=abc",
		"/internal/rag/retrieve?query=hello&source=0",
		"/internal/rag/retrieve?query=hello&top_k=0",
		"/internal/rag/retrieve?query=hello&top_k=1000",
	} {
		rr := invokeHandler(t, Handler(service), target, 42)
		assert.Equal(t, http.StatusBadRequest, rr.Code, target)
	}
}

func TestBridgeEmptyResultCitationsIsArray(t *testing.T) {
	service, _, _ := newCorpusService(t, nil, nil)
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=both", 42)
	require.Equal(t, http.StatusOK, rr.Code)
	citations := decodeEnvelope(t, rr.Body.Bytes())
	assert.Empty(t, citations)
	assert.Contains(t, rr.Body.String(), `"citations":[]`)
}

func TestBridgeKnowledgeSearchTriggersCatchUp(t *testing.T) {
	service, _, knowledgeRetriever := newCorpusService(t, nil, []kb.Passage{{DocID: 1}})
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=kb", 42)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Eventually(t, func() bool { return knowledgeRetriever.catchUps.Load() == 1 }, time.Second, 10*time.Millisecond)

	feedsService, _, feedsKnowledge := newCorpusService(t, []Passage{{FeedID: 1}}, nil)
	rr = invokeHandler(t, Handler(feedsService), "/internal/rag/retrieve?query=hello", 42)
	require.Equal(t, http.StatusOK, rr.Code)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), feedsKnowledge.catchUps.Load())
}

func TestBridgeKnowledgeFailureReturnsEmptyEnvelope(t *testing.T) {
	buffer := captureLogger(t)
	service, _, knowledgeRetriever := newCorpusService(t, nil, nil)
	knowledgeRetriever.err = errors.New("database exploded")
	rr := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=kb", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	citations := decodeEnvelope(t, rr.Body.Bytes())
	assert.Empty(t, citations)
	logged := buffer.String()
	assert.Contains(t, logged, "test-req-id")
	assert.NotContains(t, rr.Body.String(), "database exploded")
}
