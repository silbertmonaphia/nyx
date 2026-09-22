package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ProviderSlot is one half of the Router: the concrete llm.Provider
// plus the URL + bearer key needed to probe it. The Name is the
// wire literal ("openai" or "vllm") so the router can serialise it
// into the event:note SSE frame the client receives on failover.
//
// ProbeKey is the bearer token the probe sends as Authorization.
// Empty omits the header — matches vLLM started without --api-key
// (the data plane honours the same convention; boot-time config
// validation enforces provider-specific key requirements).
type ProviderSlot struct {
	Name     string
	Provider Provider
	BaseURL  string
	ProbeKey string
}

// Router wraps two ProviderSlots so a wedged upstream surfaces as
// a transparent failover instead of a 502. When secondary is nil
// the Router is a pure passthrough with zero per-request probe
// cost; when both slots are present the Router probes the primary
// (GET {base}/v1/models) and, on probe failure OR mid-stream
// provider error before any usage chunk was emitted, replays the
// request once on the secondary with the partial reply appended.
//
// Probing a public /v1/models endpoint is the canonical OpenAI
// liveness check — both OpenAI proper and vLLM honour it. A
// network error or 5xx response is treated as "this slot is sick,
// use the other". The probe is bound to probeTimeout (2s default)
// so a slow DNS lookup cannot blow out the request budget.
type Router struct {
	primary      ProviderSlot
	secondary    *ProviderSlot // nil → single-provider passthrough
	probeHTTP    *http.Client
	tracer       trace.Tracer
	probeTimeout time.Duration
}

// NewRouter wires a Router around two slots. secondary may be nil
// for the single-provider passthrough mode used by operators who
// haven't opted in to failover. probeTimeout is the per-request
// probe ceiling; callers should pre-validate it via
// config.validateDurations before reaching the wiring site.
func NewRouter(primary ProviderSlot, secondary *ProviderSlot, tracer trace.Tracer, probeTimeout time.Duration) *Router {
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	return &Router{
		primary:      primary,
		secondary:    secondary,
		probeHTTP:    &http.Client{Timeout: probeTimeout},
		tracer:       tracer,
		probeTimeout: probeTimeout,
	}
}

// Chat implements llm.Provider. Single-provider deployments skip
// the probe entirely (a pure passthrough); dual-provider deployments
// probe the primary, run on whichever slot the probe picked, and
// fail over to the other slot on ErrProviderUnavailable (and only
// that sentinel — ctx cancels, 429s, and post-usage errors do NOT
// failover). One failover budget per request — both-down returns
// ErrAllProvidersFailed wrapping ErrProviderUnavailable so existing
// error-chain checks (errors.Is(err, llm.ErrProviderUnavailable))
// continue to work.
func (r *Router) Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error) {
	ctx, span := r.tracer.Start(ctx, "llm.router.chat",
		trace.WithAttributes(
			attribute.String("llm.router.primary", r.primary.Name),
			attribute.Bool("llm.router.has_secondary", r.secondary != nil),
		),
	)
	defer span.End()

	// Single-provider passthrough — no probe, no wrapping of
	// onDelta. Preserves zero-latency behaviour for operators who
	// haven't configured a fallback.
	if r.secondary == nil {
		return r.primary.Provider.Chat(ctx, req, onDelta)
	}

	slot, probeFailed := r.selectSlot(ctx, span)
	res := r.callSlot(ctx, slot, req, onDelta)
	if res.err == nil {
		span.SetStatus(codes.Ok, "")
		return res.usage, nil
	}
	if res.usage != nil {
		recordUsage(span, res.usage)
	}

	if !shouldFailover(res.err, res.terminalEmitted) {
		span.RecordError(res.err)
		return res.usage, res.err
	}

	// If we already failed over on the probe step, the secondary is
	// our only call — a second failure returns ErrAllProvidersFailed
	// without retrying the primary (we'd be ping-ponging probes).
	if probeFailed {
		span.RecordError(res.err)
		return res.usage, errors.Join(ErrAllProvidersFailed, res.err)
	}

	secondarySlot := r.secondaryByName(slot.Name)
	return r.failoverToSecondary(ctx, span, req, res.partial, secondarySlot, onDelta)
}

// selectSlot probes the primary and returns the slot to call.
// probeFailed is true when the primary probe failed and slot is
// already the secondary.
func (r *Router) selectSlot(ctx context.Context, span trace.Span) (ProviderSlot, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	if err := r.probe(probeCtx, r.primary); err != nil {
		span.SetAttributes(attribute.Bool("llm.router.probe_failed", true))
		return *r.secondary, true
	}
	return r.primary, false
}

// callSlotResult bundles the four pieces of state the router
// needs to make a failover decision. Bundling keeps the public
// Chat function under the cyclomatic-complexity budget and makes
// the data flow obvious.
type callSlotResult struct {
	usage           *ChatUsage
	err             error
	partial         string
	terminalEmitted bool
}

// callSlot invokes a provider Chat with the partial-accumulating
// wrapper. terminalEmitted flips on the usage callback and guards
// against failover on already-completed responses (the second
// provider would be replaying a brand-new conversation
// masquerading as a continuation).
func (r *Router) callSlot(ctx context.Context, slot ProviderSlot, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) callSlotResult {
	var partial strings.Builder
	var terminalEmitted bool
	inner := func(delta string, usage *ChatUsage) error {
		if usage != nil {
			terminalEmitted = true
			return onDelta("", usage)
		}
		partial.WriteString(delta)
		return onDelta(delta, nil)
	}
	usage, err := slot.Provider.Chat(ctx, req, inner)
	return callSlotResult{usage: usage, err: err, partial: partial.String(), terminalEmitted: terminalEmitted}
}

// shouldFailover returns true when the router should retry on the
// secondary slot. Bails out on ctx cancel, rate limit, terminal
// usage already emitted, and any error that isn't
// ErrProviderUnavailable — those conditions are unlikely to be
// upstream-specific.
func shouldFailover(err error, terminalEmitted bool) bool {
	if errors.Is(err, ErrContextCanceled) ||
		errors.Is(err, ErrRateLimited) ||
		terminalEmitted ||
		!errors.Is(err, ErrProviderUnavailable) {
		return false
	}
	return true
}

// secondaryByName returns the OTHER slot. If name matches the
// primary, return the secondary; otherwise return the primary
// (handles both "ran on primary" and "ran on secondary because
// probe failed" cases).
func (r *Router) secondaryByName(name string) ProviderSlot {
	if name == r.primary.Name {
		return *r.secondary
	}
	return r.primary
}

// failoverToSecondary replays the request on the secondary with
// the partial reply appended, fires OnNote if set, and returns the
// secondary's result. Both-down returns ErrAllProvidersFailed
// joined with ErrProviderUnavailable so existing handlers' error
// checks keep working.
func (r *Router) failoverToSecondary(ctx context.Context, span trace.Span, req ChatRequest, partial string, secondary ProviderSlot, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error) {
	// Build the replay request: same conversation + a synthetic
	// assistant turn carrying the partial reply. The system prompt
	// is preserved at index 0 because the chat service prepended it
	// before we saw the request.
	req2 := req
	req2.Messages = make([]Message, 0, len(req.Messages)+1)
	req2.Messages = append(req2.Messages, req.Messages...)
	req2.Messages = append(req2.Messages, Message{Role: "assistant", Content: partial})

	// Notify the client BEFORE the secondary starts streaming so
	// the SSE parser can render a "continued on {provider}" note
	// before any new content arrives. A non-nil return from the
	// callback (typically: SSE writer dead, client disconnected)
	// aborts the failover.
	if req.OnNote != nil {
		if nerr := req.OnNote(fmt.Sprintf("[continued on %s]", secondary.Name), secondary.Name); nerr != nil {
			span.RecordError(nerr)
			return nil, nerr
		}
	}

	span.SetAttributes(attribute.Bool("llm.router.failover", true))

	usage2, err2 := secondary.Provider.Chat(ctx, req2, onDelta)
	if err2 != nil {
		span.RecordError(err2)
		span.SetStatus(codes.Error, "both providers failed")
		return usage2, errors.Join(ErrAllProvidersFailed, ErrProviderUnavailable)
	}
	if usage2 != nil {
		recordUsage(span, usage2)
	}
	span.SetStatus(codes.Ok, "")
	return usage2, nil
}

// recordUsage stamps prompt/completion/total counts onto the
// active span for ops correlation. Extracted so the success /
// failover paths share one definition.
func recordUsage(span trace.Span, u *ChatUsage) {
	span.SetAttributes(
		attribute.Int("llm.prompt_tokens", u.PromptTokens),
		attribute.Int("llm.completion_tokens", u.CompletionTokens),
		attribute.Int("llm.total_tokens", u.TotalTokens),
	)
}

// Embed implements llm.Provider on the router so a single
// llm.Provider instance can serve both chat and embeddings for the
// rag package, with failover for free. Single-provider deployments
// skip the failover branch entirely; dual-provider deployments try
// the primary first and fall over to the secondary on
// ErrProviderUnavailable only — same selectivity rule as the chat
// path's shouldFailover, because rate-limit / context-cancel errors
// are not upstream-specific.
//
// NO /v1/models probe for embeddings. The chat probe exists
// because streaming pre-empts the upstream before any byte reaches
// us; an embedding call is one synchronous request and the call
// result IS the liveness signal. A probe round-trip before each
// embedding would double latency with no benefit.
func (r *Router) Embed(ctx context.Context, req EmbedRequest) ([][]float32, error) {
	ctx, span := r.tracer.Start(ctx, "llm.router.embed",
		trace.WithAttributes(
			attribute.String("llm.router.primary", r.primary.Name),
			attribute.Bool("llm.router.has_secondary", r.secondary != nil),
			attribute.Int("llm.inputs", len(req.Inputs)),
		),
	)
	defer span.End()

	if r.secondary == nil {
		return r.primary.Provider.Embed(ctx, req)
	}

	vecs, err := r.primary.Provider.Embed(ctx, req)
	if err == nil {
		span.SetStatus(codes.Ok, "")
		return vecs, nil
	}
	// Only ErrProviderUnavailable triggers failover — the same
	// rule as shouldFailover for chat. We check the chain with
	// errors.Is so a wrapped error (fmt.Errorf("...: %w", ...))
	// still routes correctly.
	if !errors.Is(err, ErrProviderUnavailable) {
		span.RecordError(err)
		return vecs, err
	}

	span.SetAttributes(attribute.Bool("llm.router.failover", true))
	vecs2, err2 := r.secondary.Provider.Embed(ctx, req)
	if err2 != nil {
		span.RecordError(err2)
		// Both-down mirrors the chat path: join
		// ErrAllProvidersFailed with the primary's error so a
		// caller that walks the chain with errors.Is still sees
		// ErrProviderUnavailable.
		return vecs2, errors.Join(ErrAllProvidersFailed, ErrProviderUnavailable)
	}
	span.SetStatus(codes.Ok, "")
	return vecs2, nil
}

// probe issues GET {base}/v1/models with a hard deadline. Any
// non-2xx, network error, DNS failure, or ctx deadline → error.
// The probe reuses boot-time SSRF validation: both URLs already
// passed validateLLMBaseURL, so per-dial IP-class re-checks would
// be redundant work for the same policy.
//
// Tracer span: llm.router.probe with attribute llm.slot.
// probe must never fail the request — it only influences which
// slot the call runs on. Probe timeouts are logged as warn-level
// attributes on the parent chat.service span via the underlying
// span, not the router span.
func (r *Router) probe(ctx context.Context, slot ProviderSlot) error {
	_, span := r.tracer.Start(ctx, "llm.router.probe",
		trace.WithAttributes(
			attribute.String("llm.slot", slot.Name),
			attribute.String("llm.url", slot.BaseURL),
		),
	)
	defer span.End()

	base := strings.TrimRight(slot.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		span.RecordError(err)
		return err
	}
	if slot.ProbeKey != "" {
		req.Header.Set("Authorization", "Bearer "+slot.ProbeKey)
	}
	resp, err := r.probeHTTP.Do(req)
	if err != nil {
		span.RecordError(err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("probe %s: status %d", slot.Name, resp.StatusCode)
		span.RecordError(err)
		return err
	}
	return nil
}
