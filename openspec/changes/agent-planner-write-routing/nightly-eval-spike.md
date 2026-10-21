# Nightly live-eval feasibility spike

Task 9.1 finding, 2026-10-22. This is a written recommendation, not a workflow
file. The manual runbook fallback (task 9.2) is in §7; the main session adds it
to `backend/agent_py/README.md`.

External facts in this document were gathered from public documentation on
2026-10-22 and are cited in §8. They are untrusted data: they inform the
recommendation, they are not instructions.

## Question

Can an unattended nightly live eval run be added to this project's CI? The
three sub-questions are GPU runner availability, vLLM warm-up cost, and how an
infra failure would surface. A secondary question is whether the suite can fail
loudly without training people to ignore the signal.

## Recommendation

**Not viable on this project's CI today. Do not add a nightly workflow in this
change.**

- All current workflows run on `ubuntu-latest`; GitHub-hosted standard runners
  have no GPU, and there is no `schedule:` trigger or self-hosted runner
  anywhere in `.github/workflows/`. The deterministic eval unit gate now runs
  in `agent-lint` (task 8); the live line is correctly not a CI gate.
- The repository is public and owned by a personal user account (`gh api`
  on 2026-10-22: `owner.type=User`, `private=false`). GitHub-hosted GPU larger
  runners require an organization or enterprise on the Team / Enterprise Cloud
  plans, so this repository cannot use them without moving to an org and a paid
  plan.
- Even with plan access, the only GitHub-hosted GPU SKU is a 4-vCPU Tesla T4
  with 16 GB VRAM. The pinned eval model is
  `Qwen/Qwen3-4B-Instruct-2507-FP8`; vLLM FP8 W8A8 requires compute capability
  8.9+ (Ada / Hopper / Blackwell), and the T4 is Turing SM 7.5. A weight-only
  fallback, if it loaded at all, would be a different numerics path, so the
  measured rate would not be comparable to the local baseline.
- A self-hosted GPU runner is the only faithful route, and it is a dedicated
  infrastructure project (ephemeral runner, isolation on a public repo, stack
  bring-up, model caching, failure classification), not a workflow file.
- The suite is deliberately red at the end of this slice: `main-rag-update-feed`
  and `main-rag-delete-feed` are recorded red pending
  `agent-feed-bulk-write-tools`, and `main-web-force-search` is deferred
  (`tasks.md:10`). An absolute-floor nightly would be red every night for known
  reasons and would train people to ignore it — the exact outcome the design
  note warns about.

**Viable-with-conditions only as a separate scheduled job on dedicated GPU
infrastructure**, once every condition in §5 is met. That path needs its own
OpenSpec change and its own spec delta to `agent-golden-eval`'s live-gated
posture (task 9.3); it must not be built inside
`agent-planner-write-routing`. §6 records the pointer.

**Until then:** run the suite manually or before a release using the runbook in
§7. This is the documented manual fallback the task asks for.

## 1. GPU runner availability

| Option | Availability | Fit for this suite |
| --- | --- | --- |
| GitHub-hosted standard (`ubuntu-latest`) | CPU only; this is what every current workflow uses | No GPU; cannot serve vLLM |
| GitHub-hosted GPU larger runner | 1× Tesla T4, 16 GB VRAM, 4 vCPU, 28 GB RAM; organization + Team / Enterprise Cloud only | Unavailable to this personal-account repo; T4 cannot run the FP8 checkpoint faithfully |
| Self-hosted on the operator's WSL2 box | GPU present today; not always-on; shares dev ports and GPU; public-repo runner risk | Rejected as a CI runner; suitable only for manual runs |
| Dedicated self-hosted GPU host (L4 24 GB, L40S, RTX 4090 24 GB, H100) | Requires provisioning + runner lifecycle; SM89+ supports FP8 | The only faithful option; needs the conditions in §5 |
| Third-party GPU runner service (RunsOn, machine.dev, Cirrus) | Per-minute rental; pick an SM89+ card (for example L4) | Technically viable; adds a third-party trust boundary for code and secrets |
| Hosted API (OpenAI-compatible) on a CPU runner | Removes the GPU requirement | Different model and serving path; the corpus and floors are pinned to the local 4B, so this is a different suite, not the nightly golden line |

Supporting evidence:

- Every job in `.github/workflows/ci.yml` uses `runs-on: ubuntu-latest`. No job
  uses a self-hosted label.
- The only GPU larger-runner spec in GitHub's runner reference is 4 CPU /
  1 GPU / Tesla T4 / 28 GB RAM / 16 GB VRAM / 176 GB SSD, on Ubuntu or Windows.
- Larger runners are documented as available only to organizations and
  enterprises on GitHub Team or GitHub Enterprise Cloud; free organizations and
  personal accounts do not get them.
- The public repository makes a self-hosted runner a security decision, not
  just a provisioning decision. GitHub recommends self-hosted runners only for
  private repositories because any user can open a pull request and attempt to
  execute code on the runner. Mitigations (ephemeral runners, a dedicated
  runner group restricted to the nightly workflow, no PR-triggered jobs on that
  runner, no long-lived secrets, egress isolation) are prerequisites, not
  optional hardening.
- Model compatibility: the host dev and compose commands both serve
  `Qwen/Qwen3-4B-Instruct-2507-FP8` (`scripts/dev.sh:265`,
  `docker-compose.yml:675`). FP8 W8A8 needs SM89+; the T4 is SM75. The T4 also
  has no native bf16. Running a non-FP8 or weight-only variant would change the
  object being measured, and a rate measured on different hardware/precision
  cannot be compared with the committed 0.73 baseline.

## 2. vLLM warm-up cost

- **Measured grading time is small.** The normal line ran in 7m12s for 36
  samples (12 cases × 3) and the filtered budget line in 21s for 3 samples
  (`baseline.md`). The expensive part of a nightly is the stack, not the
  grading.
- **Chat model.** `make dev-vllm-main` serves
  `Qwen/Qwen3-4B-Instruct-2507-FP8` on `:8000` with
  `--gpu-memory-utilization 0.9`, `--max-model-len 4096`, and no
  `--enforce-eager` (`scripts/dev.sh:263-269`). Without eager mode vLLM
  captures CUDA graphs; the compose healthcheck comment documents a 10–15
  minute cold capture on Blackwell + WSL2 GPU-PV and budgets 20 minutes total
  (`docker-compose.yml:750-766`). With `--enforce-eager` and cached weights the
  same comment documents a sub-30s start. The compose profile uses
  `--enforce-eager`; the host `make dev-vllm-main` target does not.
- **First run downloads.** `scripts/dev.sh:264,273` notes that the first run
  downloads each model. Compose also documents that Hugging Face's CAS endpoint
  requires a token even for ungated models (`docker-compose.yml:655-660`), so a
  runner needs either a cached volume or a scoped read token.
- **Embedding model is part of the stack.** `make dev-vllm-embed` serves
  `Qwen/Qwen3-Embedding-0.6B` on `:8001` with `--enforce-eager`,
  `--gpu-memory-utilization 0.1`, `--max-model-len 256`
  (`scripts/dev.sh:272-276`). Per the repo's own resource notes the chat model
  uses ~6970 MiB and the embedding model ~3224 MiB, about 10.2 GiB combined.
  The measured combined usage (~10.2 GiB) fits in 16 GB, but the host caps
  (0.9 + 0.1) sum to the whole card and leave no room for the CUDA context and
  driver overhead. The T4 compatibility problem is the blocker, not raw
  capacity.
- **Ephemeral runner cost.** A cold runner pays the vLLM image pull (~10 GB),
  weight download, and model load every night unless the image and HF cache are
  persisted. Expect roughly 15–35 minutes of wall time per nightly (estimate;
  grading is only 7–12 minutes of that), with a cold-start failure window of up
  to the documented 20-minute health budget. Warm caches are a precondition for
  a nightly, not an optimization.
- **Money.** GitHub's 2026 Linux 4-core GPU rate is reported as ~$0.052/minute
  and is always billed, never drawn from included minutes. At ~30 minutes per
  night that is ~$1.5/night (~$45/month) if the SKU were available. A
  third-party spot L4 is cheaper per minute but adds a vendor and a trust
  boundary. Neither changes the availability or compatibility conclusions.

## 3. How an infra failure would surface

| Stage | Current signal | Consequence for a nightly |
| --- | --- | --- |
| Runner provisioning | Job never starts or fails before any step | Same red status as a quality regression; no eval artifact |
| Stack bring-up | Bring-up step fails; logs only | Same red status |
| `make eval-run` preflight (`eval-run` target) | `eval preflight failed — Go API <url> => <code>, agent <url> => <code>`, exit 1 | Loud and specific. The Go check accepts any HTTP code except 000; the agent check requires 200 |
| Provider preflight (`evals/providers/agent_run.py`, `preflight` / `main`) | Per-sample `ProviderError`, provider exits 1 | A stack that dies mid-run fails later samples one by one |
| Chat model down or still loading | Agent `/healthz` pings Redis and `{LLM_BASE_URL}/v1/models` (`backend/agent_py/agent/main.py:611-637`); returns 503 until the model is reachable | The Makefile preflight blocks before grading. Weak spot: healthz treats only HTTP >= 500 as an LLM failure, so a 404/401 from a mispointed base URL would pass |
| Embedding model down | **No health check anywhere.** The Go backend's RAG retrieval degrades gracefully and continues without context, and the harness preflight never calls `:8001` | RAG cases fail as a rate drop with no infra label. This is the most dangerous silent failure |
| Mid-run agent or LLM error | `evals/bin/check_rate.py` (`sample_verdicts`) turns a provider row `error` into a failed sample; only harness accounting errors reach `report["errors"]` and exit 1 | A rate drop indistinguishable from a model regression; only the per-sample artifact shows the provider error |
| Total provider failure | promptfoo exits with a code other than 0/100; the `eval-run` target refuses to grade | Loud, but still the same red job status as a floor breach |
| Budget stack absent | `main-budget-exceeded` reported SKIPPED with a machine-readable reason | Correct behavior, but a nightly must not read SKIPPED as green |
| Artifact upload failure | Red at the upload step | Distinct if the workflow splits the step; otherwise another undifferentiated red |

The harness has no INFRA / REGRESSION distinction today. That is a harness
change, not a workflow change, which is why §5 requires it.

## 4. Can it fail loudly without training people to ignore it?

Not with the current harness and floors.

- **Known reds are intentional.** This slice ends with three red cases
  (`tasks.md:10`): update and delete are recorded red until the bulk-tool
  sibling lands, and force-search is deferred to slice 2. A nightly gated on
  absolute per-case floors would be red every night from day one.
- **Rate noise is real.** The 2026-10-16 artifact recorded an overall floor of
  0.85, while the fresh baseline is 0.73 and records a 0.70 floor
  (`baseline.md`); the budget case flapped 2/3 then 3/3 before the fresh run
  reproduced 3/3; best-effort cases carry 0.66 floors over three samples.
  Absolute floors flap on model nondeterminism.
- **Partial runs can look perfect.** The newest 2026-10-16 artifact read 1.00
  from a 3-sample partial that exercised only the budget case (design context).
  This change lands the task 7.2 report guard (`baseline.is_baseline` plus the
  `PARTIAL RUN (not a baseline)` summary line), but `decide_exit_code` still
  returns 0 for a non-baseline run when no floor is breached. A nightly must
  gate on the baseline flag, not on the exit code alone.
- **Infra failures look like regressions** (§3). A runner or stack hiccup would
  be triaged as a model quality event.
- **An alert nobody owns is ignored.** A red nightly with no named owner and no
  alert channel is noise by construction.

A trustworthy nightly would need all of the following:

1. A separate readiness job for the stack and both models, so readiness
   failures are INFRA, not REGRESSION.
2. Provider and transport errors classified INCONCLUSIVE and excluded from the
   rate, with one transport retry.
3. A drift gate rather than absolute green while known reds exist: for example,
   breach on two consecutive runs, or an overall rate drop greater than 0.10
   against a rolling median. Known-red cases carry an owner and an expiry and
   are excluded from the gate while remaining visible.
4. The unfiltered normal line plus a separately scheduled budget line.
5. One alert channel, one owner, and a same-day triage expectation.

These are spec and harness changes, not workflow YAML. That is the reason task
9.3 says a viable outcome needs its own change.

## 5. Conditions for viability

Must-have before any workflow is written:

1. **Dedicated GPU host with SM89+ and at least 24 GB VRAM** (L4 24 GB, L40S,
   RTX 4090 24 GB, or H100). Do not use the operator's WSL2 workstation as a
   runner.
2. **Ephemeral, isolated runner.** A dedicated runner group restricted to the
   nightly workflow, no PR-triggered workflow on that runner, no long-lived
   secrets (a scoped short-lived Hugging Face read token at most), egress
   isolation, and pinned images and actions. The repository is public; these
   are requirements, not hardening extras.
3. **In-repo CI bring-up script.** Postgres and Redis (service containers or
   compose), Go build, migrations, agent venv, vLLM chat and embed, readiness
   gates for `/api/health`, the agent `/healthz`, `:8000/v1/models`, and
   `:8001/v1/models`, then teardown and log capture. Cache the vLLM image and
   Hugging Face weights across runs. Do not reuse `scripts/e2e-side-stack.sh`
   as-is: it assumes the operator's infra is already up and starts no Docker or
   vLLM.
4. **Harness classification.** INFRA versus REGRESSION, embedding-endpoint
   readiness in preflight, provider errors not counted as model failures, and
   the task 7.2 baseline flag enforced by the nightly gate (the exit code alone
   is not enough).
5. **Signal design.** The drift gate, known-red annotations, and alert
   ownership from §4.
6. **Budget line scheduled separately**, weekly or as a serial second stack,
   because it needs a different `AGENT_BUDGET_PER_USER_PER_HOUR`. A filtered
   budget result is not a suite baseline.
7. **Cost and ownership.** Roughly $0.15–0.50/night on a third-party spot L4,
   or about $1.50/night on a GitHub GPU SKU if it were available; plus a named
   owner for the runner and the alerts.

If any of 1–4 is missing, the nightly is either unfaithful (different model or
precision), unsafe (public-repo self-hosted runner), or noisy (infra failures
graded as regressions). That is why the recommendation is conditional rather
than yes.

## 6. Task 9.3 record

If the conditions in §5 are ever met, the work needs its own OpenSpec change —
for example `agent-nightly-eval` — with a spec delta to `agent-golden-eval`
covering scheduled execution, the live-gated posture, INFRA/REGRESSION
classification, the drift gate, and artifact retention, plus a runner and
bring-up design. It must not be built in `agent-planner-write-routing`. Link
that change back to this file (task 9.1) and to the runbook section (task 9.2)
rather than duplicating them.

## 7. Manual runbook fallback

This is the text to add under `backend/agent_py/README.md` → Golden Set Eval
Suite.

### Manual live-run runbook

Run the live suite before a release, after any planner / executor / tool /
prompt change, and for a periodic trend. The suite is serial by contract: one
4B vLLM serves every sample. `make eval-golden` pins `--max-concurrency 1`, and
the live Playwright set pins `--workers=1` (`npm run test:e2e:live`). Never run
two live suites against the same model concurrently.

1. **Infra.** `make up` starts Postgres `:5433`, Redis `:6380`, and Jaeger
   `:16686`; confirm with `docker compose ps`.

2. **Models.** In separate terminals run `make dev-vllm-main` (chat `:8000`)
   and `make dev-vllm-embed` (embeddings `:8001`). The first run downloads the
   models. Wait for both before starting:

   ```bash
   curl -fsS http://localhost:8000/v1/models >/dev/null
   curl -fsS http://localhost:8001/v1/models >/dev/null
   ```

   Do not race the cold start. Without `--enforce-eager`, the chat model can
   take 10–15 minutes to capture CUDA graphs.

3. **Tavily stub (recommended).** The keyless live search tier rate-limits and
   stalls web samples. Start the bundled stub and export its URL so the side
   stack's agent inherits it:

   ```bash
   backend/agent_py/.venv/bin/python evals/stubs/tavily_stub.py --port 8899 &
   export PYTHON_AGENT_WEB_SEARCH_ENABLED=true
   export PYTHON_AGENT_TAVILY_BASE_URL=http://127.0.0.1:8899/search
   ```

4. **Side stack.** Start it on spare ports:

   ```bash
   ./scripts/e2e-side-stack.sh up
   ```

   Defaults are Go `:8180`, agent `:9100`, metrics `:9101`, Vite `:5273`. The
   script starts no Docker services and no vLLM; it assumes `make up` and the
   host models are already running. For a second concurrent stack, set
   `E2E_STACK_SUFFIX=-<tag>` and spare `E2E_GO_PORT` / `E2E_AGENT_PORT` /
   `E2E_AGENT_METRICS_PORT` / `E2E_VITE_PORT`. Health checks:

   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8180/api/feeds
   curl -fsS http://localhost:9100/healthz
   curl -fsS http://localhost:8001/v1/models >/dev/null
   ```

   The first should print `401` (up and protected). The second should return
   `{"status":"ok"}`. Curl the embedding endpoint yourself: the eval preflight
   checks the Go API and the agent only, and a down embedder shows up as RAG
   case failures rather than an infra error. If RAG cases fail with embedding
   errors, the side backend may have inherited a compose hostname from
   `backend/.env`; export `LLM_EMBEDDING_BASE_URL=http://localhost:8001/v1`,
   `LLM_EMBEDDING_MODEL=Qwen/Qwen3-Embedding-0.6B`,
   `LLM_EMBEDDING_ALLOW_PRIVATE_URL=true`, and `EMBEDDING_DIMENSIONS=1024`
   before `./scripts/e2e-side-stack.sh up`.

5. **Main line.** Match the harness mode to the running agent:

   ```bash
   EVAL_WEB_SEARCH=on make eval-golden
   ```

   Use `EVAL_WEB_SEARCH=off` only when the agent was started with
   `PYTHON_AGENT_WEB_SEARCH_ENABLED=false`; the weather case is the tripwire.
   The optional injection line runs on the same stack:

   ```bash
   make eval-security
   ```

6. **Budget line.** Use a second side stack with a tiny inherited cap:

   ```bash
   AGENT_BUDGET_PER_USER_PER_HOUR=500 E2E_STACK_SUFFIX=-budget \
     E2E_GO_PORT=8280 E2E_AGENT_PORT=9200 E2E_AGENT_METRICS_PORT=9201 \
     ./scripts/e2e-side-stack.sh up

   EVAL_GO_API_URL=http://localhost:8280 EVAL_AGENT_URL=http://localhost:9200 \
     EVAL_BUDGET_STACK=1 EVAL_FILTER=main-budget-exceeded make eval-golden
   ```

   The side-stack script inherits the unprefixed
   `AGENT_BUDGET_PER_USER_PER_HOUR`. Both side stacks share Redis `:6380`, so
   use the default fresh per-run eval user or clear that user's budget counter
   before the budget run. The filtered run measures only
   `main-budget-exceeded`; its overall rate is not a suite baseline.

7. **Read the artifacts.** A run writes `evals/artifacts/<UTC-stamp>/` with
   `report.json`, `summary.md`, `promptfoo.json`, and per-sample files under
   `samples/`. Read `summary.md`: overall rate, per-case rates, the SKIPPED
   set, and the applied floor with the baseline rate it was derived from.
   `NOT RUN` cases and filtered runs are not a baseline; the summary marks such
   a run `PARTIAL RUN (not a baseline)`. A partial run must not be cited as a
   suite baseline.

8. **Land evidence (optional).** Artifacts are gitignored in bulk. To commit a
   baseline, force-add only the report and summary:

   ```bash
   git add -f evals/artifacts/<stamp>/report.json evals/artifacts/<stamp>/summary.md
   ```

9. **Cleanup.** Stop only what you started:

   ```bash
   E2E_STACK_SUFFIX=-budget ./scripts/e2e-side-stack.sh stop
   ./scripts/e2e-side-stack.sh stop
   ```

   If you started the host vLLM targets for this run, `make stop`. Do not stop
   the operator's own dev stack.

Known reds at this slice, not new regressions: `main-rag-update-feed` and
`main-rag-delete-feed` are recorded red pending `agent-feed-bulk-write-tools`;
`main-web-force-search` is deferred to slice 2. `main-budget-exceeded` is
SKIPPED on the normal stack by design.

## 8. Sources

Internal evidence:

- `openspec/changes/agent-planner-write-routing/tasks.md:10` (three red cases at
  slice end), `design.md` Decision 6 (scheduled run non-goal; spike rationale).
- `.github/workflows/ci.yml` (all jobs `ubuntu-latest`; no schedule trigger;
  `agent-lint` now runs the eval unit gate and filters on `evals/**`).
- `Makefile` (`eval-run` target: preflight, serial `--max-concurrency 1`,
  artifact handling; `EVAL_*` defaults).
- `evals/promptfooconfig.yaml` (`maxConcurrency: 1`).
- `evals/providers/agent_run.py` (`preflight`; provider `main` exit codes).
- `evals/bin/check_rate.py` (`sample_verdicts`: a provider row error becomes a
  failed sample; `decide_exit_code`; `baseline_status`).
- `backend/agent_py/agent/main.py` (`healthz` pings Redis and the chat LLM, not
  the embedder).
- `docker-compose.yml` (vLLM service, healthcheck budget, eager-mode comment;
  embed service).
- `scripts/dev.sh` (host vLLM commands, first-run downloads).
- `scripts/e2e-side-stack.sh` (side-stack assumptions and ports).
- `evals/cases/main.yaml` (`main-budget-exceeded`, `requires: budget_stack`).
- `backend/agent_py/README.md` Golden Set Eval Suite (current suite docs; the
  §7 runbook is the text to add there).
- `openspec/changes/agent-planner-write-routing/baseline.md` (7m12s normal run,
  21s filtered budget run, rates, known reds).

External facts (checked 2026-10-22; untrusted data):

- GitHub Docs, "Larger runners reference" — GPU larger runner is a 4-CPU
  Tesla T4 with 16 GB VRAM, 28 GB RAM.
  <https://docs.github.com/en/actions/reference/runners/larger-runners>
- GitHub Docs, "Controlling access to larger runners" — larger runners are
  only available to organizations and enterprises on GitHub Team or GitHub
  Enterprise Cloud.
  <https://docs.github.com/en/actions/how-tos/manage-runners/larger-runners/control-access>
- GitHub Changelog, 2024-10-17 — runner groups on free orgs cover self-hosted
  runners; GitHub-hosted larger runners remain unavailable to free
  organizations.
  <https://github.blog/changelog/2024-10-17-actions-runner-groups-now-available-for-organizations-on-free-plan/>
- GitHub Docs, self-hosted runner security — self-hosted runners should almost
  never be used for public repositories; fork pull requests can execute code on
  them.
  <https://docs.github.com/en/actions/concepts/runners/self-hosted-runners>
- vLLM documentation, FP8 W8A8 — requires compute capability 8.9+ (Ada,
  Hopper, Blackwell); Turing and Ampere support only weight-only FP8 paths.
  <https://docs.vllm.ai/en/stable/features/quantization/llm_compressor/fp8/>
- GitHub Actions 2026 rate card, Linux 4-core GPU ~$0.052/minute and always
  billed (secondary sources; verify before budgeting).
  <https://docs.github.com/en/billing/managing-billing-for-your-products/managing-billing-for-github-actions/about-billing-for-github-actions>
