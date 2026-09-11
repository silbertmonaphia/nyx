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
//  1. Decode the JSON body (cap at MaxBodyBytes).
//  2. Set SSE headers + X-Accel-Buffering: no (defeats nginx buffering).
//  3. Wrap w in a flushingWriter so every chunk hits the wire.
//  4. Call service.Chat, forwarding each delta as `event: delta\ndata: {"delta":"..."}\n\n`.
//  5. Emit `event: done\ndata: {"usage":{...}}\n\n` then `data: [DONE]\n\n`.
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

	// Step 5: stream. The callback is invoked once per content
	// chunk AND once for the final usage chunk (delta="",
	// finalUsage != nil). The latter is our signal to write the
	// terminal "done" frame.
	var (
		finalUsage *llm.ChatUsage
		writeErr   error
	)
	_, writeErr = h.service.Chat(r.Context(), req, func(delta string, finalUsageChunk *llm.ChatUsage) error {
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
	})

	// Step 6: terminal frame.
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
func writeSSEData(w io.Writer, data string) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
