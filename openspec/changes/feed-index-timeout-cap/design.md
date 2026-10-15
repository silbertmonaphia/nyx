# Design

## D1 — Chosen: per-call deadline on the existing best-effort call

The three indexer calls are already failure-tolerant (warn + continue) and already have a healer (retrieve-time `BackfillOnce`, fired once per user per process window from the chat path). The only defect is that the failure path waits out `LLM_TIMEOUT` (60s) instead of failing fast. A per-call deadline (`context.WithTimeout`, `FEED_INDEX_TIMEOUT`, default 5s) fixes exactly that: zero semantic change, zero new machinery, request latency bounded at the cap.

Healthy-path check: a single-feed embed against the local 0.6B embed server is sub-second; 5s is generous headroom for load. The cap does not change *whether* indexing is attempted — only how long the attempt may take before the write proceeds.

## D2 — Considered alternatives: async + degradation designs

If the write path must never touch the embedder at all (or write latency must drop closer to zero under a healthy-but-slow embedder), these are the shapes considered, in increasing strength:

**A. In-process queue.** Write path enqueues `{userID, feedID}` to a bounded channel (say 1024); one or two goroutines drain it and call `Index` with their own generous deadline. Cheapest async: no schema, no infra. Loss profile: queue contents die with the process and a full channel must shed (drop-oldest + counter metric). Tolerable *only because* the backfill heals dropped jobs — but "events vanish silently on restart" makes operational reasoning murky, and there is no lag/depth visibility without new metrics.

**B. Transactional outbox (the proper async).** New table `feed_index_jobs(feed_id, user_id, op, enqueued_at, attempts, next_attempt_at)`, enqueued in the *same transaction* as the feed write (atomicity: no lost jobs even on crash between write and enqueue); a poller (`FOR UPDATE SKIP LOCKED` batches, ticker) processes with exponential backoff and a dead-letter counter after K attempts. Durable, restart-safe, uses Postgres (source of truth, no new infra). Costs: migration + sqlc queries + poller lifecycle + retry policy + depth/lag metrics — real machinery for a problem whose worst symptom today is a bounded 5s (post-fix) write.

**C. Redis list/stream — rejected.** The repo's own invariant is that Redis is an opt-in, best-effort cache (`REDIS_ENABLED=false` by default; every cache failure is swallowed). A *durable* queue cannot hang off a component the system is allowed to run without; a Redis-disabled deployment would need a different path anyway. Wrong tool per the project's conventions.

**D. LISTEN/NOTIFY triggers — rejected.** Notifications are lost while a listener is disconnected (restart, partition) and carry no retry; the sweep/backfill would remain the real durability layer, so this is B's complexity without B's guarantees.

**Read-after-write trade (why async is not free):** today a healthy embedder indexes *within* the write request, so the user's next RAG chat sees the fresh feed. All async variants widen that window to "until the worker runs" (normally ms; during an outage, until the next `BackfillOnce`). The current sync design buys read-after-write consistency at the cost of the outage tax; the cap keeps that consistency while removing the tax. Any async move must consciously trade the consistency back.

**Degradation layers any async variant should carry:** a circuit breaker around the embedder client (consecutive-failure count → fail fast for a cooldown → half-open probe), a queue-depth shed policy with a dropped-jobs counter, per-job deadlines (the worker may use a longer cap than the request path), and OTel spans + counters for depth, lag, failures, and drops. None of these are worth building while the capped sync path meets the need.

**Revival criteria** (revisit async when any holds): write P99 latency under a *healthy but loaded* embedder becomes user-visible; indexing lag becomes a support question; partial RAG results during outages become a product complaint; or the embedder moves behind a batch/offline indexing pipeline.

**Queue technology choices (recorded for any future revival).** Should async return, the options tier by infrastructure cost:

1. *In process* — bounded channel + worker goroutines: zero components, but jobs die with the process, each replica carries its own queue with no global view, and shedding is hand-rolled. A stopgap, not an architecture.
2. *Postgres as the queue* — the best fit while the stack stays single-datastore: a hand-rolled outbox table (transactional enqueue is the one property only the same database can give: feed write and job enqueue commit atomically, so a crash between them is impossible), `pgmq` (an extension offering SQS-like send/read/delete with visibility timeouts — the same extension-install path as pgvector), or River (a Go library providing retries, backoff, uniqueness, and a UI over the same `FOR UPDATE SKIP LOCKED` mechanics). No new container, dev/prod parity for free, jobs inside the existing backup and transaction domain.
3. *Redis family* — `LPUSH`/`BRPOP`, Redis Streams (consumer groups, `XACK`, `XAUTOCLAIM` — the first variant with durable-ish semantics), or asynq. Rejected while Redis remains an optional, best-effort cache (`REDIS_ENABLED=false` default, failures swallowed); a durable queue cannot depend on a component the system is allowed to run without. Revives if Redis becomes mandatory for other reasons (multi-replica rate limiting, cache).
4. *Dedicated brokers* — NATS JetStream (lightest durable stream), RabbitMQ (quorum queues, per-message acks), Kafka (a pipeline tool, disproportionate for one job kind). Each adds a container to persist, monitor, upgrade, and runbook; worth it only at multi-job / multi-service scale. Cloud queues (SQS / Pub/Sub) break local parity and don't apply to a self-hosted stack.

For every variant the consumer is already idempotent — the indexer skips no-op updates via the stored `content_hash` — so at-least-once delivery needs no dedup table. Selection order for this repo: hand-rolled Postgres outbox → River for batteries-included → in-process only as a stopgap → Redis or a broker when either enters the stack for its own reasons.

**Owner ruling (2026-10-15):** no queue is introduced for this problem — the per-call cap is the fix. This landscape stays on file to serve the revival criteria above.

## D3 — The cap's configuration

`FEED_INDEX_TIMEOUT`, viper `SetDefault` `"5s"`, validated with `time.ParseDuration` + positive check alongside the existing timeout validations. Deliberately *not* reusing `LLM_TIMEOUT`: that knob bounds chat streaming semantics (60s default, different blast radius); coupling them would make tuning one silently affect the other.

## D4 — Semantics preserved, uniformly

All three call sites get the same cap: create and update share the `Index` path; delete's `Delete` call is equally pointless to await (a dead embedding row is harmless — the retriever filters `deleted_at IS NULL`). The warn-and-continue contract, span attributes, and the no-op indexer behavior are unchanged; only the deadline added to the context differs.

## D5 — Healability pin, and the drop-on-skip

A cap-skipped index must remain eligible for `BackfillOnce`. The live outage simulation exposed the exact contract: `BackfillOnce` selects via `ListFeedsWithoutEmbeddings` — **missing rows only**, never hash-stale rows. Consequences:

- a skipped *create* leaves no row → backfill heals it;
- a skipped *update* would leave the previous embedding (old `content_hash`) in place — backfill would never pick it up, and RAG would keep serving stale content indefinitely. That state is dormant in today's failure path too (the timeout simply took 60s to reach it), but the cap makes it worth closing properly.

Decision: on a skipped create/update the service best-effort deletes the feed's embedding row (same cap, warn on failure). The row delete is a pure SQL `DELETE` (`Indexer.Delete` → `DeleteFeedEmbedding`, no embedder call) so it completes in milliseconds even while the embedder hangs; the feed then reverts to missing-row eligibility and the next `BackfillOnce` re-embeds it. Missing beats stale: an absent row means "not retrievable yet", a stale row means "retrievable with wrong content". Skipped *deletes* need no drop — the row is harmless because the retriever filters `deleted_at IS NULL`, and the feed is gone.

The unit test for the bounded-wait path asserts the stub saw the delete after the skipped index, and the live simulation (task 5.2) verifies row-dropped → chat-backfill → row-restored with the new hash. That simulation also caught the lazy backfill never running at all — `detachContext` (`chat/service.go`) canceled its own context before returning, so every `BackfillOnce` exited on its first query; the sibling `fix(chat)` commit repairs it, without which this change's healability story would have been both untestable and untrue.

## D6 — No rollout sibling

Per the classification test in `openspec/config.yaml`: implementable and verifiable in dev/CI right now (unit tests + live e2e), no production flip, no multi-day window. Task 5's outage simulation runs against the local stack as evidence, not as a gate.
