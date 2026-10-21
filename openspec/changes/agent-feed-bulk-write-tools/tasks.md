# Tasks

## 1. Registry tools

- [ ] 1.1 Add `UpdateFeedsArgs{Query, Title, Description}` and `DeleteFeedsArgs{Query}` to `backend/internal/tool/feeds.go` with `validate:"required,min=1,max=..."` bounds matching `ListFeedsArgs.Query`. Verify the existing slice-aware validator accepts them with no validator change (`internal/tool/validate.go` already handles `required`/`min`/`max` on slices).
- [ ] 1.2 Implement the dispatch bodies: resolve the query to matched ids through the same owner-scoped read path `list_feeds` uses, then loop the existing `feed.Service.UpdateFeed` / `DeleteFeed` once per id, collecting a per-target result. Verify no change to `backend/queries/*.sql`, the feed repository, or the service interface.
- [ ] 1.3 Register both tools with no `annotations`, so `agent-mcp-tools`' annotation-absent fail-safe gates them automatically. Verify `create_feed` / `update_feed` / `delete_feed` registrations are untouched.
- [ ] 1.4 Add unit tests for the dispatch bodies: multi-target success, empty match returning a zero-target success, and a per-target failure producing a partial result rather than an unqualified success. Verify per-target failure reasons go through the safe-error funnel and carry no internal error text.

## 2. Match cap

- [ ] 2.1 Add a configured maximum match size and refuse the whole call when the query matches more than the maximum. Verify zero rows are mutated on refusal.
- [ ] 2.2 Classify the refusal so it is not retried automatically — it is neither a correctable-argument error nor a transient failure. Verify a single over-broad call produces exactly one dispatch attempt.
- [ ] 2.3 Verify a narrower retry of the same query proceeds normally, and add a test for it.

## 3. Confirmation preview

- [ ] 3.1 Extend the `confirm_request` frame for multi-record calls with the owner-scoped match count and a bounded sample of feed titles. Verify single-record calls emit the frame unchanged (no count, no sample).
- [ ] 3.2 Verify the preview count and sample cover only the caller's own feeds even when the query textually matches another user's, and that no cross-user existence is disclosed.
- [ ] 3.3 Fix and document the sample size as configuration; keep the query length bound consistent with `list_feeds` rather than introducing a second bound for the same parameter.

## 4. MCP surface

- [ ] 4.1 Add the two argument aliases and two handlers in `backend/internal/mcp/service.go`, following the existing shim pattern. Verify the tools appear in `tools/list` with their schemas normalized by the existing path.
- [ ] 4.2 Verify the planner's catalog block picks the new tools up with no hardcoded names — `agent-mcp-tools` requires the block to mirror the live registry, so this should be automatic; confirm rather than assume.

## 5. Eval-side destructive-tool coverage

- [ ] 5.1 Add the new tool names to `DESTRUCTIVE_TOOLS` in `evals/graders/security.py:75`. Verify the security line grades an ungated bulk call as a failure.
- [ ] 5.2 Add a grader test that fails when a registry tool that mutates is absent from the destructive-tool list, so a future mutating tool cannot silently escape the security line.
- [ ] 5.3 Move `main-rag-update-feed` and `main-rag-delete-feed` in `evals/cases/main.yaml` to the bulk tools and fix their target queries — the `list_feeds` query is a literal `ILIKE` over title and description, so the declared fixtures must actually contain the query substring for the match to be non-empty. Verify `evals/validate.py` passes.

## 6. Docs

- [ ] 6.1 Document the two tools, the substring-matching semantic, the cap, and the preview in `backend/agent_py/README.md` and the MCP tool listing wherever the five feed tools are enumerated. Verify the documented tool list matches the registration table.