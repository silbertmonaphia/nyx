package rag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/feed"
	"nyx/internal/kb"
	"nyx/internal/llm/ssrf"
)

const (
	rerankFailureThreshold = 3
	rerankCooldown         = 30 * time.Second
	defaultRerankTimeout   = 2 * time.Second
	maxRerankResponseBytes = 4 << 20
)

var rerankOutcomes = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rag_rerank_total",
		Help: "Rerank stage outcomes by result.",
	},
	[]string{"outcome"},
)

type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
	Model() string
	Key() string
}

type RerankerResolver func(ctx context.Context, userID int) (Reranker, error)

type HTTPReranker struct {
	baseURL string
	apiKey  string
	model   string
	key     string
	client  *http.Client
	tracer  trace.Tracer
}

func NewHTTPReranker(baseURL, apiKey, model string, timeout time.Duration, allowPrivate bool, tracer trace.Tracer) *HTTPReranker {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if timeout <= 0 {
		timeout = defaultRerankTimeout
	}
	return &HTTPReranker{
		baseURL: trimmed,
		apiKey:  apiKey,
		model:   model,
		key:     rerankClientKey(trimmed, model, apiKey),
		client: &http.Client{
			Transport: ssrf.NewTransport(allowPrivate, "rerank_base_url"),
			Timeout:   timeout,
		},
		tracer: tracer,
	}
}

func rerankClientKey(baseURL, model, apiKey string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{baseURL, model, apiKey}, "|")))
	return fmt.Sprintf("%x", sum)
}

func (r *HTTPReranker) Model() string { return r.model }

func (r *HTTPReranker) Key() string { return r.key }

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

func RerankPayload(model, query string, docs []string) ([]byte, error) {
	return json.Marshal(rerankRequest{
		Model:     model,
		Query:     query,
		Documents: docs,
		TopN:      len(docs),
	})
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	ctx, span := r.tracer.Start(ctx, "rag.rerank.request",
		trace.WithAttributes(
			attribute.String("rag.rerank.model", r.model),
			attribute.Int("rag.rerank.documents", len(docs)),
		),
	)
	defer span.End()

	payload, err := RerankPayload(r.model, query, docs)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rag: encode rerank request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/rerank", bytes.NewReader(payload))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rag: build rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "rerank transport failed")
		return nil, fmt.Errorf("rag: rerank request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRerankResponseBytes))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rag: read rerank response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("rag: rerank endpoint returned status %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, "rerank upstream error")
		return nil, err
	}

	var out rerankResponse
	if err := json.Unmarshal(body, &out); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rag: decode rerank response: %w", err)
	}

	scores := make([]float64, len(docs))
	for i := range scores {
		scores[i] = math.NaN()
	}
	scored := 0
	for _, result := range out.Results {
		if result.Index < 0 || result.Index >= len(docs) {
			continue
		}
		if !math.IsNaN(scores[result.Index]) {
			continue
		}
		scores[result.Index] = result.RelevanceScore
		scored++
	}
	span.SetAttributes(attribute.Int("rag.rerank.scored", scored))
	span.SetStatus(codes.Ok, "")
	return scores, nil
}

func orderCandidates(scores []float64, n int) []int {
	if n <= 0 {
		return nil
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		sa, oka := candidateScore(scores, ia)
		sb, okb := candidateScore(scores, ib)
		if oka != okb {
			return oka
		}
		if oka && sa != sb {
			return sa > sb
		}
		return ia < ib
	})
	return order
}

func candidateScore(scores []float64, index int) (float64, bool) {
	if index < 0 || index >= len(scores) {
		return 0, false
	}
	score := scores[index]
	if math.IsNaN(score) {
		return 0, false
	}
	return score, true
}

type rerankBreaker struct {
	failures  int
	openUntil time.Time
}

func (s *Service) SetReranker(resolver RerankerResolver, candidates, maxDocChars int) {
	s.rerankerResolver = resolver
	s.rerankCandidates = candidates
	s.rerankMaxDocChars = maxDocChars
	s.breakMu.Lock()
	s.breakers = map[string]*rerankBreaker{}
	s.breakMu.Unlock()
}

func (s *Service) resolveReranker(ctx context.Context, userID int) Reranker {
	if s.rerankerResolver == nil {
		return nil
	}
	reranker, err := s.rerankerResolver(ctx, userID)
	if err != nil {
		log.Warn().Err(err).Int("rag_user_id", userID).Msg("rerank resolver failed; using dense order")
		return nil
	}
	return reranker
}

func (s *Service) fetchK(requested int, reranker Reranker) int {
	if reranker == nil || requested <= 0 || s.rerankCandidates <= requested {
		return requested
	}
	return s.rerankCandidates
}

func (s *Service) rerankFeedPassages(ctx context.Context, userID int, query string, reranker Reranker, passages []Passage, requested int) []Passage {
	if reranker == nil || len(passages) == 0 {
		return passages
	}
	docs := make([]string, len(passages))
	for i, passage := range passages {
		docs[i] = truncateUTF8(feedRerankText(passage), s.rerankMaxDocChars)
	}
	order := s.rerankOrder(ctx, userID, query, reranker, docs)
	if order == nil {
		return truncatePassages(passages, requested)
	}
	out := make([]Passage, 0, min(len(order), requested))
	for _, index := range order {
		if requested > 0 && len(out) >= requested {
			break
		}
		out = append(out, passages[index])
	}
	return out
}

func (s *Service) rerankKnowledgePassages(ctx context.Context, userID int, query string, reranker Reranker, passages []kb.Passage, requested int) []kb.Passage {
	if reranker == nil || len(passages) == 0 {
		return passages
	}
	docs := make([]string, len(passages))
	for i, passage := range passages {
		docs[i] = truncateUTF8(kb.RerankText(passage), s.rerankMaxDocChars)
	}
	order := s.rerankOrder(ctx, userID, query, reranker, docs)
	if order == nil {
		return truncateKnowledgePassages(passages, requested)
	}
	out := make([]kb.Passage, 0, min(len(order), requested))
	for _, index := range order {
		if requested > 0 && len(out) >= requested {
			break
		}
		out = append(out, passages[index])
	}
	return out
}

func (s *Service) rerankOrder(ctx context.Context, userID int, query string, reranker Reranker, docs []string) []int {
	ctx, span := s.tracer.Start(ctx, "rag.rerank",
		trace.WithAttributes(
			attribute.String("rag.rerank.model", reranker.Model()),
			attribute.Int("rag.rerank.candidates", len(docs)),
		),
	)
	defer span.End()

	key := reranker.Key()
	if !s.breakerAllow(key) {
		rerankOutcomes.WithLabelValues("cooldown").Inc()
		span.SetAttributes(attribute.Bool("rag.rerank.cooldown", true))
		return nil
	}
	scores, err := reranker.Rerank(ctx, query, docs)
	if err != nil {
		s.breakerRecord(key, false)
		rerankOutcomes.WithLabelValues("fallback").Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, "dense fallback")
		log.Warn().Err(err).Int("rag_user_id", userID).Msg("rerank failed; using dense order")
		return nil
	}
	scored := 0
	for _, score := range scores {
		if !math.IsNaN(score) {
			scored++
		}
	}
	if scored == 0 {
		s.breakerRecord(key, false)
		rerankOutcomes.WithLabelValues("fallback").Inc()
		span.SetAttributes(
			attribute.Int("rag.rerank.scored", 0),
			attribute.Bool("rag.rerank.fallback", true),
		)
		span.SetStatus(codes.Error, "no usable rerank scores")
		log.Warn().Int("rag_user_id", userID).Msg("rerank returned no usable scores; using dense order")
		return nil
	}
	s.breakerRecord(key, true)
	rerankOutcomes.WithLabelValues("success").Inc()
	order := orderCandidates(scores, len(docs))
	span.SetAttributes(
		attribute.Int("rag.rerank.scored", scored),
		attribute.Int("rag.rerank.returned", len(order)),
		attribute.Bool("rag.rerank.fallback", false),
	)
	span.SetStatus(codes.Ok, "")
	return order
}

func (s *Service) breakerAllow(key string) bool {
	s.breakMu.Lock()
	defer s.breakMu.Unlock()
	breaker, ok := s.breakers[key]
	if !ok {
		return true
	}
	return !time.Now().Before(breaker.openUntil)
}

func (s *Service) breakerRecord(key string, ok bool) {
	s.breakMu.Lock()
	defer s.breakMu.Unlock()
	if s.breakers == nil {
		s.breakers = map[string]*rerankBreaker{}
	}
	breaker, found := s.breakers[key]
	if !found {
		breaker = &rerankBreaker{}
		s.breakers[key] = breaker
	}
	if ok {
		breaker.failures = 0
		breaker.openUntil = time.Time{}
		return
	}
	breaker.failures++
	if breaker.failures >= rerankFailureThreshold {
		breaker.failures = 0
		breaker.openUntil = time.Now().Add(rerankCooldown)
	}
}

func feedRerankText(p Passage) string {
	return ChunkText(&feed.Feed{Title: p.Title, Description: p.Description})
}

func truncatePassages(passages []Passage, requested int) []Passage {
	if requested <= 0 {
		return nil
	}
	if len(passages) <= requested {
		return passages
	}
	return passages[:requested]
}

func truncateKnowledgePassages(passages []kb.Passage, requested int) []kb.Passage {
	if requested <= 0 {
		return nil
	}
	if len(passages) <= requested {
		return passages
	}
	return passages[:requested]
}
