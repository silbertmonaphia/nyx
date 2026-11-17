# Tasks

## 1. Schema and queries

- [ ] 1.1 Add `backend/migrations/000020_drop_kb_shared_scope.{up,down}.sql`: delete shared sources, drop the three `*_scope_owner` checks and `scope` columns, set `owner_id SET NOT NULL`, create `kb_sources_owner_name_uidx (owner_id, name)` and `kb_chunks_owner_idx (owner_id)`, and write a schema-only down migration; verify with `cd backend && make migration-check`.
- [ ] 1.2 Add migration 000020 to the `kb` schema list in `backend/sqlc.yaml` after 000018; verify `make sqlc` regenerates with no `Scope` fields and `OwnerID int64`.
- [ ] 1.3 Rewrite `backend/queries/kb.sql`: `ListSources` (all owned, ordered by name), `UpsertDirSource` with the `kind='dir'` conflict guard, `EnsureInlineSource` with the `kind='inline'` guard, owner-only `SetSourceEnabled`/`DeleteUserSource`/`ListDocumentsByOwner`/`ListPendingDocuments`/`ListDocumentsWithUnembeddedChunks`, drop `GetVisibleDocument`/`ListSourcesByOwner`/`UpsertSharedSource`, drop `scope` from upserts/chunks, and reduce `RetrieveKBPassages` to owner equality across chunk/document/source; verify `make sqlc && make sqlc-diff`.
- [ ] 1.4 Update `backend/internal/kb/store.go` wrappers and conversion helpers to the regenerated types (owner `int64`, no scope); verify `cd backend && go build ./...`.
- [ ] 1.5 Update `backend/internal/kb/store_test.go` pgxmock fixtures/assertions (column order, owner types, new queries) and add cases for the dir-upsert conflict returning `ErrSourceNameConflict` and disabled sources remaining listable; verify `SKIP_CONTAINERS=true go test ./internal/kb/ -run Store`.

## 2. KB domain and HTTP semantics

- [ ] 2.1 Remove `ScopeShared`/`ScopeUser` and every `Scope` field in `backend/internal/kb/model.go` (including `OutlineSource`); verify `go build ./...` and `go vet ./internal/kb/`.
- [ ] 2.2 Rework `backend/internal/kb/service.go`: drop `Config.Root`/`SharedSourceName` and the deployment embedder, delete `ProvisionShared`/`ProvisionInline`, remove shared special cases, filter `Enabled` in `IndexVisible`/`Outline`, use `GetOwnedDocument` for document reads, and make `resolveEmbedder` return nil without a resolver; verify the updated `service_test.go` passes.
- [ ] 2.3 Rework `backend/internal/kb/indexer.go`: drop the deployment embedder field, make `NewIndexer` take only options, resolve the embedder per owner for every source, and drop scope literals; verify `indexer_test.go` passes.
- [ ] 2.4 Update `backend/internal/kb/handler.go` (`SourceView`/`sourceView` drop `scope`, index summary wording), `errors.go` (remove `ErrSharedSourceLocked`, add `ErrSourceNameConflict` mapped to 409), and `config.go`; verify `handler_test.go` passes.
- [ ] 2.5 Update `backend/internal/kb/integration_test.go` and `recall_integration_test.go` to create per-user directory sources instead of shared sources and to assert owner-only retrieval/isolation; verify with a pgvector Postgres (`DB_URL=... go test ./internal/kb -run 'Integration|Recall'`).
- [ ] 2.6 Update `backend/internal/kb/retriever_test.go` to pin owner-only predicates across chunk/document/source; verify `SKIP_CONTAINERS=true go test ./internal/kb/`.
- [ ] 2.7 Update `docs/knowledge-base.md` (sources, visibility, endpoint table — `POST /api/kb/index` remains but is not surfaced by the SPA — and operator import), `README.md`, and `FUTURE_BACKEND.md` §12 to the per-user-only model; verify no remaining `scope`/shared-corpus wording with `grep -rn "shared" docs/knowledge-base.md README.md FUTURE_BACKEND.md`.

## 3. API wiring and configuration

- [ ] 3.1 Update `backend/cmd/api/main.go`: `mountKBStack` builds indexer/service without the deployment embedder, remove the boot `ProvisionShared` call, and update the KB/LLM warning; verify `go build ./...`.
- [ ] 3.2 Remove `KB_ROOT`/`KB_SHARED_SOURCE_NAME` fields, defaults, bindings, and validation from `backend/internal/platform/config/config.go` and their tests; verify `SKIP_CONTAINERS=true go test ./internal/platform/config/`.
- [ ] 3.3 Delete `backend/internal/rag/deployment.go` once `cmd/kb` no longer references `rag.NewDeploymentEmbedder`; verify `grep -rn "NewDeploymentEmbedder" backend` returns nothing and `go build ./...`.
- [ ] 3.4 Update `backend/internal/rag/wire_contract_test.go` to assert the outline no longer carries `scope`; verify `SKIP_CONTAINERS=true go test ./internal/rag/`.

## 4. Operator CLI

- [ ] 4.1 Redesign `backend/cmd/kb/run.go`: `--user` required, `--root` required, `--name` optional with basename default and reserved `inline` rejection, `UpsertDirSource` instead of shared upsert, per-user embedder resolution via `llmsettings`, and a status report carrying `owner_id`/`kind` with no scope or deployment model; verify `go build ./...`.
- [ ] 4.2 Rewire `backend/cmd/kb/main.go` to construct the cipher/settings resolver for `index` and to stop building a deployment embedder; verify `go run ./cmd/kb` prints the usage and exits non-zero.
- [ ] 4.3 Rewrite `backend/cmd/kb/run_test.go` for the new options, default naming, reserved name, resolver failure paths, upsert arguments, and status shape; verify `go test ./cmd/kb/`.
- [ ] 4.4 Update `docs/deployment.md` KB section, `backend/.env.example`, and the `docker-compose.yml` KB mount comment to the `kb index --user <id> --root <dir>` flow and the BYOK-embedding/`USER_LLM_ENCRYPTION_KEY` requirement; state that the SPA no longer offers an index button, so directory resync is a CLI (or API) action. Verify the documented command matches `go run ./cmd/kb index -h` behavior.

## 5. Python agent and cross-runtime contract

- [ ] 5.1 Drop `(scope: …)` from the planner outline line in `backend/agent_py/agent/planner.py`; verify `make -C backend/agent_py test` planner tests pass.
- [ ] 5.2 Remove `scope` from planner/catalog/rag-client outline fixtures and add an assertion that `(scope:` is not rendered; verify `backend/agent_py/.venv/bin/python -m pytest tests/test_planner_kb.py tests/test_planner_catalog_wiring.py tests/test_rag_client.py -q`.
- [ ] 5.3 Update `FUTURE_AGENT.md`'s KB-outline bullet to drop scopes; verify the roadmap no longer advertises a scope field.

## 6. Frontend

- [ ] 6.1 Remove `KbScope` and the `scope` field from `frontend/src/features/kb/types/kb.ts`; verify `npm run typecheck` (after the generated types are regenerated in 6.4).
- [ ] 6.2 Remove the scope badge and the `scope === 'user'` gate in `KnowledgeBasePage.tsx` so toggle/delete render for every source, and delete the `kb.scope` i18n blocks; verify `npm test -- KnowledgeBasePage`.
- [ ] 6.3 Remove the KB page "Index now" action: drop the button, the scan-report toast aggregation, `useKbMutations.indexSources`, `kbService.indexSources`, the `KbIndexReport`/scan-report types, and the `kb.index*` i18n keys; verify `npm run typecheck` and no remaining `kb-index` test id.
- [ ] 6.4 Update KB unit/e2e fixtures (`KnowledgeBasePage.test.tsx`, `kbService.test.ts`, `useKb.test.ts`, `frontend/tests/e2e/kb-ui.spec.ts`), remove the upload-then-index test, and add a dir-source control-visibility case; verify `npm test` and `npm run test:e2e -- tests/e2e/kb-ui.spec.ts`.
- [ ] 6.5 Regenerate `api/openapi.json` and `frontend/src/api/openapi/schema.d.ts` (`make openapi`, `npm run openapi:gen`); verify `make openapi-diff` and `npm run openapi:check`.
- [ ] 6.6 Update `FUTURE_FRONTEND.md` §12 (scope badge and index action removed; the page exposes upload/create/edit/delete only); verify the section no longer mentions a scope badge or an index button.

## 7. Integration verification

- [ ] 7.1 Run the full backend suite with Docker: `cd backend && make test` (pgvector integration included) and fix any fixture drift; verify it passes.
- [ ] 7.2 Run `make coverage-check` and restore coverage with owner-only equivalents; verify the gate passes.
- [ ] 7.3 Run the frontend gates: `npm run lint`, `npm test`, `npm run test:coverage`, `npm run build`; verify all pass.
- [ ] 7.4 Run the agent gates: `make -C backend/agent_py lint` and `make -C backend/agent_py test`; verify both pass.
- [ ] 7.5 CLI smoke on the dev DB: register a real directory for a user with saved embedding settings, run `kb index --user <id> --root <dir>`, then `kb status`; verify the source is owned by that user and KB retrieval returns a marker document from the SPA/agent.
- [ ] 7.6 `openspec validate kb-private-corpus --strict`; verify the change validates before archiving.
