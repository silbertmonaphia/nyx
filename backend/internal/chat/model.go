// Package chat is the chat-domain facade. It owns the chat-completion
// request shape the wire exposes, validates incoming messages, and
// streams responses back as Server-Sent Events.
//
// The package follows the same model/service/handler split as the
// movie and user domains. The handler diverges from the others in
// one way: it is mounted directly on chi (not huma), because huma
// v2 has no first-class SSE. The route still runs inside the
// project's standard middleware chain (RequestID → Recoverer →
// Prometheus → Logging → CORS → RateLimit → maxBodyBytes → Auth)
// because RegisterChatRoute mounts it on the same router after the
// huma adapter wraps it. See backend/HUMA.md for the rationale.
//
// Persistence is intentionally absent in v1: clients send the full
// conversation history with every request and the server keeps no
// state. Adding persistence is a deliberate follow-up (FUTURE.md §6).
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
}

// Message is one turn. Role is restricted to RoleUser / RoleAssistant
// from clients; RoleSystem from a client is rejected at the service
// layer so the server-controlled system prompt is the only system
// voice the upstream LLM ever hears.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
