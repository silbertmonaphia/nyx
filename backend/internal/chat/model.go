// Package chat is the chat-domain facade. It owns the chat-completion
// request shape the wire exposes, validates incoming messages, and
// streams responses back as Server-Sent Events.
//
// The package follows the same model/service/handler split as the
// feed and user domains. The handler diverges from the others in
// one way: it is mounted directly on chi (not huma), because huma
// v2 has no first-class SSE. The route still runs inside the
// project's standard middleware chain (RequestID → Recoverer →
// Prometheus → Logging → CORS → RateLimit → maxBodyBytes → Auth)
// because RegisterChatRoute mounts it on the same router after the
// huma adapter wraps it. See backend/HUMA.md for the rationale.
//
// Persistence is absent from THIS package: clients send the full
// conversation history with every request and the Go layer keeps no
// state. Chat history is owned by the Python agent (chat_sessions /
// chat_messages); the optional ChatRequest.RunID below is how a
// client names the session it wants to resume, and the session REST
// routes in sessions_proxy.go are how the SPA reads that history
// back. See FUTURE.md §6.
package chat

// Role constants. The wire shape accepts "user" and "assistant"
// from clients; "system" is rejected because the system prompt is
// server-controlled. Keeping the constants in this file (vs.
// importing llm) lets the chat domain own its wire contract while
// the llm package stays unaware of where Messages come from.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// MaxBodyBytes is the per-request cap on the JSON body the handler
// will decode. The router-level maxBodyBytes middleware (1 MiB) is
// the outer bound; this is the chat-specific inner cap so a
// pathological 1 MiB payload of mostly garbage can't reach the
// JSON decoder and burn CPU. 64 KiB comfortably holds the
// configured max history (50 × ~1 KiB average) with headroom for
// longer messages up to the per-message 32 KiB cap.
const MaxBodyBytes = 64 << 10

// ChatRequest is the JSON body the wire decodes. Messages is the
// only required field. Model is accepted in the wire shape for
// forward-compatibility (a future "personas" feature may let
// authenticated users pick from an operator-curated allowlist) but
// is currently IGNORED — the chat service builds the llm.ChatRequest
// from config, not from the client. See service.go for the
// rejection path.
//
// Field names use JSON tags, not huma tags, because this route is
// hand-decoded (chi + json.Decoder) — huma is bypassed for SSE.
type ChatRequest struct {
	Messages []Message `json:"messages"`
	Model    *string   `json:"model,omitempty"`
	// RAG, when true, asks the server to ground the answer in the
	// authenticated user's feeds via top-K semantic retrieval.
	// Omitted on the wire when false (json:",omitempty") so today's
	// non-RAG requests look identical to before this field landed.
	// DisallowUnknownFields on the handler decoder rejects typos
	// like "ragged" with 400.
	RAG bool `json:"rag,omitempty"`
	// RunID, when supplied, is the session the client wants the
	// agent to resume. The handler forwards it unchanged in the
	// proxied /agent/run body; the Python service owns validation (a
	// non-uuid4 comes back as its own 400 naming the field). Omitted
	// on the wire when nil so a fresh-run request is byte-identical
	// to the pre-session body. The Go layer MUST NOT mint, rewrite,
	// or validate run ids — the SPA (via the sidebar) and the Python
	// service are the only parties that decide session identity.
	//
	// The pointer type is what makes "absent" distinguishable from
	// "empty string": a client that sends run_id:"" is forwarded
	// as-is (Python rejects it), rather than being silently dropped
	// like a bare string with omitempty would.
	RunID *string `json:"run_id,omitempty"`
}

// Message is one turn. Role is restricted to RoleUser / RoleAssistant
// from clients; RoleSystem from a client is rejected at the service
// layer so the server-controlled system prompt is the only system
// voice the upstream LLM ever hears.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
