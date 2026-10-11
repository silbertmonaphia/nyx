package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"nyx/internal/llm"
	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// Handler exposes the chat endpoint. The struct is kept small — it
// only carries the Service — so tests can construct it without
// spinning up the full main.go wiring. RegisterChatRoute is the
// single mount point; see backend/HUMA.md for why this route is on
// chi and not huma.
type Handler struct {
	service *Service
}

// NewHandler wraps a Service for HTTP. The service is what does the
// actual work; the handler's only job is to translate the wire
// format (JSON in, SSE out).
func NewHandler(s *Service) *Handler {
	return &Handler{service: s}
}

// RegisterChatRoute installs POST /api/chat on the supplied chi
// router. The route runs inside the project's standard middleware
// chain (RequestID → Recoverer → Logging → CORS → RateLimit →
// maxBodyBytes) because chi's router.Use applies to every
// subsequently-mounted handler, and RegisterChatRoute is called
// after the global middleware in main.go.
//
// Middleware on this route specifically:
//   - NewAuth: bearer JWT validation; stamps user ID on the context.
//   - userLimiter: per-user 5-streams/min bucket (FUTURE.md §6).
//     May be nil to disable the limiter (useful for tests).
//
// The route is intentionally NOT registered with huma — the
// response is a stream of unknown length and JSON schema, and
// huma's body marshaller would force the whole response into a
// single in-memory buffer. See backend/HUMA.md for the full
// rationale.
func RegisterChatRoute(router chi.Router, h *Handler, tokens auth.TokenService, userLimiter *UserRateLimiter) {
	if h == nil {
		panic("chat.RegisterChatRoute: handler is required")
	}
	router.Route("/api/chat", func(r chi.Router) {
		r.Use(middleware.NewAuth(tokens))
		if userLimiter != nil {
			r.Use(userLimiter.Middleware)
		}
		r.Post("/", h.stream)
	})
}

// stream is the SSE handler. Lifecycle:
//
//  1. Decode the JSON body (cap at MaxBodyBytes).
//  2. Pre-stream validation (returns 400 envelope before SSE bytes).
//  3. Dispatch to streamProxy when IsAgentEnabled(); otherwise to
//     streamLegacy. Both functions own the rest of the SSE lifecycle.
//  4. (Legacy) write SSE headers, run service.Chat, emit done+[DONE].
//  5. (Proxy) write SSE headers, drain Python agent SSE body byte-for-byte.
//
// Validation errors (400) are written as the standard JSON envelope
// BEFORE any SSE bytes — a client that hits a 400 sees the same
// envelope shape every other endpoint uses, and the SPA's response
// interceptor handles it without an SSE-parser special case.
//
// Upstream errors after the stream has started emit one trailing
// `event: error\ndata: {"error":"<safe>","request_id":"..."}\n\n`
// frame followed by `[DONE]`. The HTTP status is already 200 by the
// time the error fires (we sent the headers), so the SPA keys on the
// event line, not the status. This is the standard SSE "partial
// success" pattern; the alternative (rewinding the status) would
// require buffering which defeats the whole point of streaming.
//
// Path dispatch: when the service is configured with the agent proxy
// (AGENT_ENABLED=true), the handler routes through streamProxy;
// otherwise it routes through streamLegacy. The legacy path's wire
// envelope is byte-for-byte unchanged from before the agent-langgraph
// swap (see openspec/changes/agent-langgraph/specs/chat-agent-proxy/
// spec.md requirement "Single-turn fallback parity preserved").
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	// Step 1: decode. MaxBodyBytes bounds the in-memory JSON parse;
	// the router-level maxBodyBytes (1 MiB) is the outer cap that
	// fires first on truly huge bodies.
	var req ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		api.WriteError(w, r, http.StatusBadRequest, "Invalid chat request", err.Error())
		return
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		// Reject trailing JSON after the request body — defends
		// against a malformed client that sends two top-level
		// objects.
		api.WriteError(w, r, http.StatusBadRequest, "Invalid chat request", "unexpected extra JSON")
		return
	}

	// Step 2: pre-stream validation. Runs BEFORE we write any SSE
	// headers so a bad request gets the standard 400 JSON envelope
	// rather than a mid-stream event:error frame (which the SPA
	// would have to special-case because it's already in SSE-mode).
	if err := h.service.Validate(&req); err != nil {
		api.WriteError(w, r, http.StatusBadRequest, "Invalid chat input", err.Error())
		return
	}

	// Step 2.5: dispatch to the agent proxy when configured. The
	// dispatch is mutually exclusive with the legacy path — the
	// proxy takes over the entire SSE lifecycle.
	if h.service.IsAgentEnabled() {
		h.streamProxy(w, r, req)
		return
	}

	// Step 3: SSE headers. Cache-Control + Connection are advisory
	// (Go's http server manages Connection itself for HTTP/1.1)
	// but harmless and required by some proxies. X-Accel-Buffering
	// is the nginx-specific kill-switch for response buffering.
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Step 4: flush-on-write. Every SSE frame is followed by
	// Flush() so the chunk reaches the client immediately. Without
	// this, chi's WrapResponseWriter (in Prometheus + Logging
	// middleware) would buffer the response until the handler
	// returns.
	fw := &flushingWriter{w: w}

	streamLegacy(fw, r, req, h.service)
}

// streamLegacy drives today's single-turn path. It is byte-for-byte
// the same code as the pre-chunk-2 stream body — the SSE envelope
// (event: <name>\ndata: <json>\n\n for delta/note/error/done; bare
// data: [DONE]\n\n as the trailing sentinel) is preserved by
// reference, not by copy, so any change in legacy behaviour would
// surface as a regression in handler_test.go.
//
// Extracted from the pre-chunk-2 stream() body so the additive
// proxy path lives in streamProxy without disturbing this surface.
func streamLegacy(fw *flushingWriter, r *http.Request, req ChatRequest, svc *Service) {
	var (
		finalUsage *llm.ChatUsage
		writeErr   error
	)
	_, writeErr = svc.Chat(r.Context(), req,
		func(delta string, finalUsageChunk *llm.ChatUsage) error {
			// Heartbeat check: if the client has disconnected, bail
			// before doing any I/O. The provider's recv loop will also
			// notice on its next read, but this short-circuits one
			// chunk of work.
			if err := r.Context().Err(); err != nil {
				return err
			}
			// Final usage chunk — nothing to write inline; the handler
			// emits the terminal "done" event after Chat returns.
			if finalUsageChunk != nil {
				finalUsage = finalUsageChunk
				return nil
			}
			if err := writeSSEEvent(fw, "delta", map[string]string{"delta": delta}); err != nil {
				return err
			}
			return fw.Flush()
		},
		// onNote is fired by llm.Router when it fails over from one
		// upstream to another; the handler renders the note as an SSE
		// `event: note` frame so the SPA can show a "continued on …"
		// hint before the secondary's first delta lands. Other
		// providers ignore the callback; passing nil would also work
		// but the router wants to know we noticed.
		func(text, provider string) error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			if err := writeSSEEvent(fw, "note", map[string]string{
				"text":     text,
				"provider": provider,
			}); err != nil {
				return err
			}
			return fw.Flush()
		})

	// Terminal frame.
	if writeErr != nil {
		// Distinguish client-cancel from provider failure: only the
		// former gets the "client closed request" wire message.
		// Everything else funnels through api.ClassifyAndLog so the
		// safeDetail (static literal) is what reaches the client.
		safeMsg := "Chat failed"
		if errors.Is(writeErr, llm.ErrContextCanceled) || errors.Is(writeErr, context.Canceled) {
			safeMsg = "Client closed request"
		}
		_ = writeSSEEvent(fw, "error", map[string]string{
			"error":      safeMsg,
			"request_id": reqctx.RequestIDFromContext(r.Context()),
		})
		_ = fw.Flush()
		api.ClassifyAndLog(r.Context(), writeErr, safeMsg)
		_ = writeSSEData(fw, "[DONE]")
		_ = fw.Flush()
		return
	}

	// Happy-path terminal: a `done` event carrying usage (when the
	// upstream emitted it) and the standard `[DONE]` sentinel that
	// closes the SSE stream per the spec.
	_ = writeSSEEvent(fw, "done", map[string]any{
		"usage": usageToMap(finalUsage),
	})
	if err := fw.Flush(); err != nil {
		return
	}
	_ = writeSSEData(fw, "[DONE]")
	_ = fw.Flush()
}

// streamProxy drives the AGENT_ENABLED=true path. It opens the SSE
// envelope, calls the Python orchestrator via AgentProxy, and copies
// the upstream response body to the chat response writer BYTE-FOR-
// BYTE per the spec requirement "Wire envelope is byte-for-byte
// identical" (openspec/changes/agent-langgraph/specs/chat-agent-proxy/
// spec.md).
//
// Lifecycle:
//
//  1. Auth fence — reqctx.UserIDFromContext must be non-zero. A
//     zero userID with SSE headers already flushed would force a
//     200-then-error frame, which the SPA would have to special-
//     case from a real 401.
//  2. Write SSE headers (Content-Type, Cache-Control, Connection,
//     X-Accel-Buffering). Status 200 because the SSE envelope is
//     opened regardless of upstream outcome.
//  3. Call AgentProxy.Run with the verified user id (forwarded
//     as X-Nyx-User-Id) and the user's Authorization header
//     (passthrough to /mcp + /internal/rag/retrieve). On
//     ErrAgentUnavailable → emit error{code:"agent_unavailable"}
//     + done + terminator and return. On HTTP non-5xx → forward
//     body verbatim.
//  4. Read resp.Body in a loop, copy bytes straight to fw with
//     Flush after each chunk. Client disconnect surfaces as a
//     write error which we log + abandon.
//
// This function OWNS the SSE lifecycle from headers through the
// final frame. The legacy streamLegacy helper is not called. On
// failure, the handler emits one typed error frame + done +
// terminator; the SPA's SSE parser sees a consistent failure
// shape regardless of mode.
func (h *Handler) streamProxy(w http.ResponseWriter, r *http.Request, req ChatRequest) {
	// Defensive auth check. The auth middleware already 401s
	// unauthenticated requests, but main.go could in theory wire
	// this route without middleware (admin subrouter, internal
	// health-check backends), so the handler double-checks. A
	// zero userID with SSE headers already flushed would force
	// us into a 200-then-error frame, which the SPA would have
	// to special-case from a real 401.
	userID := reqctx.UserIDFromContext(r.Context())
	if userID == 0 {
		api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
		return
	}

	// Write the SSE headers. Same shape as the legacy path so a
	// proxy / CDN that already keys on Content-Type does the
	// right thing. The downstream consumer can be a legacy SPA
	// (ignoring unknown frame names) or a new one (rendering
	// per-step progress from the Python orchestrator's stream).
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fw := &flushingWriter{w: w}

	// Forward the user's Authorization header verbatim as a
	// passthrough to /mcp + /internal/rag/retrieve (Go re-verifies
	// on those endpoints). The Python agent itself does NOT verify
	// it — the network boundary is the trust boundary at this hop.
	// The Go proxy MUST NOT mint a service-to-service token, MUST
	// NOT re-validate, MUST NOT log the value.
	bearer := r.Header.Get("Authorization")
	resp, err := h.service.Proxy().Run(r.Context(), req, userID, bearer)
	if err != nil {
		// Fail closed per the spec requirement "Python service
		// unavailable fails closed": emit error + done + terminator
		// and return. The HTTP status is already 200 (SSE envelope
		// opened) so the SPA's SSE parser sees the typed event
		// rather than a non-200 response.
		emitAgentUnavailable(fw, r.Context())
		api.ClassifyAndLog(r.Context(), err, "Agent service unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy the upstream response body byte-for-byte. Each read
	// is flushed so the SPA sees SSE chunks in real time rather
	// than after the upstream closes its TCP connection.
	if err := copyAndFlush(fw, resp.Body); err != nil {
		// Log the read/write failure but DO NOT translate to
		// an SSE frame — the Python stream may already have
		// emitted its own terminator; appending another typed
		// error frame on top would corrupt the wire contract.
		// A client disconnect is the common case (broken pipe
		// on Write) and is not an operator-visible error.
		if !errors.Is(err, context.Canceled) && r.Context().Err() == nil {
			api.ClassifyAndLog(r.Context(), err, "Agent SSE stream failed")
		}
		return
	}
}

// emitAgentUnavailable writes the fail-closed SSE envelope to fw
// per the spec scenario "Python service unavailable fails closed":
// one error frame with code:"agent_unavailable", followed by done
// + terminator. The static literals stay safe to ship (no err.Error()
// in the response details); the underlying error is logged via
// api.ClassifyAndLog by the caller.
func emitAgentUnavailable(fw *flushingWriter, ctx context.Context) {
	_ = writeSSEEvent(fw, "error", map[string]string{
		"error":      "Agent service unavailable",
		"code":       "agent_unavailable",
		"request_id": reqctx.RequestIDFromContext(ctx),
	})
	_ = fw.Flush()
	_ = writeSSEEvent(fw, "done", map[string]any{})
	_ = fw.Flush()
	_ = writeSSEEvent(fw, "terminator", map[string]string{
		"reason": "agent_unavailable",
	})
	_ = fw.Flush()
}

// copyAndFlush reads from src in 4 KiB chunks and writes each chunk
// to dst, flushing after every write so the SSE bytes reach the
// client immediately rather than buffering in chi's
// WrapResponseWriter. Returns the first non-EOF error.
func copyAndFlush(dst *flushingWriter, src io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if ferr := dst.Flush(); ferr != nil {
				return ferr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// usageToMap projects *llm.ChatUsage into a wire-safe map. nil
// usage produces an empty map rather than nil so the JSON encoder
// emits `"usage": {}` instead of `"usage": null` — clients don't
// have to special-case the "no usage reported" branch.
func usageToMap(u *llm.ChatUsage) map[string]int {
	if u == nil {
		return map[string]int{}
	}
	return map[string]int{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

// flushingWriter wraps an http.ResponseWriter so the handler can
// call an explicit Flush() after every SSE frame. This defeats
// chi's WrapResponseWriter buffering on the streaming path; without
// it, SSE chunks accumulate until the handler returns and the SPA
// never sees a delta until the model is done.
//
// Flush errors are propagated to the caller — they typically mean
// the client disconnected (broken pipe), which is the signal to
// stop reading from the provider.
type flushingWriter struct {
	w http.ResponseWriter
}

func (f *flushingWriter) Write(p []byte) (int, error) {
	return f.w.Write(p)
}

// Flush delegates to http.NewResponseController, the stdlib-blessed
// way to access Flusher on writers that may have been wrapped
// (chi's WrapResponseWriter, middleware-added content encoders,
// etc.). NewResponseController always returns a non-nil controller
// for any http.ResponseWriter; the controller's Flush returns an
// error when the underlying writer doesn't implement http.Flusher
// (which would mean our streaming path can't work at all).
func (f *flushingWriter) Flush() error {
	return http.NewResponseController(f.w).Flush()
}

// writeSSEEvent writes one event frame: `event: <name>\ndata: <json>\n\n`.
// The data payload is JSON-marshalled; callers pass a map or struct
// for the field shape. Trailing flush is the caller's job — the
// SSE wire format requires the frame terminator (blank line) but
// the kernel will buffer until either a flush or the connection
// closes.
func writeSSEEvent(w io.Writer, name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("chat: marshal SSE event: %w", err)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	return err
}

// writeSSEData writes one bare data frame: `data: <text>\n\n`. Used
// for the trailing `[DONE]` sentinel where no event name is needed.
//
//nolint:unparam // the parameter is parked for future bare-data frames (comment payloads, ping); today's callers all pass doneSentinel
func writeSSEData(w io.Writer, data string) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
