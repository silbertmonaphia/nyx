package rag

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"nyx/internal/kb"
)

type Corpus string

const (
	CorpusFeeds Corpus = "feeds"
	CorpusKB    Corpus = "kb"
	CorpusBoth  Corpus = "both"
)

const rrfRankConstant = 60

var (
	ErrInvalidCorpus         = errors.New("rag: invalid corpus")
	ErrInvalidRetrievalParam = errors.New("rag: invalid retrieval parameter")
)

type KnowledgeRetriever interface {
	Retrieve(ctx context.Context, userID int64, queryVec []float32, embeddingModel string, k int, sourceID *int64, pathPrefix string) ([]kb.Passage, error)
}

type RetrievalOptions struct {
	Corpus     Corpus
	SourceID   *int64
	PathPrefix string
	TopK       int
}

type CorpusPassage struct {
	Kind      string
	Feed      Passage
	Knowledge kb.Passage
	Rank      int
}

type RetrievalResult struct {
	Passages     []CorpusPassage
	ContextBlock string
}

const (
	CitationKindFeed = "feed"
	CitationKindDoc  = "doc"
)

func ParseCorpus(raw string) (Corpus, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(CorpusFeeds):
		return CorpusFeeds, nil
	case string(CorpusKB):
		return CorpusKB, nil
	case string(CorpusBoth):
		return CorpusBoth, nil
	default:
		return "", ErrInvalidCorpus
	}
}

func (s *Service) SetKnowledgeRetriever(retriever KnowledgeRetriever, topK, maxContextChars int) {
	s.knowledge = retriever
	s.kbTopK = topK
	s.kbMaxContextChars = maxContextChars
}

func (s *Service) RetrieveCorpus(ctx context.Context, userID int, query string, opts RetrievalOptions) (RetrievalResult, error) {
	corpus, err := ParseCorpus(string(opts.Corpus))
	if err != nil {
		return RetrievalResult{}, err
	}
	if strings.TrimSpace(query) == "" {
		return RetrievalResult{}, nil
	}
	embedder, err := s.resolveEmbedder(ctx, userID)
	if err != nil {
		return RetrievalResult{}, err
	}
	if embedder == nil {
		return RetrievalResult{}, ErrNoEmbedder
	}
	vectors, err := embedder.EmbedStrings(ctx, []string{query})
	if err != nil {
		return RetrievalResult{}, err
	}
	if len(vectors) == 0 {
		return RetrievalResult{}, nil
	}
	feedK := s.topK
	knowledgeK := s.kbTopK
	if opts.TopK > 0 {
		feedK = opts.TopK
		knowledgeK = opts.TopK
	}
	var feedPassages []Passage
	var knowledgePassages []kb.Passage
	switch corpus {
	case CorpusFeeds:
		feedPassages, err = s.retriever.Retrieve(ctx, userID, vectors[0], feedK)
	case CorpusKB:
		knowledgePassages, err = s.retrieveKnowledge(ctx, userID, vectors[0], embedder.Model(), knowledgeK, opts)
	case CorpusBoth:
		feedPassages, err = s.retriever.Retrieve(ctx, userID, vectors[0], feedK)
		if err == nil {
			knowledgePassages, err = s.retrieveKnowledge(ctx, userID, vectors[0], embedder.Model(), knowledgeK, opts)
		}
	}
	if err != nil {
		return RetrievalResult{}, err
	}
	switch corpus {
	case CorpusFeeds:
		return RetrievalResult{
			Passages:     feedCorpusPassages(feedPassages),
			ContextBlock: renderContextBlock(feedPassages, s.maxCtxChars),
		}, nil
	case CorpusKB:
		return RetrievalResult{
			Passages:     knowledgeCorpusPassages(knowledgePassages),
			ContextBlock: renderKnowledgeContext(knowledgePassages, s.kbMaxContextChars),
		}, nil
	default:
		fused := fuseCorpora(feedPassages, knowledgePassages)
		return RetrievalResult{
			Passages:     fused,
			ContextBlock: renderFusedContext(fused, s.maxCtxChars),
		}, nil
	}
}

func (s *Service) RetrieveKnowledge(ctx context.Context, userID int, query string) ([]kb.Passage, string, error) {
	result, err := s.RetrieveCorpus(ctx, userID, query, RetrievalOptions{Corpus: CorpusKB})
	if err != nil {
		return nil, "", err
	}
	passages := make([]kb.Passage, 0, len(result.Passages))
	for _, passage := range result.Passages {
		if passage.Kind == CitationKindDoc {
			passages = append(passages, passage.Knowledge)
		}
	}
	return passages, result.ContextBlock, nil
}

func (s *Service) CatchUpKnowledge(ctx context.Context, userID int) {
	catchUpper, ok := s.knowledge.(interface {
		CatchUpOnce(ctx context.Context, userID int64)
	})
	if !ok {
		return
	}
	catchUpper.CatchUpOnce(ctx, int64(userID))
}

func (s *Service) retrieveKnowledge(ctx context.Context, userID int, vec []float32, model string, k int, opts RetrievalOptions) ([]kb.Passage, error) {
	if s.knowledge == nil || k <= 0 {
		return nil, nil
	}
	return s.knowledge.Retrieve(ctx, int64(userID), vec, model, k, opts.SourceID, opts.PathPrefix)
}

func feedCorpusPassages(feeds []Passage) []CorpusPassage {
	out := make([]CorpusPassage, len(feeds))
	for i, passage := range feeds {
		out[i] = CorpusPassage{Kind: CitationKindFeed, Feed: passage, Rank: i + 1}
	}
	return out
}

func knowledgeCorpusPassages(passages []kb.Passage) []CorpusPassage {
	out := make([]CorpusPassage, len(passages))
	for i, passage := range passages {
		out[i] = CorpusPassage{Kind: CitationKindDoc, Knowledge: passage, Rank: i + 1}
	}
	return out
}

func fuseCorpora(feeds []Passage, knowledge []kb.Passage) []CorpusPassage {
	type scored struct {
		passage CorpusPassage
		score   float64
		kind    int
	}
	items := make([]scored, 0, len(feeds)+len(knowledge))
	for i, passage := range feeds {
		rank := i + 1
		items = append(items, scored{
			passage: CorpusPassage{Kind: CitationKindFeed, Feed: passage, Rank: rank},
			score:   1.0 / float64(rrfRankConstant+rank),
			kind:    0,
		})
	}
	for i, passage := range knowledge {
		rank := i + 1
		items = append(items, scored{
			passage: CorpusPassage{Kind: CitationKindDoc, Knowledge: passage, Rank: rank},
			score:   1.0 / float64(rrfRankConstant+rank),
			kind:    1,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		if items[i].passage.Rank != items[j].passage.Rank {
			return items[i].passage.Rank < items[j].passage.Rank
		}
		return items[i].kind < items[j].kind
	})
	out := make([]CorpusPassage, len(items))
	for i, item := range items {
		out[i] = item.passage
	}
	return out
}

func renderKnowledgeContext(passages []kb.Passage, maxChars int) string {
	if maxChars <= 0 || len(passages) == 0 {
		return ""
	}
	blocks := make([]string, len(passages))
	for i, passage := range passages {
		blocks[i] = knowledgeBlock(i+1, passage)
	}
	return renderBlocks(blocks, maxChars, "\n\n[truncated — additional passages omitted]")
}

func renderFusedContext(passages []CorpusPassage, maxChars int) string {
	if maxChars <= 0 || len(passages) == 0 {
		return ""
	}
	blocks := make([]string, len(passages))
	for i, passage := range passages {
		if passage.Kind == CitationKindDoc {
			blocks[i] = knowledgeBlock(i+1, passage.Knowledge)
			continue
		}
		blocks[i] = passage.Feed.Render(i + 1)
	}
	return renderBlocks(blocks, maxChars, "\n\n[truncated — additional passages omitted]")
}

func knowledgeBlock(index int, passage kb.Passage) string {
	header := fmt.Sprintf("[Doc %d] Source: %s | Path: %s", index, passage.SourceName, passage.RelPath)
	if len(passage.HeadingPath) > 0 {
		header += " | Section: " + strings.Join(passage.HeadingPath, " / ")
	}
	return header + "\n" + passage.Content
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func renderBlocks(blocks []string, maxChars int, marker string) string {
	if maxChars <= 0 || len(blocks) == 0 {
		return ""
	}
	var b strings.Builder
	for i, block := range blocks {
		next := b.Len() + len(block) + len(contextSeparator)
		if i > 0 {
			next -= len(contextSeparator)
		}
		if next > maxChars && b.Len() == 0 {
			limit := maxChars - len(marker) - 1
			if limit < 0 {
				limit = 0
			}
			if len(block) > limit {
				block = truncateUTF8(block, limit) + "…"
			}
			b.WriteString(block)
			b.WriteString(marker)
			return b.String()
		}
		if next > maxChars {
			b.WriteString(marker)
			break
		}
		if i > 0 {
			b.WriteString(contextSeparator)
		}
		b.WriteString(block)
	}
	return b.String()
}
