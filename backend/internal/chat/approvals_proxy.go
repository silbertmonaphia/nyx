package chat

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// This file owns POST /api/chat/approvals — the SPA's decision hop for
// the agent's confirmation gate (openspec/changes/agent-feed-tools,
// design D7). When the Python orchestrator pauses a mutating tool call
// it emits a confirm_request SSE frame; the user's approve/deny lands
// here and is brokered to the waiting run through the agent's
// /agent/approvals endpoint.
//
// The hop is deliberately dumb. The handler forwards the decision body
// opaquely and relays the upstream status + body verbatim: it MUST NOT
// parse the body, inspect the decision, rewrite the payload, translate
// the upstream answer, or synthesize a result of its own. Interpreting
// or shortcutting a decision in Go would create a second source of
// truth for an authorization outcome — the Python service owns
// approval semantics (ownership checks, first-wins idempotency,
// expiry), and this layer is a transport hop.
//
// Trust model is identical to the other two agent hops: Python does
// NOT verify the JWT; the shared AGENT_SERVICE_TOKEN authenticates
// the Go caller, which stamps the verified user id as
// X-Nyx-User-Id; the caller's Authorization header rides along as a
// passthrough for the Python service's own bearer-scoped calls
// (e.g. /mcp). Neither credential value may be logged.
//
// Fail-closed is the whole point of the endpoint: an unreachable or
// disabled agent answers an error envelope, and an approval is never
// auto-granted, never defaulted to "approve", and never served from a
// fallback path. A decision that does not reach the broker leaves the
// waiting run to expire its approval TTL, which the Python side closes
// as a denial — the safe direction.

// approvalsPath is the upstream endpoint the Python agent service
// mounts the decision broker on. The Go route and the upstream path
// deliberately differ in prefix (/api/chat/approvals vs
// /agent/approvals): the /agent/* space is service-to-service and
// never linked from the SPA.
const approvalsPath = "/agent/approvals"

// Static response literals for the approvals hop. Every one is safe to
// ship: they describe the failure in user-facing terms and carry no
// error text, no upstream detail, and no decision semantics. The
// underlying error goes to the log via api.ClassifyAndLog.
const (
	// 503 — agent mode is off (AGENT_ENABLED=false / no
	// PYTHON_AGENT_URL). There is no broker to forward to; that is a
	// server-side capability gap, not a client error. Status chosen to
	// mirror the session routes so the SPA has one "agent unavailable"
	// branch.
	approvalsDisabledMessage = "Chat approvals are unavailable"
	approvalsDisabledDetail  = "The agent service is disabled"

	// 502 — agent mode is on but the upstream could not be reached
	// (connection refused / DNS failure / timeout).
	approvalsUnreachableMessage = "Agent approvals unavailable"
	approvalsUnreachableDetail  = "The agent service could not be reached"
)

// RegisterApprovalsRoute mounts POST /api/chat/approvals on the
// supplied chi router, mirroring chat.RegisterChatRoute's middleware
// posture:
//
//   - middleware.NewAuth runs FIRST, so an unauthenticated decision is
//     401ed at the edge and is NEVER forwarded to the Python service
//     (spec scenario "Missing or invalid JWT is rejected at the edge").
//   - the per-user limiter (when non-nil) runs next, same token-bucket
//     type the chat stream and MCP surfaces use. The caller supplies a
//     dedicated bucket instance: approval decisions are cheap
//     short-lived JSON hops (the MCP sizing philosophy), not held
//     streams, so sharing the 5/min stream bucket would let a chatty
//     stream session 429 the user's own approval and turn it into a
//     timeout-denial.
//
// The route is registered unconditionally (like the session routes);
// when the agent is disabled the handler answers 503 rather than the
// route 404ing, so the SPA's failure branch is the same in both modes.
//
// A chi note: this registers an absolute static path under /api/chat
// on the same tree RegisterChatRoute mounts its /api/chat subrouter
// on. chi matches static children before catch-all children (tree.go
// iterates node types in order, ntStatic before ntCatchAll), so the
// subrouter's /* mount cannot swallow this route regardless of
// registration order. TestApprovals_ChatRoutesStillRouted pins the
// coexistence.
func RegisterApprovalsRoute(router chi.Router, h *Handler, tokens auth.TokenService, limiter *UserRateLimiter) {
	if h == nil {
		panic("chat.RegisterApprovalsRoute: handler is required")
	}
	chain := []func(http.Handler) http.Handler{middleware.NewAuth(tokens)}
	if limiter != nil {
		chain = append(chain, limiter.Middleware)
	}
	router.With(chain...).Post("/api/chat/approvals", h.approvals)
}

// approvals forwards one approval decision to the Python agent and
// relays its answer verbatim. Lifecycle:
//
//  1. Auth fence — a zero user id means the route was mounted without
//     the auth middleware. 401 rather than proxying with
//     "X-Nyx-User-Id: 0", which Python would treat as a foreign (and
//     possibly valid-looking) caller.
//  2. Availability gate — no forwarder configured (AGENT_ENABLED=false)
//     → 503. No upstream call is attempted, and nothing that looks
//     like a success is ever written.
//  3. Forward POST /agent/approvals with the raw body, the verified
//     user id in X-Nyx-User-Id, and the caller's Authorization header
//     as a passthrough. The body is streamed straight through — it is
//     already bounded by the router-level 1 MiB maxBodyBytes
//     middleware, and buffering or decoding it here would be the first
//     step toward interpreting a decision Go does not own.
//  4. Transport failure (refused / DNS / timeout / canceled ctx) →
//     502 with a STATIC safe detail via api.ClassifyAndLog. Fail
//     closed: no decision is recorded anywhere, no fallback path
//     exists.
//  5. Otherwise relay the upstream status, content type, and body
//     byte-for-byte — including 4xx/5xx bodies (the agent's typed 404
//     for unknown/expired/foreign approval ids is the client-facing
//     contract; the Go layer must not wrap or remap it).
//
// The forwarder is the JSON proxy derived from the service's
// AgentProxy (newSessionsProxyFromService): same base URL, transport,
// service token, and forwarding contract (header policy, verbatim
// relay) already pinned by the session-route tests. Reusing it keeps
// every agent hop on one configuration surface — AGENT_ENABLED +
// PYTHON_AGENT_URL + AGENT_SERVICE_TOKEN — so the approvals hop can
// never be half-configured.
func (h *Handler) approvals(w http.ResponseWriter, r *http.Request) {
	userID := reqctx.UserIDFromContext(r.Context())
	if userID == 0 {
		api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
		return
	}
	if h.sessions == nil || h.service == nil || !h.service.IsAgentEnabled() {
		api.WriteError(w, r, http.StatusServiceUnavailable, approvalsDisabledMessage, approvalsDisabledDetail)
		return
	}

	resp, err := h.sessions.Forward(r.Context(), sessionProxyRequest{
		method:      http.MethodPost,
		path:        approvalsPath,
		body:        r.Body,
		contentType: r.Header.Get("Content-Type"),
		userID:      userID,
		bearer:      r.Header.Get("Authorization"),
	})
	if err != nil {
		if r.Context().Err() != nil {
			// The client hung up (or the server is shutting down).
			// There is nobody to write the 502 to, and logging a
			// canceled context as a proxy failure would be noise.
			return
		}
		api.WriteError(w, r, http.StatusBadGateway, approvalsUnreachableMessage,
			api.ClassifyAndLog(r.Context(), err, approvalsUnreachableDetail))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	writeSessionResponse(w, r, resp)
}
