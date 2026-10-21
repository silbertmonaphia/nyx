# Proposal

## Why

The agent cannot perform a plural mutation. `update_feed` and `delete_feed` each take a single `id` (`backend/internal/tool/feeds.go:61-69`), a `tool` plan step dispatches exactly once with one args object (`executor.py:277`), and a planner at plan time has no way to know a real feed id — so it emits a placeholder. The committed golden-set baseline shows the result: `update_feed` called with `id: "feed_id_from_list"` (schema wants a number), `delete_feed` called with `{"feed_id": "list_feeds_3months"}` (wrong key, step id as value), six samples in a row, zero writes, and a green 3/3 because only plan shape was graded.

The user's actual request in those cases is plural — 「更新我近3个月的**所有**关于不好情绪的feed」. Reaching N feeds needs either N tool steps (requires knowing N at plan time, which the planner cannot) or a react loop letting the 4B issue the calls itself. Neither exists, and no prompt wording creates either.

The decision that shapes this change: **a bulk write selects its targets by query, not by id.** The planner can author a query — it has the user's own words — and it cannot author an id. Query-based arguments are the only form expressible at plan time, and they put the selection on the server instead of on a 4B that has already demonstrated it cannot carry a value from one step into a later tool call correctly.

## What Changes

- **Two new registry tools**, both Go, both selecting by query:
  - `update_feeds(query, title, description?)` — updates every feed matching `query` for the caller.
  - `delete_feeds(query)` — soft-deletes every feed matching `query` for the caller.
  - Arg structs use `validate:"required,min=1,max=100"`; the existing validator already applies `required`/`min`/`max` to slices, so no validator change.
  - Dispatch bodies loop over the matched ids calling the existing single-row `feed.Service` methods — **no repository, query, or migration change**.
- **A match cap.** A bulk write MUST refuse when the matched set exceeds a configured maximum rather than mutating an unbounded set. `list_feeds` today is a read; a destructive op matching "most of the user's feeds" is a different risk class.
- **An exact result set in `tool_result`.** The tool returns which ids it changed and which it failed and why. This is what lets the answering step report honestly — the baseline's hallucinated 「已全部转化」 exists precisely because nothing told the model what actually happened.
- **A preview path for approval.** The confirmation gate must be able to show the match count and a sample of titles before the user approves, so approval is not granted against an invisible blast radius.
- **Destructive-tool allowlists on both sides of the wire.** The eval grader's hardcoded `DESTRUCTIVE_TOOLS` (`evals/graders/security.py:75`) must learn the new names or the security line stops grading them. The Go side needs no equivalent change: feed tools register without annotations, and `agent-mcp-tools` treats annotation-absent as not-read-only, so a new mutating tool is confirmation-gated by default.

**Non-change worth stating:** `update_feed` / `delete_feed` stay. Single-target by id remains correct for "update *this* feed"; the plural cases get a plural tool.

## Capabilities

### New Capabilities

- None. The registry and MCP surfaces already exist; this adds tools to them.

### Modified Capabilities

- `agent-tool-registry`: a registered mutating tool can select multiple targets by a caller-supplied query rather than an explicit id list, subject to a match cap and an exact per-target result; the existing per-owner scoping keeps the selection inside the caller's own feeds.
- `agent-mcp-tools`: a bulk mutating tool's confirmation surfaces the match count and a title sample before the decision, so one approval covering many rows is not granted blind.

## Impact

- **Code**: `backend/internal/tool/feeds.go` (two arg structs, two dispatch bodies, two registrations, the match cap), `backend/internal/mcp/service.go` (two arg aliases, two handlers), `evals/graders/security.py` (destructive-tool list), `evals/cases/main.yaml` (the two mutation cases move to the bulk tools once they exist).
- **Not touched**: `backend/queries/*.sql`, the feed repository, the feed service interface, migrations. The loop lives in the tool body and reuses the existing single-row methods. Arg templating — the other half of making a write executable — lands in `agent-planner-write-routing`, not here.
- **Security surface introduced**: a tool argument that is a query over the caller's own data. Cross-user reach is precluded structurally (every query filters `user_id` from the request context, never from arguments). The new trust boundary is **match breadth**, addressed by the cap and the preview, not by user scope.
- **Rollout gates**: none. No production env flip, no deployed-stack smoke, no multi-day green window — the tools are inert until registered, and registration is the change itself.