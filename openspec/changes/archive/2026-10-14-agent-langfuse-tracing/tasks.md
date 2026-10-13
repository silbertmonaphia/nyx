# Tasks

> Scope fence (parallel work, do not expand): no edits to `backend/` Go files, `frontend/src/`,
> the four active `-rollout` changes' directories, or `agent/executor.py` beyond the observation
> wrap sites named in 3.4. Rollout gates live in
> `openspec/changes/agent-langfuse-tracing-rollout/` — do not put prod flips or deployed-stack
> smokes here.

## 1. Self-hosted Langfuse stack (compose profile)

- [x] 1.1 Add the `langfuse` profile block to root `docker-compose.yml`: `langfuse-web` /
  `langfuse-worker` (`docker.langfuse.com/langfuse/langfuse*:4`, pinned concrete tags),
  `langfuse-db` (postgres:17), `langfuse-redis` (redis:7), `langfuse-clickhouse`
  (clickhouse-server:25.12), `langfuse-minio`; upstream healthchecks + `depends_on:
  service_healthy` chain; named volumes per stateful service; `langfuse-web` joins
  `[default, internal]`, everything else stays off `internal`; **only** `127.0.0.1:3000:3000`
  published; `TELEMETRY_ENABLED=false`; env from root `.env` with dev defaults documented in
  `.env.example`; adapted-from comment citing the upstream compose URL + commit. Verify:
  `docker compose --profile langfuse config` is valid; `docker compose --profile langfuse up -d
  langfuse-web langfuse-worker` (deps auto-start) reaches healthy; `docker compose ps` + host
  `ss -ltn` show port 3000 as the only new published port; `make up` (default profile) starts
  none of the new services (design D1). **Verified** — throwaway project `nyx_lf_probe`
  (same compose file, `-p` + `--env-file`; the operator's `.env` is permission-guarded and
  untouched): all six containers Up healthy; `docker ps` shows exactly one published port
  (`127.0.0.1:3000->3000`); `config --quiet` valid. Bring-up surfaced and fixed three real
  issues: (a) images switched to Docker Hub refs `langfuse/langfuse*:4.58.0` — the vendor
  registry's layer downloads redirect through registry-1.docker.io and time out on this
  network; digests verified identical across registries (divergence #4 in the block comment);
  (b) chainguard minio + explicit `user: 101:101` leaves `HOME=/` unwritable → `mc ready local`
  healthcheck failed forever ("mkdir /.mc: permission denied"); fixed with `HOME=/tmp`;
  (c) `langfuse-web` crash-looped under the initial 1g cap ("FATAL ERROR: Reached heap limit")
  during Next.js/Prisma init — raised to 2g + `NODE_OPTIONS=--max-old-space-size=1536`.

- [x] 1.2 Deterministic key bootstrap: wire `LANGFUSE_INIT_ORG_ID/PROJECT_ID/ORG_NAME/
  PROJECT_NAME/PUBLIC_KEY/SECRET_KEY/USER_EMAIL/USER_NAME/USER_PASSWORD` (dev values in root
  `.env.example`, matching what task 2.3 puts into the agent env). Verify, without touching
  existing volumes: `docker compose -p nyx_lf_probe --profile langfuse up -d ...` on a throwaway
  project, then `curl -u "$PK:$SK" http://localhost:3000/api/public/traces` returns HTTP 200 —
  no UI interaction; tear the probe project down with `down -v` (spec `agent-llm-tracing` —
  "Fresh profile up yields a working key pair"). **Verified** — fresh-volume probe boot:
  `GET /api/public/projects` with the `.env.example` key pair → 200
  `{"id":"nyx-agent","name":"Nyx Agent","organization":{"id":"nyx-dev",...}}` (bootstrap exact);
  wrong keys → 401; unauthenticated → 401. Note: v4's trace-read API is
  `GET /api/public/v2/observations?fromStartTime&toStartTime` (the v1 `/traces` path 404s —
  task 4.1's spec must use v2; confirmed against the installed SDK's own API client docs).
  Probe project still up (reused for the 4.1 e2e). Teardown decision: LEFT RUNNING for
  operator inspection of the e2e traces (UI http://localhost:3000, pk/sk `pk-lf-nyx-dev-agent` /
  `sk-lf-nyx-dev-agent`); tear down anytime with `docker compose -p nyx_lf_probe --env-file
  /tmp/nyx-langfuse-probe.env --profile langfuse down -v` (wipes the probe volumes). The real
  `make up-langfuse` path creates fresh volumes under the main project.
  **Literal run (2026-10-14, post-commit):** operator appended the `LANGFUSE_*` block to the root
  `.env` and ran the real path. First boot stalled in the web container's init phase (log went
  silent right after MCP-feature registration, twice — including after a web-only `restart`);
  the probe was torn down (freeing :3000) and a full `make down-langfuse` → `make up-langfuse`
  cycle booted clean, health 200 within ~20s. Verified on the real project: only
  `127.0.0.1:3000` published; `GET /api/public/projects` with the bootstrapped key pair → 200
  (`nyx-dev` / `nyx-agent`), wrong keys → 401; the same cycle served as the literal
  `down-langfuse` semantics check — `nyx-db-1` (up 2 weeks) / `nyx-redis-1` / `nyx-jaeger-1`
  (up 12h) untouched throughout.

- [x] 1.3 Makefile: `up-langfuse` / `down-langfuse` mirroring `up` / `down` semantics
  (idempotent, stop-only-langfuse-services for down), `make status` reports the langfuse
  containers, root README/Makefile help text updated. Verify: `make up-langfuse` twice is
  idempotent; `make status` lists them; `make down-langfuse` leaves `db`/`redis`/`jaeger`
  running; `make up`/`make down` behavior unchanged. **Verified (command-level)** — targets
  added with `#:` help text; `make -n up-langfuse` → `docker compose --profile langfuse up -d
  langfuse-web langfuse-worker`; `make -n down-langfuse` → `docker compose --profile langfuse
  stop langfuse-web langfuse-worker langfuse-db langfuse-redis langfuse-clickhouse
  langfuse-minio` (stop-only; `docker compose down` would take the whole project — including
  the operator's db/redis/jaeger — down, so `stop` is the correct verb here). The probe
  exercised the exact up command (plus `-p`/`--env-file`) idempotently. Resolved text differs
  from the draft (web on 2g after 1.1's OOM finding). Remaining literal run of `make
  up-langfuse` against the operator's `.env` is blocked on the operator appending the new
  `LANGFUSE_*` block (bash access to `.env` denied by policy — flagged to the user; the
  `.env.example` section is copy-ready).

## 2. Dependency + config surface

- [x] 2.1 Add `langfuse` to `backend/agent_py/pyproject.toml` pinned to a concrete v4 release;
  verify fresh `pip install`/venv sync succeeds and `python -c "import langfuse; print(...)"`
  prints the pinned version; capture the version in the design's verification notes for 3.2.
  **Verified** — mid-apply correction: the Python SDK's version line is independent of the
  platform's; PyPI's current line is 4.x (4.17.0), so the bound is `langfuse>=4.17,<5` (compose
  platform stays 4.58.0). `.venv/bin/pip install 'langfuse>=4.17,<5'` → `Successfully installed
  langfuse-4.17.0`; `python -c "import langfuse; print(langfuse.__version__)"` → `4.17.0`.

- [x] 2.2 `config.py`: add `langfuse_enabled: bool = False`, `langfuse_host:
  str = "http://langfuse-web:3000"`, `langfuse_public_key: str = ""`,
  `langfuse_secret_key: SecretStr = SecretStr("")`; extend the `model_validator` to refuse
  startup when enabled and either key is empty, with an error naming the missing env var.
  Verify: `test_config.py` covers defaults + both refusal cases (spec `agent-python-service` —
  "Langfuse enabled without credentials refuses startup", "Langfuse defaults applied when
  unset"). **Verified** — `tests/test_config.py` 24 passed (was 18): new
  `test_langfuse_defaults` (enabled false, host localhost:3000), parametrized
  `test_langfuse_enabled_without_credentials_refused` (public-missing / secret-missing /
  both-missing, each naming the var), `test_langfuse_enabled_with_credentials_accepted`
  (SecretStr doesn't leak via repr); `langfuse_host` added to the host-reachable-defaults
  parametrize. Mid-apply artifact correction (surfaced): the draft spec/design had the host
  default as the compose hostname — corrected to the host-dev `http://localhost:3000`
  convention documented at config.py's mcp_url/otel_endpoint comments (host-run `make dev-agent`
  can't resolve compose-only names; compose overrides for the container).

- [x] 2.3 Docs + env wiring for the new keys: `backend/agent_py/README.md` env table (the four
  keys + the compose-profile bring-up section), `backend/agent_py/.env.example` (commented dev
  values incl. the `LANGFUSE_INIT_*`-matching key pair and `PYTHON_AGENT_LANGFUSE_HOST=
  http://localhost:3000` for host-run dev), and the compose `agent` service env (keys passthrough
  + `PYTHON_AGENT_LANGFUSE_HOST=http://langfuse-web:3000`). Verify: `grep -n
  'PYTHON_AGENT_LANGFUSE' backend/agent_py/README.md backend/agent_py/.env.example` finds all
  four; `docker compose --profile app --profile langfuse config` shows the agent service carrying
  the five values; the documented bring-up commands run as written.
  **Verified** — README gains an "LLM tracing: Langfuse (opt-in)" section (bring-up, env table,
  identity/masking notes, D2 rationale pointer); `.env.example` gains the commented block (4
  greppable vars in each file). Compose: `config --no-interpolate agent` shows the agent service
  carrying `PYTHON_AGENT_LANGFUSE_HOST=http://langfuse-web:3000` + both key passthroughs
  (`${LANGFUSE_INIT_PROJECT_*}` — single source with the web bootstrap); ENABLED deliberately
  NOT listed in the `environment:` block so the `backend/.env` value isn't masked by
  compose's environment-precedence. Note: full interpolated `config` can't render in this
  worktree — `frontend/.env` doesn't exist (operator runs the SPA via vite); validated with
  `--no-interpolate` + earlier full renders that don't need the app profile.

## 3. Trace client + wiring in agent_py

- [x] 3.1 `observability.py`: `setup_langfuse(settings)` — idempotent module-level singleton;
  when disabled it constructs nothing (returns a no-op handle); when enabled it builds the
  client from settings with a dedicated, never-globally-registered `TracerProvider` (design D2/
  D5), and `shutdown_langfuse()` flushes + shuts it down, wired into the existing lifespan
  teardown next to `shutdown_observability`. Verify: unit tests — disabled → no client, no
  network call (spy on constructor); enabled → client present and
  `trace.get_tracer_provider()` identity unchanged across setup (global provider untouched);
  shutdown flushes pending spans to an in-memory exporter.
  **Verified** — unit tests: `tests/test_langfuse_tracing.py` (disabled → None + zero registry
  state; enabled → `trace.get_tracer_provider()` identity unchanged + a span created via the
  client lands in the injected exporter; idempotent; shutdown flushes + resets; handler
  None↔instance). Live wire: real OTLP export (no injected exporter) from this module into the
  probe instance → both `wire-probe` + `wire-probe-step` spans read back via
  `/api/public/v2/observations`. Two pinned-version findings folded in: `host=` is deprecated
  (use `base_url=`) and the SDK builds no proxyless session — we now pass our own
  `OTLPSpanExporter` mirroring the SDK's construction (endpoint `/api/public/otel/v1/traces`,
  Basic(pk:sk) + `x-langfuse-sdk-*` headers) with `trust_env=False`, the same posture as the
  Jaeger exporter above. Lifespan wiring: `setup_langfuse` after `setup_observability`,
  `shutdown_langfuse()` before `shutdown_observability()` in teardown.
  Correction: at this point the "Jaeger pipeline untouched" property rested on the
  global-provider identity test alone; review then proved the deeper ambient-context
  re-parenting (C1) — see §5.1 for the fix, the new regression test, and the re-run evidence.

- [x] 3.2 Credential scrubbing: a `scrub()` function (bearer/JWT/API-key-shaped substrings →
  `[REDACTED]`) applied to every observation payload the agent constructs; check the pinned SDK
  for a client-level mask hook and wire it if present, else record the finding (with the pinned
  version) in the change's notes and re-check at the e2e gate. Verify: unit tests cover bearer,
  `Bearer <jwt>`, and `sk-`-shaped inputs, pass-through for normal text, and that the helper is
  applied on the tool-args/result path (spec `agent-llm-tracing` — "Credentials never reach the
  trace backend"). **Verified** — SDK 4.17.0 **has both hooks** and both are wired:
  `mask=` (SDK-API data; also covers handler-captured payloads) and `mask_otel_spans=` (final
  OTel-batch backstop — patches only changed attributes, returns None on clean batches).
  Regression: the `sk-proj-...` shape needed hyphens in the key-body regex. Unit coverage:
  bearer/JWT/`sk-lf-`/`sk-proj-` redaction, normal-text pass-through, idempotence, recursive
  container walk, hook contracts. Tool args/result path: `scrub_value` runs inside
  `trace_observation`/`update_observation` (every agent-constructed payload) and a live run's
  tool span carries the scrubbed metadata (test 3.4). The bearer token itself only ever rides
  HTTP headers (MCP/RAG clients), never prompts or observation attributes.

- [x] 3.3 Attach the handler at the graph-config seam (`main.py:494-496`): append the Langfuse
  callback handler to `config["callbacks"]` and set `config["metadata"]` `langfuse_session_id=
  str(run_id)` + `langfuse_user_id=str(user_id)`, only when tracing is enabled; if a call site
  escapes config propagation, pass `config={"callbacks": [...]}` explicitly at that `ainvoke`
  (design D3). Verify: integration test with a stub LLM + in-memory exporter driving
  `run_agent_stream` — a canned run yields one trace whose session id is the run_id and user id
  is the caller; planner and executor generations each present with non-empty input/output;
  provider-reported usage attached (task 3.5's stub returns usage); with tracing
  disabled the in-memory exporter sees zero spans (spec `agent-llm-tracing` — generation +
  identity requirements). **Verified** — `tests/test_langfuse_run.py` (real `BaseChatModel`
  fake so the callback machinery fires; full SSE endpoint): identity → `session.id == run_id`
  and `user.id == "42"` on the run's spans (handler maps the metadata keys via
  `_parse_langfuse_trace_attributes`); generations → planner + executor `generation`-type spans
  each with non-empty input/output, `model.name == stub-model`, and provider-reported
  `usage_details = {"input": 11, "output": 22, "total": 33}` (no fallback to the estimated
  constant); no explicit per-`ainvoke` fallback was needed — langGraph config propagation
  reaches every call site; disabled → zero spans. Also captured free: langGraph node spans
  (`planner`/`executor`) + `langfuse.trace.metadata.thread_id`.

- [x] 3.4 Step + tool observations: a no-op-when-disabled context-manager helper; wrap the
  existing `agent.step.*` sites (`planner.py:100`, `executor.py:104`, `executor.py:297`) and the
  MCP dispatch site, carrying step id, name, status, tool args (scrubbed), tool outcome, and
  measured duration (design D4). Verify: extending the 3.3 integration test — a tool-step run
  shows plan/step/tool spans with duration > 0 under the same session; a failing tool dispatch
  yields a failed tool span + failed step span; disabled → zero spans.
  **Verified** — `trace_observation`/`update_observation` helpers (no-op when disabled); plan
  span wraps `planner.plan_node` (input = messages (dual-shape accessor, matching
  `_messages_to_prompt`'s convention), output = plan or `{status: failed, code}`), step spans
  wrap the executor loop (input = step shape, terminal `output.status`), tool dispatches wrap
  each attempt in `agent.tool_call` (masked args, `attempt`, `output.status`, ERROR level on
  failure). The dispatch wrap deliberately keeps failure classification OUTSIDE the `with`
  (observation closes as ERROR before the retry logic runs — the tests caught the alternative
  ordering as an exception-path problem during development). Integration test asserts
  `agent.step.tool` (metadata.tool == list_feeds) + `agent.step.llm` + `agent.step.plan` span
  presence in the same session; duration is inherent to the span (start→end); failed-dispatch
  test (`test_failed_tool_dispatch_marks_spans`): 3 attempt spans (initial + 2 retries), each
  `output.status=failed` + ERROR level, step span `output.status=failed`, all in the run's
  trace (same-trace-id assertion — session/user attributes ride the trace root, so children are
  matched by trace id, not by attribute duplication). Test-harness note: the SDK's
  process-wide per-public-key registry is NOT de-registered by `shutdown()` — both tracing test
  files clear it in teardown, otherwise a later test's `get_client()` sees ≥2 instances and
  silently hands the handler a tracing-disabled client (observed live during development).

- [x] 3.5 Best-effort containment: every Langfuse call on the run path wrapped so an exception
  logs at warn and returns; flush only on shutdown, never on the SSE critical path. Verify:
  integration test — with the exporter raising on every export, a run completes with frames
  byte-identical to the tracing-disabled stream (compare frame lists), a warn-level log is
  emitted, and the `done` frame is not delayed by flush (spec `agent-llm-tracing` — "Trace
  delivery is best-effort"). **Verified** — `test_export_failures_never_change_the_run`: with a
  raising exporter the SSE frame-kind sequence is identical to the disabled run (done +
  terminator present); containment units: `trace_observation` yields None + runs the body when
  the client fails to open an observation, body exceptions propagate untouched, `None`-client
  paths are full no-ops. No flush on the run path anywhere — `flush`/`shutdown` only via
  `shutdown_langfuse()` in lifespan teardown.

## 4. E2E gate + docs sync

- [x] 4.1 `frontend/tests/e2e/chat-langfuse-tracing.spec.ts` (house pattern — header documents
  preconditions: `make up` + `make up-langfuse` + `make dev` with the flag on + reachable LLM):
  register a fresh user → open chat → send a tool-step prompt → assert the reply renders → read
  the bearer from `sessionStorage`/decode the JWT for the numeric user id → poll
  `GET /api/public/traces` (basic auth with the dev key pair) until the run's trace appears →
  assert session id = run_id, user id matches, planner + executor generations with non-empty
  input/output and non-zero usage, and the tool span with duration. Verify: `cd frontend && npm
  run test:e2e -- chat-langfuse-tracing` passes against the live stack, twice consecutively (no
  cross-test state); the trace assertions come from Langfuse's own read API (consumer-side
  parse). **Verified** — spec committed; ran **twice consecutively, both passed** (12.2s /
  10.2s) against a *side-instance* stack (operator's dev stack untouched, per the worktree
  rule): side agent :9010 with tracing on (→ operator's vLLM :8000, db :5433, redis :6380),
  side backend :8081 (own JWT secret; serves /mcp + RAG bridge so the graph is self-consistent),
  side vite :5174 (VITE_API_URL→8081), probe Langfuse :3000. `playwright.config.ts` gained the
  `PLAYWRIGHT_BASE_URL` override (default unchanged at :5173) — exactly for this side-instance
  pattern. Correction to the draft: v4's read API is
  `GET /api/public/v2/observations?fromStartTime&toStartTime&userId|traceId&fields=basic,io,usage`
  (v1 `/traces` 404s; userId is a query filter; field GROUPS control IO/usage inclusion).
  Wire findings folded into the spec: `userId`/`sessionId` ride the trace root observation only
  (children are reached via `traceId`; the root-first ingestion batch means the poll must wait
  for generations, not just non-empty results), and multiple spans carry
  `isRootObservation=true` (the agent's step observations among them), so the attribution
  anchor is the userId-bearing observation, not the flag. Verified trace content per run:
  LangGraph root (user + session) → planner/executor ChatOpenAI generations (io + usage) →
  `agent.step.plan` / `agent.step.tool` / `agent.step.llm` / `agent.tool_call` spans.

- [x] 4.2 Docs sync: update `AgentStack.zh.md` §三-A (gap row now partially closed: prompt/reply
  tracing landed; cost/eval still open), strike queued item 1 in §四, annotate item 5 as
  unblocked-by; update `AgenticStackNaive.zh.md` P1.3 note ("Langfuse 式 prompt/response 级 trace
  未做" → landed) and flip its status marker; add the tracing entry to `FUTURE_AGENT.md`
  observability section. Verify: `grep -rn -i 'langfuse' AgentStack.zh.md AgenticStackNaive.zh.md
  FUTURE_AGENT.md` shows the updated entries and no stale "未做"/queued claims for item 1.
  **Verified** — §三-A row rewritten (landed; flag/host/UI + what's captured + Jaeger stays
  metadata-only); §四: item 1 moved into 已落地 ("原列表第 1、4、5 项", sibling noted as PM-run),
  queue renumbered 1–4, item 5 (prompt registry) annotated "Langfuse 已落地…有落点";
  AgenticStackNaive P1.3 flipped 🟡→✅ with the new note; FUTURE_AGENT §1.2 gains the
  `[x] LLM-level traces (Langfuse)` bullet. Greps: updated entries present; remaining "未做"
  hits are unrelated (P0.4 template, cross-session memory).

- [x] 4.3 Full-suite integration pass (gate before review): `cd backend/agent_py && pytest`
  (testcontainers Postgres up) — suite green incl. the new tests; CI-mode run (no
  `TEST_ASYNC_PG_DSN`) unaffected; `cd frontend && npm run lint && npm test`; full `npm run
  test:e2e` (regression on chat-rag/chat-agent-* specs); `make up`/`make dev` flows unchanged.
  Confirm no Go files changed → `make sqlc-diff` / golangci-lint untouched by construction.
  This gate runs the change's whole surface before hand-off; the rollout sibling's deployed-stack
  gates stay out of this change.
  **Verified** — agent suite DB-gated: **180 passed, 1 skipped** (21s) against a dedicated
  throwaway `postgres:17` container on 127.0.0.1:5434 (`TEST_ASYNC_PG_DSN`; the operator's
  shared Postgres was deliberately NOT written to — policy-denied, and a throwaway container is
  the isolation rule anyway); CI-mode: 148 passed / 33 skipped. Frontend: unit **207 passed**;
  `npx eslint` on the changed files clean — the repo-wide `npm run lint` fails on two
  PRE-EXISTING items untouched by this diff (`.dependency-cruiser.cjs` `__dirname` no-undef +
  a warning in gitignored `coverage/`); flag for a separate hygiene fix. Full e2e:
  **19 passed** on a clean run (first run had one flake in the untouched
  `chat-agent-retry.spec.ts` replay case — passed standalone and on rerun; pre-existing
  parallelism flake, noted). No Go files changed → sqlc-diff/golangci-lint untouched by
  construction. `make up` unchanged (langfuse is profile-gated); `make dev` flows unaffected.
  Post-review-fix re-run (§5): DB-gated **186 passed, 1 skipped**; CI-mode 153 passed / 34
  skipped; e2e spec green twice + full e2e **19 passed**.

## 5. Review-verdict fixes (test-reviewer, 2026-10-14; verdict: request changes → all applied)

> The reviewer's C1 probe empirically proved that `start_as_current_observation` (the original
> D2 implementation) writes into the SHARED, provider-agnostic OTel context slot: enabling
> tracing re-parented Jaeger spans onto langfuse-only span ids (and adopted the langfuse trace
> id), orphaning the existing Jaeger trees. §5 rewrites that mechanism; the D2 claims in §3.1's
> evidence and design.md hold only as of this section.

- [x] 5.1 C1 (must-fix) — detach the observations from the ambient OTel context.
  `trace_observation` now uses the non-context `start_observation` + explicit
  `trace_context={"trace_id", "parent_span_id"}` (trace id from the langfuse SDK's OWN context —
  `get_current_trace_id()`, probe-verified to be independent of the ambient OTel span; an
  `is_langfuse_span(ambient)` guard was tried first and empirically wrong — the handler does not
  attach its spans ambiently in 4.17.0) + a ContextVar parent for nesting (step → tool_call) +
  explicit guarded `end()`. `jaeger_span(tracer, name)` helper replaces every run-path
  `tracer.start_as_current_span`: nested spans parent from our own ContextVar; top-level spans
  keep AMBIENT parenting (preserving the ASGI `POST /agent/run` parent the pre-tracing stack
  produced), bypassing langfuse spans if one is ever ambient (defensive). Also exposed: the
  graph nodes resolved their tracer via a fresh global lookup (`graph.py` factories) — the
  injected `app.state.tracer` never reached them; `build_graph` now takes `tracer=` and main
  passes it. Verify: `test_enabling_tracing_does_not_touch_the_jaeger_tree` (no Jaeger span
  parented to ANY langfuse span id + agent-subtree structure identical enabled vs disabled) and
  `test_trace_observation_does_not_hijack_the_otel_context` (ambient current span unchanged
  inside the helper). **Verified** — 25 tracing tests green; the enabled run's Jaeger tree shows
  `agent.step.* → POST /agent/run`, `agent.budget → agent.step.llm`,
  `agent.planner.structured → agent.step.plan`, zero dangling parents.

- [x] 5.2 C2 (should-fix) — D6 containment on the two remaining call sites: `setup_langfuse`'s
  enabled branch (SDK import / exporter build / client construction) and `get_langfuse_handler`
  (SDK import / `CallbackHandler()`) now log at warn and degrade to tracing-off instead of
  failing boot / runs. Misconfiguration (enabled without keys) still refuses at the config
  validator — the wrap only covers SDK-side failure. **Verified** — code path inspection +
  the disabled/raising suites stay green (an SDK-import failure would now only be reachable by
  breaking the install; the degrade path is exercised by construction of the try blocks).

- [x] 5.3 C4/C5 (nits) — `plan_node`'s trace input uses `state.get("messages", [])` (matching
  `_messages_to_prompt`'s accessor); `trace_observation`'s docstring now matches the guard
  structure (construction guarded, `end()` guarded in `finally`, body exceptions propagate).
  **Verified** — planner suite green; docstring read-through.

- [x] 5.4 T1 — spec-scenario gaps closed: `test_react_iterations_are_each_captured` (planner +
  each react-loop model call = own generation, `agent.step.react` span) and
  `test_tool_reprompt_model_call_is_captured` (registry-rejected tool → re-prompt model call is
  a generation). Harness fixes the tests exposed: `main.py` binds `RagClient`/`ToolsClient` by
  from-import — the fixture was silently hitting REAL `http://mcp.test` (hence "MCP 502"
  through the dev proxy and bogus transient retries); both main bindings are now patched, and
  the fakes implement `bind_tools` (langchain-core 1.6's `BaseChatModel.bind_tools` raises
  NotImplementedError — ChatOpenAI implements it, test fakes must too). **Verified** — both
  tests green; react run: 3 generations; re-prompt run: 3 generations.

- [x] 5.5 T2 — `test_resumed_run_keeps_langfuse_session` (DB-gated): phase 1 drives
  `_stream_agent_run` directly and `aclose()`s right after the plan frame (the sweeper suite's
  disconnect trick — httpx's ASGITransport runs the route to completion, so an HTTP-level
  "disconnect" never leaves the row running), leaving the row `running`; phase 2 echoes the
  run_id over HTTP → `resumed` note → both phases' spans carry one session id == the run_id.
  **Verified** — test green (1.15s); spec scenario "Resumed runs keep their session" covered.

- [x] 5.6 T3/T4/T5 — export-level credential sweep over ALL span attributes of a traced run
  (the caller's live bearer is on the request; any leak would fail the sweep); failed tool/step
  spans asserted to carry `langfuse.observation.level == "ERROR"`; frame-parity comment explains
  why kinds-only (ids differ per request). **Verified** — all assertions green.

- [x] 5.7 Remaining nits — Makefile header RAM estimate 4 GB → 6 GB (matches compose); e2e spec
  env knobs renamed `NYX_E2E_LANGFUSE_*` (bare `LANGFUSE_*` are the SDK's own names — colliding
  with operator-exported agent credentials); rollout §2.4 log-level wording corrected
  (agent open/update failures at warn, OTLP processor export failures at error). Accepted as-is
  with rationale: the custom OTLP exporter keeps the default timeout (dev-facing integration;
  the SDK default mirrors it). **Verified** — greps/read-through; e2e spec still green under
  the renamed knobs.

## 6. Post-commit fix — shutdown hang with tracing enabled (peer report, 2026-10-14)

> Reported by the `agent-web-tools` session: with `PYTHON_AGENT_LANGFUSE_ENABLED=true` in the
> dev `.env`, `pytest tests/` hangs at fixture teardown. Reproduced here: `timeout 90 pytest`
> → exit 124; faulthandler stack = `shutdown_langfuse` → `client.shutdown` →
> `resource_manager.flush` → **unbounded `queue.join()`** on the SDK's ingestion queue. The
> SDK's sibling threads are daemons, so the fix bounds the wait and abandons a wedged flush —
> the documented best-effort delivery contract, never a process-wide stall.

- [x] 6.1 `shutdown_langfuse` runs `client.shutdown()` on a daemon thread with a bounded
  `Event.wait(5s)`; on timeout it logs a warning and returns (state reset first, so shutdown
  stays idempotent and re-entrant). **Verified** — forced-env repro now completes:
  `PYTHON_AGENT_LANGFUSE_ENABLED=true … pytest tests/test_run.py` → exit 0 (35.7s; bounded
  stalls, zero unlimited hangs) where it previously hung past 90s.

- [x] 6.2 Test hermeticity (the actual trigger): ten `Settings(...)` constructions across eight
  lifespan-driving test files read the developer's `.env` implicitly — once that file enabled
  tracing, every fixture teardown constructed/shut down a real client. All sites now pass
  `_env_file=None`(the established convention in `test_config.py` / the langfuse test files).
  **Verified** — with the dev `.env` (tracing enabled) present, plain
  `pytest -q` → **243 passed, 34 skipped in 7.7s** (previously: hang). Real environment
  variables still override, so an explicitly forced run remains possible — now bounded by
  6.1.
