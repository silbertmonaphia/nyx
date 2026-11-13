package llmsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/llm"
	"nyx/internal/llm/openai"
	"nyx/internal/llm/ssrf"
)

const (
	ProbeOK            = "ok"
	ProbeAuthFailed    = "auth_failed"
	ProbeRateLimited   = "rate_limited"
	ProbeInvalidModel  = "invalid_model"
	ProbeInvalidConfig = "invalid_config"
	ProbeWidthMismatch = "width_mismatch"
	ProbeUnreachable   = "unreachable"
	ProbeTimeout       = "timeout"

	probeQueryText  = "nyx connectivity probe"
	defaultProbeTTL = 10 * time.Second
)

type SlotProbeResult struct {
	Probed     bool                 `json:"probed"`
	Status     string               `json:"status,omitempty"`
	HTTPStatus int                  `json:"http_status,omitempty"`
	Detail     string               `json:"detail,omitempty"`
	Request    *llm.CapturedRequest `json:"request,omitempty"`
}

type SlotResults struct {
	Chat      *SlotProbeResult `json:"chat"`
	Embedding *SlotProbeResult `json:"embedding"`
	Fallback  *SlotProbeResult `json:"fallback"`
	Tavily    *SlotProbeResult `json:"tavily"`
}

type ValidateResult struct {
	Slots SlotResults `json:"slots"`
}

type ValidateInput struct {
	Values UpdateInput
	Slots  []string
}

func (r *Resolver) ValidateValues(ctx context.Context, userID int, values UpdateInput, slots []string) (*ValidateResult, error) {
	return r.Validate(ctx, userID, ValidateInput{Values: values, Slots: slots})
}

func (r *Resolver) Validate(ctx context.Context, userID int, in ValidateInput) (*ValidateResult, error) {
	stored, err := r.service.Get(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	timeout := r.configProbeTimeout()
	tracer := r.tp.Tracer("nyx.llmsettings.validate")
	res := &ValidateResult{}
	if wantsSlot(in.Slots, "chat") {
		res.Slots.Chat = r.probeOpenAISlot(ctx, tracer, openaiSlotPlan{
			slot: "chat", baseURL: in.Values.LLMBaseURL, submittedKey: in.Values.LLMAPIKey,
			storedKey: storedKeyFor(stored, func(s *Settings) string { return s.LLMAPIKey }),
			model:     in.Values.LLMModel, embed: false,
		}, timeout)
	}
	if wantsSlot(in.Slots, "fallback") {
		res.Slots.Fallback = r.probeOpenAISlot(ctx, tracer, openaiSlotPlan{
			slot: "fallback", baseURL: in.Values.FallbackBaseURL, submittedKey: in.Values.FallbackAPIKey,
			storedKey: storedKeyFor(stored, func(s *Settings) string { return s.FallbackAPIKey }),
			model:     in.Values.FallbackModel, embed: false,
		}, timeout)
	}
	if wantsSlot(in.Slots, "embedding") {
		res.Slots.Embedding = r.probeOpenAISlot(ctx, tracer, openaiSlotPlan{
			slot: "embedding", baseURL: in.Values.EmbeddingBaseURL, submittedKey: in.Values.EmbeddingAPIKey,
			storedKey: storedKeyFor(stored, func(s *Settings) string { return s.EmbeddingAPIKey }),
			model:     in.Values.EmbeddingModel, embed: true,
		}, timeout)
	}
	if wantsSlot(in.Slots, "tavily") {
		tavilyKey := in.Values.TavilyAPIKey
		if tavilyKey == "" && stored != nil {
			tavilyKey = stored.TavilyAPIKey
		}
		res.Slots.Tavily = r.probeTavilySlot(ctx, tracer, in.Values.TavilyBaseURL, tavilyKey, timeout)
	}
	return res, nil
}

func wantsSlot(slots []string, name string) bool {
	if len(slots) == 0 {
		return true
	}
	for _, s := range slots {
		if s == name {
			return true
		}
	}
	return false
}

func storedKeyFor(stored *Settings, pick func(*Settings) string) string {
	if stored == nil {
		return ""
	}
	return pick(stored)
}

type openaiSlotPlan struct {
	slot         string
	baseURL      string
	submittedKey string
	storedKey    string
	model        string
	embed        bool
}

func (r *Resolver) probeOpenAISlot(ctx context.Context, tracer trace.Tracer, p openaiSlotPlan, timeout time.Duration) *SlotProbeResult {
	ctx, span := tracer.Start(ctx, "validate."+p.slot)
	defer span.End()
	start := time.Now()

	res := &SlotProbeResult{Probed: true}
	finish := func() *SlotProbeResult {
		span.SetAttributes(attribute.String("slot", p.slot), attribute.String("status", res.Status))
		if res.Status != ProbeOK && res.Status != "" {
			span.SetStatus(codes.Error, res.Status)
		}
		logProbeOutcome(p.slot, res, start)
		return res
	}
	if p.baseURL == "" {
		res.Probed = false
		return finish()
	}
	if p.model == "" {
		res.Status = ProbeInvalidConfig
		res.Detail = "model is required for this slot"
		return finish()
	}
	apiKey := p.submittedKey
	if apiKey == "" {
		apiKey = p.storedKey
	}
	if apiKey == "" && !isLoopbackBaseURL(p.baseURL) {
		res.Status = ProbeInvalidConfig
		res.Detail = "api key is required for a non-loopback endpoint"
		return finish()
	}

	capture := &llm.RequestCapture{}
	client, err := openai.NewClientWithCapture(r.clientConfig(p.baseURL, apiKey, p.model), r.tp.Tracer("nyx.llmsettings.validate"), capture)
	if err != nil {
		res.Status = classifyConstructErr(err)
		return finish()
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var probeErr error
	if p.embed {
		probeErr = r.runEmbedProbe(probeCtx, client, p.model, res)
	} else {
		probeErr = r.runChatProbe(probeCtx, client, res)
	}
	res.Status = classifyProbeErr(probeErr)
	res.HTTPStatus, _ = llm.UpstreamStatusOf(probeErr)
	res.Request = capture.Captured()
	span.RecordError(probeErr)
	return finish()
}

func (r *Resolver) runChatProbe(ctx context.Context, client llm.Provider, res *SlotProbeResult) error {
	_, err := client.Chat(ctx, llm.ChatRequest{
		Messages:  []llm.Message{{Role: "user", Content: probeQueryText}},
		MaxTokens: 1,
	}, func(string, *llm.ChatUsage) error { return nil })
	return err
}

func (r *Resolver) runEmbedProbe(ctx context.Context, client llm.Provider, model string, res *SlotProbeResult) error {
	vecs, err := client.Embed(ctx, llm.EmbedRequest{Model: model, Inputs: []string{probeQueryText}})
	if err != nil {
		return err
	}
	if len(vecs) != 1 {
		return ErrEmbeddingWidthMismatch
	}
	res.Detail = fmt.Sprintf("%d dims", len(vecs[0]))
	if len(vecs[0]) != r.base.EmbeddingDimensions {
		res.Detail = fmt.Sprintf("expected %d dims, got %d", r.base.EmbeddingDimensions, len(vecs[0]))
		return ErrEmbeddingWidthMismatch
	}
	return nil
}

func (r *Resolver) probeTavilySlot(ctx context.Context, tracer trace.Tracer, baseURL, apiKey string, timeout time.Duration) *SlotProbeResult {
	ctx, span := tracer.Start(ctx, "validate.tavily")
	defer span.End()
	start := time.Now()

	res := &SlotProbeResult{Probed: true}
	finish := func() *SlotProbeResult {
		span.SetAttributes(attribute.String("slot", "tavily"), attribute.String("status", res.Status))
		if res.Status != ProbeOK && res.Status != "" {
			span.SetStatus(codes.Error, res.Status)
		}
		logProbeOutcome("tavily", res, start)
		return res
	}
	if baseURL == "" {
		res.Probed = false
		return finish()
	}

	payload, _ := json.Marshal(map[string]any{"query": probeQueryText, "max_results": 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(payload))
	if err != nil {
		res.Status = ProbeInvalidConfig
		res.Detail = "base URL is not a valid URL"
		return finish()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("X-Tavily-Access-Mode", "keyless")
	}

	capture := &llm.RequestCapture{}
	httpClient := &http.Client{
		Transport: capture.Wrap(ssrf.NewTransport(r.base.BYOKAllowPrivateURL, "tavily_base_url")),
		Timeout:   timeout,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		res.Request = capture.Captured()
		res.Status = classifyTransportErr(err)
		return finish()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	res.Request = capture.Captured()
	res.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Status = ProbeOK
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Status = ProbeAuthFailed
	case resp.StatusCode == http.StatusTooManyRequests:
		res.Status = ProbeRateLimited
	default:
		res.Status = ProbeUnreachable
	}
	return finish()
}

func classifyConstructErr(err error) string {
	if errors.Is(err, ssrf.ErrPrivateAddress) {
		return ProbeUnreachable
	}
	return ProbeInvalidConfig
}

func classifyProbeErr(err error) string {
	switch {
	case err == nil:
		return ProbeOK
	case errors.Is(err, ErrEmbeddingWidthMismatch):
		return ProbeWidthMismatch
	case errors.Is(err, llm.ErrRateLimited):
		return ProbeRateLimited
	case errors.Is(err, llm.ErrInvalidInput):
		return ProbeInvalidConfig
	case errors.Is(err, llm.ErrContextCanceled):
		return ProbeTimeout
	case errors.Is(err, llm.ErrProviderUnavailable):
		if status, ok := llm.UpstreamStatusOf(err); ok {
			switch {
			case status == http.StatusUnauthorized || status == http.StatusForbidden:
				return ProbeAuthFailed
			case status == http.StatusNotFound:
				return ProbeInvalidModel
			case status == http.StatusBadRequest:
				return ProbeInvalidConfig
			}
		}
		return ProbeUnreachable
	default:
		return ProbeUnreachable
	}
}

func classifyTransportErr(err error) string {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return ProbeTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ProbeTimeout
	}
	if errors.Is(err, ssrf.ErrPrivateAddress) {
		return ProbeUnreachable
	}
	return ProbeUnreachable
}

func (r *Resolver) configProbeTimeout() time.Duration {
	d, err := time.ParseDuration(r.base.LLMConfigProbeTimeout)
	if err != nil || d <= 0 {
		return defaultProbeTTL
	}
	return d
}

func logProbeOutcome(slot string, res *SlotProbeResult, start time.Time) {
	if !res.Probed {
		return
	}
	evt := log.Info()
	if res.Status != ProbeOK {
		evt = log.Warn()
	}
	evt.Str("slot", slot).Str("status", res.Status).Dur("elapsed", time.Since(start)).Msg("llm config probe finished")
}
