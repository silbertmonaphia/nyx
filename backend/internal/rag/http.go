package rag

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

// retrieveResponse is the JSON envelope served by the Python
// agent bridge. The shape is frozen in openspec/changes/agent-langgraph
// /specs/agent-rag-bridge/spec.md — changing field names or types
// is a breaking wire change for the Python client.
//
// Empty (zero-value) Citations renders as `null` in Go's default
// JSON encoding. Python's httpx deserialiser would raise on that
// (None is not a list). The handler always allocates a non-nil
// slice so the wire shape is `[]` when there are no hits — see the
// renderResponse helper.
type retrieveResponse struct {
	ContextBlock string             `json:"context_block"`
	Citations    []retrieveCitation `json:"citations"`
}

// retrieveCitation is one cited feed. Snippet is the canonical
// chunk text the embedder saw (Title + "\n\n" + Description, or
// Title alone when Description is empty) — the same shape
// rag.ChunkText produces — so a citation's snippet mirrors the
// embedded text one-for-one. The feed_id is the SQL SERIAL PK; the
// Python client uses it to back-link into the user's UI.
type retrieveCitation struct {
	FeedID  int64  `json:"feed_id"`
	Snippet string `json:"snippet"`
}

// safeDetailMissingQuery is the static wire-level detail for a
// missing ?query= parameter. Static literal — never call
// err.Error() and copy it here.
const safeDetailMissingQuery = "Missing required query parameter"

// safeDetailMissingUser is the static wire-level detail for a
// request that arrived without an authenticated user on the
// context. In production the JWT middleware short-circuits before
// the handler runs; this branch is defensive (unit tests can hit
// it by bypassing the middleware).
const safeDetailMissingUser = "Authentication required"

// safeDetailInternal is the static wire-level detail for an
// internal retrieval failure. The Python client treats empty
// context as "no RAG" rather than a hard error, so this branch
// surfaces an empty 200 envelope — but the detail string is still
// returned on the rare 5xx path so operators can correlate.
const safeDetailInternal = "RAG retrieval failed"

// Handler returns an http.Handler that wraps rag.Service.Retrieve
// for the Python agent service. Mounted on chi at /internal/rag/retrieve
// behind the existing Bearer JWT middleware (cmd/api/main.go mount
// site). User identifier MUST come from the verified JWT via
// reqctx.UserIDFromContext — the query param user_id is accepted
// only for protocol convenience and is ignored as authoritative
// (see spec §3.1).
//
// Error contract:
//   - missing ?query=      → 400 with safe detail (no body err leak)
//   - missing user on ctx  → 401 (defensive; middleware catches it
//     first in production)
//   - svc.Retrieve error   → 200 with empty envelope + log entry
//     (Python treats empty as "no RAG")
//   - panic                → 500 with safe detail + log entry
//
// The handler is NOT registered with huma — it lives on chi
// directly so it never reaches the OpenAPI document. This is
// deliberate: the path prefix /internal/ signals service-to-service
// and the route must never be linked from frontend or advertised
// to external clients.
func Handler(svc *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Catch panics so a runaway goroutine inside Retrieve or
		// the JSON encoder can't tear down the process. The
		// Recoverer middleware higher up the stack already does
		// this, but the rag package keeps a local guard so the
		// handler is independently safe when mounted in isolation
		// (tests, future internal sub-routers).
		defer func() {
			if rec := recover(); rec != nil {
				reqID := reqctx.RequestIDFromContext(r.Context())
				log.Warn().
					Str("request_id", reqID).
					Interface("panic", rec).
					Msg("rag retrieve handler panic recovered")
				api.WriteError(w, r, http.StatusInternalServerError, "Internal server error", nil)
			}
		}()

		query := strings.TrimSpace(r.URL.Query().Get("query"))
		if query == "" {
			api.WriteError(w, r, http.StatusBadRequest, safeDetailMissingQuery,
				api.ClassifyAndLog(r.Context(), nil, safeDetailMissingQuery))
			return
		}

		userID := reqctx.UserIDFromContext(r.Context())
		if userID == 0 {
			// Production: middleware.NewAuth has already 401'd.
			// Defensive: a handler-only test that bypasses the
			// middleware lands here. Render the same envelope
			// shape the middleware would have written.
			api.WriteError(w, r, http.StatusUnauthorized, safeDetailMissingUser,
				api.ClassifyAndLog(r.Context(), nil, safeDetailMissingUser))
			return
		}

		passages, block, err := svc.Retrieve(r.Context(), userID, query)
		if err != nil {
			// Internal failure: log via the safe-error funnel
			// and return 200 with an empty envelope. The Python
			// client treats empty context as "no RAG" — never a
			// hard failure — so a transient retrieval error
			// shouldn't break the chat.
			_ = api.ClassifyAndLog(r.Context(), err, safeDetailInternal)
			renderResponse(w, retrieveResponse{
				ContextBlock: "",
				Citations:    []retrieveCitation{},
			})
			return
		}

		// Always allocate a non-nil slice so the wire shape is
		// `"citations": []` (not `null`) when Retrieve returned
		// no passages. httpx's JSON deserialiser rejects null for
		// a list field with a confusing TypeError; the Go-side
		// one-line allocation is cheaper than a Python bug.
		citations := make([]retrieveCitation, 0, len(passages))
		for _, p := range passages {
			citations = append(citations, retrieveCitation{
				FeedID:  int64(p.FeedID),
				Snippet: citationSnippet(p),
			})
		}

		renderResponse(w, retrieveResponse{
			ContextBlock: block,
			Citations:    citations,
		})
	})
}

// citationSnippet formats a passage into the citation's `snippet`
// field. The shape mirrors rag.ChunkText — Title + "\n\n" +
// Description, or just Title when Description is empty — so a
// citation reads as the same text the embedder saw when indexing
// the feed. No "[Feed N]" prefix; the feed_id already identifies
// the source.
func citationSnippet(p Passage) string {
	if p.Description == "" {
		return p.Title
	}
	return p.Title + "\n\n" + p.Description
}

// renderResponse writes the JSON envelope with the canonical
// content type. Centralised so the success and empty-on-error
// paths stay byte-identical (Python's content-type sniffing is
// the only thing that distinguishes them — they shouldn't differ).
func renderResponse(w http.ResponseWriter, body retrieveResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
