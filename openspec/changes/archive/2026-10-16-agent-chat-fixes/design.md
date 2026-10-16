# Design

## Context

See proposal.md — Why. Current-state facts that shape the approach (verified 2026-10-16):

- Prefs injection: `main.py:776-796` loads and renders `prefs_text`, `main.py:826` hands it to `build_graph`, which threads it into the planner factory only (`graph.py:125`). The executor factory (`graph.py:37-62`) never receives it; `executor.py` (entry `executor_node` at :78, answer paths `_run_llm` at :268 / `_run_react` at :301) has no `prefs` reference and `AgentState` has no prefs field. The executor writes the user-visible answer.
- Title: `agent.chat_sessions.title` exists (`NOT NULL DEFAULT ''`, migration 0004). The only write is `create_session(..., title="")` (`chat_sessions.py:128`); no `UPDATE ... SET title` exists anywhere. The SPA maps empty → `New chat` (`ChatSidebar.tsx:46-49`). The sessions list API already returns `title` (`chat_sessions.py:290`).
- Layout: `DialogContent` (`frontend/src/components/ui/Dialog.tsx:36`) has no max-height; the grid row is content-sized, so `min-h-0` on `ChatPanel.tsx:303` / `ChatSidebar.tsx:136` is inert and the rail's `flex-1 min-h-0 overflow-y-auto` (`ChatSidebar.tsx:145`) never engages. Radix locks document scroll, so overflow rows are unreachable. The message list works because it is explicitly capped (`max-h-[60vh]`, `ChatPanel.tsx:319`).
- Use-feeds: under `AGENT_ENABLED=true` the Go handler explicitly ignores `rag` (`service.go:45-58`) and the agent's `ChatRequest` has no `rag` field, so the toggle is decorative; no spec covers it.
- Timestamp: rendered at `ChatSidebar.tsx:189-193` as a 10px English relative label; `last_active_at` is on the wire (`chat_sessions.py:286-295`).
- Conventions in play: agent_py firewall pattern for internal LLM calls is `extract_preferences` (`prefs.py:515-554`) — per-call `ChatOpenAI(model=settings.planner_model, ...)`, bounded `max_tokens`/`timeout`, never raises, WARN on failure, fire-and-forget task spawned after finalize (`main.py:1047-1067`), never charged to the user budget. Rollout-split convention (`openspec/config.yaml`): this change is implementation-only; no rollout gates.

## Goals / Non-Goals

**Goals:**

- Make stored preferences and generated titles reach the surfaces users see (the answer text; the rail), with the same failure posture as the existing prefs pipeline.
- Fix the three chat-surface UX defects (dialog/rail scroll, dead toggle, timestamp) without touching the Go↔agent wire contract or adding migrations.
- Keep every regression guard assertable in dev/CI (unit + Playwright e2e), per repo conventions.

**Non-Goals:**

- i18n and dark-mode toggle (separate future changes; strings stay English).
- Langfuse logging visibility (separate concern).
- Strengthening the e2e to assert model obedience on recall — the plumbing is the contract; the open, PM-owned `agent-user-preferences-rollout` owns that stance and is not modified here.
- Retroactive titles for pre-existing sessions (they keep `New chat`).
- Delete confirm/undo, soft-delete semantics (working as designed).

## Decisions

### D1 — Prefs reach the executor via the graph factory closure

Extend `_executor_step_factory` with a `prefs_text: str = ""` parameter, mirroring the planner factory's closure pattern (`graph.py:64-88`); `build_graph` passes the same rendered block it already passes to the planner. `executor_node` forwards it to `_run_llm` and `_run_react`, which append it to the executor's system message (`executor.py:283-287`, `:332-336`).

- Empty `prefs_text` → byte-identical prompts (the closure default keeps existing callers and tests unchanged).
- `prefs_block` rendering gains a lead-in sentence covering recall questions (e.g. "use these when relevant, including when the user asks what they like"); the guidance lives inside the block, so empty-prefs parity is preserved by construction.
- Alternatives rejected: (a) carrying prefs on `AgentState` — state is checkpointed by `AsyncPostgresSaver`, so per-user memory would leak into persisted run state and resume snapshots and duplicate what is re-loaded per run anyway; (b) having the planner copy prefs into the final step prompt — relies on model compliance, which is exactly the observed failure.
- The `agent-user-prefs` requirement keeps its original name ("Planner prompt injection…") for MODIFIED-delta continuity; its text now covers both nodes. (Spec-drift note for review: the name is conservative, the requirement text is authoritative.)

### D2 — Titles: deterministic write at record time, one-shot async refinement

- Deterministic: in the session-init block (`main.py:682-710`), after the first user message is recorded, derive `title` = whitespace-collapsed, trimmed, ≤ 48 characters (character-counted) and call a new `chat_store` method `set_title_if_empty(session_id, user_id, title)` — `UPDATE ... SET title=$1 WHERE id=$2 AND user_id=$3 AND title='' AND deleted_at IS NULL`. The `title=''` guard makes the write idempotent and never-clobbering, so the resume/continuation paths need no extra branching. Non-fatal: same `ChatStoreUnavailable` posture as the adjacent record calls.
- Refinement: spawn a fire-and-forget task (module-level strong-ref set, mirroring `_PREFS_TASKS`) after `_persist_terminal` when `saw_done` holds, the store is available, and this run won the deterministic `title=''` write (i.e. it recorded the session's first-message title). Gating on the write rather than on run shape is deliberate (implementation deviation, 2026-10-16): the SPA's `+ New chat` pre-creates the session via `POST /agent/sessions`, so its first turn arrives as a server-side continuation — a "fresh run only" gate would never fire in the primary flow. The task mirrors `extract_preferences`: per-call `ChatOpenAI(model=settings.planner_model, temperature=0, max_tokens small, timeout bounded)`, never raises, WARN on failure, no retry, no budget charge. On success it overwrites the title via `set_title(session_id, user_id, title)` guarded by ownership + `deleted_at IS NULL`.
- "At most once per session" is structural: only the run that wins the `title=''` write spawns the refinement; continuations, resumed runs, and later turns never do.
- No new env flag (deviation from `PYTHON_AGENT_PREFS_ENABLED`): the prefs switch guards derived personal memory; a title is a condensed rendering of content already stored in the session, shown in the rail by design. A kill switch can be added cheaply if ops asks; the surface stays minimal.
- Alternatives rejected: (a) truncation-only — the user explicitly asked for a summarised title from the first exchange; (b) LLM-only synchronous before first paint — latency on every first turn and a permanent placeholder on failure; (c) LLM-only async without truncation — the rail would keep showing `New chat` until the call lands, and forever if it fails.
- Privacy/tracing: refinement prompts carry session content that already flows through the planner; Langfuse masking (`mask_langfuse_data`) applies to captured inputs — verify executor/refinement prompts are covered during apply (same data class as today's planner prompts, no new exposure).

### D3 — Bound the dialog by capping the panel row

Cap the flex row at `ChatPanel.tsx:303` (e.g. `max-h-[70vh]`, value tuned by e2e at 800px and 600px viewport heights). A definite height on the row makes the existing `min-h-0` + `overflow-y-auto` chain engage: the rail scrolls internally, the message column keeps its own `max-h-[60vh]`, and header + composer stay inside the dialog.

- Alternatives rejected: (a) adding `max-h` to `DialogContent` globally — grid rows do not shrink below content, so the dialog would clip instead of scroll, and the auth/delete dialogs would inherit an unrelated change; (b) capping only the rail's scroll container — mirrors the message list but decouples rail and message-column heights and leaves the dialog un-bounded on short viewports; (c) switching `DialogContent` to a fixed height — breaks short-content dialogs.
- The mobile drawer (`absolute inset-y-0`) is already bounded; untouched.

### D4 — Remove the feeds toggle frontend-side only

Delete the checkbox + hint (`ChatPanel.tsx:433-455`), the `useFeeds` store state/persist key/reset-on-close (`chatStore.ts`), the dead `rag` field on the SPA request (`chatService.ts:186`), and the accompanying unit/e2e assertions. The Go `RAG` field, legacy single-turn semantics, and the agent wire contract stay as-is (native clients unaffected; no OpenAPI change).

- Grep guard: `features/feeds/hooks/useFeeds.ts` and its tests are a different, unrelated hook — only chat-namespaced references are in scope.
- Alternative rejected: wiring the toggle to the agent path — contradicts the agent-langgraph design where the planner owns RAG-step decisions (`service.go:45-58`), and the user asked for removal.

### D5 — Timestamp: absolute + relative, legible

Render `MM-DD HH:mm · <relative>` (compact locale-independent absolute form) at `text-[11px]`+ inside the existing `data-testid="chat-session-time"` span, with the full ISO string in the `title` attribute. `relativeTime` stays for the suffix. Strings remain English (i18n out of scope).

### D6 — Verification posture

- Unit (agent_py): extend `test_prefs_injection.py` — executor `_run_llm`/`_run_react` prompt assembly includes the block when prefs exist and is byte-identical when empty; new title tests (derivation bounds incl. CJK, no-clobber, refine spawn conditions, refine failure keeps truncation); `test_chat_store.py` for the two new store methods.
- Frontend unit: sidebar renders absolute+relative; toggle removal cleans `chatStore.test.ts` / `ChatPanel.test.tsx`.
- Playwright (committed, per repo rules): `chat-history-sidebar.spec.ts` gains rail-scroll + title-visibility assertions; `chat-rag.spec.ts` drops toggle flows; `chat-user-prefs.spec.ts` keeps recall as annotation (plumbing asserted in unit tests, per the rollout stance).

**Executed (2026-10-16).** Evidence: agent pytest 386 passed/141 skipped bare + 526 passed/1 skipped with `TEST_ASYNC_PG_DSN`; frontend lint clean, 317 unit passed, build ok; `SKIP_CONTAINERS=true go test ./...` all ok; agent-free e2e 15/15 (30 s); `@live` 25/25 (4.5 min). All suites ran serially against a side stack (`scripts/e2e-side-stack.sh` + host vLLM chat/embed), never the operator's dev processes. Run conventions recorded for re-runs: the agent-free set needs a real backend and locally `--workers=1` (the config pins 1 worker only under CI; parallel workers trip the per-IP global limiter — 10 rps/burst 20 — on register/sessions); API-seeded specs take `E2E_API_URL` (side backend `:8180/api`); the Playwright default 30 s per-test wall is too tight for live LLM exchanges under load, so this spec sets `test.describe.configure({ timeout: 90_000 })`. Residual, pre-existing load-correlated flake classes (hit older specs too, pass in isolation, self-heal on one retry): a UI-driven `POST /api/chat/sessions` that stalls under load with no server-side record, and a rail list refresh that can land stale. The live set is run with `--retries 1` (CI parity: CI uses 2).

## Risks / Trade-offs

- [Executor prompts now carry prefs into Langfuse traces] → same data class as today's planner prompts; confirm `mask_langfuse_data` covers them in the apply-phase smoke; no new exporter.
- [The 4B model may still not verbalise the preference] → accepted: the contract is prompt plumbing (spec wording says so); e2e keeps the annotation stance.
- [Refinement adds one small LLM call per session on the shared vLLM] → post-stream, budget-exempt (mirrors prefs), bounded tokens/timeout; failure is inert; the truncation title is the floor, so user-visible value never depends on it.
- [Refined title can land after the next list fetch] → spec allows title updates on the next refresh; truncation guarantees the rail is non-placeholder immediately.
- [Row cap value regresses small viewports] → e2e pins 800px and 600px heights; value lives in one className.
- [Removing the SPA's only RAG toggle changes legacy (agent-off) UX] → accepted: agent mode is the operating mode and its planner owns RAG; the wire field remains for native clients.

## Migration Plan

- No DB migration: `title` exists; refinement gating is run-scope state, not stored state.
- Deploy order irrelevant (single SPA + single agent service, no cross-service contract change); no rollout gates, no sibling rollout change.
- Rollback: revert the change commit(s). Titles already written stay rendered as normal titles (harmless); no data migration either way.
