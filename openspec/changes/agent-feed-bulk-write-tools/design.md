# Design

## Context

See `proposal.md` — Why. The facts that shape the approach:

- `UpdateFeedArgs{ID int; Title string; Description string}` and `DeleteFeedArgs{ID int}` (`backend/internal/tool/feeds.go:61-69`) are strictly single-target; the dispatch bodies call `feed.Service.UpdateFeed` / `DeleteFeed` exactly once with no loop (`:164-186`).
- `internal/tool/validate.go` already applies `required` (rejects an empty slice), `min`, and `max` to slices, so `IDs`-style or query-style args need no validator work.
- `ListFeedsArgs.Query` is already a substring search: `ILIKE '%' || query || '%'` over title and description, always ANDed with `user_id` from the request context (`backend/queries/feeds.sql:11,20-22`). **A bulk write inherits that semantic: literal substring matching, not semantic search.**
- All five feed tools register with no `annotations` at all (`feeds.go:81-119`), and `agent-mcp-tools` treats annotation-absent tools as not-read-only (fail-safe). A new mutating tool is therefore confirmation-gated by default with no Go-side allowlist to maintain. The only hardcoded destructive-tool list is on the eval side (`evals/graders/security.py:75`).
- Feeds are soft-deleted (`feeds.deleted_at`), so a mistaken bulk delete is recoverable.

## Goals / Non-Goals

**Goals:**

- Make a plural mutation expressible and executable, with the selection made server-side.
- Keep the destructive blast radius bounded and visible before the user approves.
- Return enough per-target detail that the answering step can report truthfully.
- Reuse the existing single-row service methods; no repository, query, or migration change.

**Non-Goals:**

- Semantic search. The query stays a substring match, matching `list_feeds`.
- Replacing the by-id tools.
- Arg templating in the planner — that lands in `agent-planner-write-routing`.
- Any bulk *create*, or a transactional all-or-nothing bulk write.

## Decisions

### 1. Query-based selection over id lists

**Chosen:** the bulk tool takes the same `query` shape `list_feeds` already takes.

**Alternatives considered:** an `ids []int` argument forces the caller — ultimately a model — to choose the set, which is the failure mode already demonstrated (placeholder ids; react results discarded). A tool-side query puts the decision in SQL where it is deterministic and owner-scoped for free.

**Consequence accepted:** the match is a literal substring match. A query like 「的」 matches broadly. That is what the cap and the preview exist for; it is not an accident to be engineered away here.

### 2. The loop lives in the tool body, not the service

**Chosen:** the dispatch body iterates the matched ids and calls the existing `feed.Service.UpdateFeed` / `DeleteFeed` once per id, collecting per-target results.

**Alternative considered:** a new `feed.Service.UpdateFeeds(ctx, ids, m)` plus a sqlc query for a set-based UPDATE. Rejected: it buys a round trip and buys nothing observable, since per-target results are already collected either way, and it would put a partial-failure contract into the repository layer that the tool layer is already positioned to express.

### 3. Refuse an over-broad match whole, not truncate

**Chosen:** exceeding the cap refuses the entire call.

**Alternative considered:** mutate the first N and report the rest as unprocessed. Rejected — it makes a destructive call's effect depend on an arbitrary ordering, which is exactly the sort of invisible-blast-radius behaviour the preview requirement exists to prevent. A refusal also gives the model a correctable signal to narrow the query rather than a silent partial success.

The refusal MUST NOT be classified as a correctable-argument error, or the retry path will re-issue the same over-broad call automatically.

### 4. Result detail is the anti-hallucination mechanism

The baseline's most damaging defect is the agent reporting a mutation that never happened (「已全部转化为积极情绪feed」 when zero writes landed). The reason it could is that nothing in the transcript told the model what actually occurred.

**Chosen:** the result names every target acted on and every target failed, with a safe per-target reason.

This is why the change is not optional polish for the eval work: without it, the `agent-planner-write-routing` answer assertion for false success has nothing reliable to grade against.

### 5. Preview via the confirmation frame, computed from the same query

**Chosen:** `confirm_request` carries the match count and a bounded title sample.

**Alternative considered:** a separate dry-run tool call before the real one. Rejected — it doubles the calls, and a dry-run result can go stale between the check and the mutation. Computing the preview inside the confirmation path, from the same owner-scoped query, keeps one source of truth.

**Open detail:** the sample size is a configuration value; this change fixes the behaviour (bounded, documented) and leaves the number to the implementer.

## Risks / Trade-offs

- **Substring semantics on a destructive op** → the cap (Decision 3) and the preview (Decision 5). Soft deletion is the backstop, not the control.
- **A newly destructive tool name missing from a hardcoded allowlist** → the Go side is annotation-driven and safe by default; the eval grader's `DESTRUCTIVE_TOOLS` is the one list to update, and a grader test that fails when a registered mutating tool is absent from it prevents a repeat.
- **Non-atomic bulk write** → a mid-run failure leaves the set partially applied. Chosen knowingly: the per-target result makes it visible, and making it atomic would need a transaction across a tool boundary the registry does not model.
- **Query length bound differs from `list_feeds`' 200 chars** → keep them consistent; two different bounds for the same parameter would be a surprise.
- **The eval corpus expects the old tool names** → `main-rag-update-feed` / `main-rag-delete-feed` move to the bulk tools; their execution assertions (in `agent-planner-write-routing`) are what confirm this change worked.

## Migration Plan

Additive: two new registrations, no change to existing tool behaviour or wire shapes. Rollback is deregistration. No migration, no query change, no backfill.

## Open Questions

- The exact preview sample size, and whether the cap should differ between update and delete. Both are configuration decisions that do not change the specs, the approach, or the task breakdown — settle them at implementation.