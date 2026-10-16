# Root dev orchestrator. `make dev` runs backend + frontend + agent in
# the background with shared Ctrl+C cleanup. The `make dev-*` singles
# (backend / frontend / agent / vllm-main / vllm-embed) run one process
# in the foreground by default — output streams live to your terminal
# and Ctrl+C kills it. Pass DETACH=1 to any single target to background
# it instead (output also tees to /tmp/nyx-*.log). The two vllm targets
# serve the chat / embedding models on the HOST for a faster dev loop
# than the compose `vllm` / `embed` profiles (needs a local vLLM
# install + GPU; do not run alongside the compose profiles).
# `dev-infra` runs the infra containers (db + redis + jaeger) in the
# foreground — Ctrl+C stops them; `up` is the detached, idempotent
# variant. `up-langfuse` / `down-langfuse` manage the self-hosted
# Langfuse tracing stack (opt-in; ~6 GB RAM — web alone needs >1g to
# boot) separately from the base infra. `dev-pgsql-inspect` /
# `dev-redis-inspect` open psql / redis-cli shells against the running
# infra. Run from the repo root; requires `go`, `npm`, `python3`, and
# the `docker compose` plugin on PATH. Backend invocations assume
# `make up` has been run.

.PHONY: dev dev-backend dev-frontend dev-agent dev-vllm-main dev-vllm-embed dev-infra dev-pgsql-inspect dev-redis-inspect dev-gpu-inspect dev-gpu-watch up down up-vllm down-vllm up-langfuse down-langfuse logs status stop help eval-golden eval-security eval-run

COMPOSE := docker compose

# Expanded to `--detach` when DETACH=1 is passed on the command line,
# empty otherwise. Lets the singles run in foreground by default and
# opt into backgrounding without a second target.
DETACH_FLAG := $(if $(DETACH),--detach)

# Tiny self-documenting help: scan the comment block above each
# `#: <description>` marker and emit `  <target>  <description>`.
help:
	@awk 'BEGIN{FS=":"} /^#:/{desc=$$2; sub(/^ */,"",desc); next} \
	  /^[a-z][a-z0-9-]*:/{ if (desc!="") { printf "  \033[36m%-7s\033[0m %s\n", $$1, desc; desc="" } }' $(MAKEFILE_LIST)
	@echo
	@echo "dev logs:      tail -f /tmp/nyx-backend.log /tmp/nyx-frontend.log /tmp/nyx-agent.log"
	@echo "vllm logs:     tail -f /tmp/nyx-vllm-main.log /tmp/nyx-vllm-embed.log"
	@echo "DETACH=1:      background the dev-{backend,frontend,agent,vllm-main,vllm-embed} target instead of foreground."

#: Stop the dev backend + frontend + agent + host vLLM processes started by any `make dev*` target. No-op if nothing is running.
stop:
	@for f in /tmp/nyx-backend.pid /tmp/nyx-frontend.pid /tmp/nyx-agent.pid \
	          /tmp/nyx-vllm-main.pid /tmp/nyx-vllm-embed.pid; do \
	  [ -f "$$f" ] && kill "$$(cat $$f)" 2>/dev/null && rm -f "$$f" || true; \
	done

#: Show running infra containers + dev backend/frontend/agent/vllm processes.
status:
	@$(COMPOSE) ps
	@echo "--- dev processes ---"
	@pgrep -af 'go run ./cmd/api' || echo "(backend not running)"
	@pgrep -af 'vite'             || echo "(frontend not running)"
	@pgrep -af 'agent.main'        || echo "(agent not running)"
	@pgrep -af 'serve Qwen'       || echo "(host vllm not running)"

#: Run backend + frontend + agent in background with shared Ctrl+C cleanup. Assumes `make up` has been run.
dev:
	@./scripts/dev.sh all --detach

#: Run the backend in the foreground (DETACH=1 to background). Assumes `make up` has been run.
dev-backend:
	@./scripts/dev.sh backend $(DETACH_FLAG)

#: Run the frontend in the foreground (DETACH=1 to background). No infra needed.
dev-frontend:
	@./scripts/dev.sh frontend $(DETACH_FLAG)

#: Run the Python agent service in the foreground (DETACH=1 to background). Assumes `make up` has been run.
dev-agent:
	@./scripts/dev.sh agent $(DETACH_FLAG)

#: Run the vLLM chat model on the host (port 8000, Qwen3-4B-Instruct FP8, hermes tool parser, ~6970 MiB VRAM). Foreground by default (DETACH=1 to background); host-run alternative to the compose vllm profile — don't run both.
dev-vllm-main:
	@./scripts/dev.sh vllm-main $(DETACH_FLAG)

#: Run the vLLM embedding model on the host (port 8001, Qwen3-Embedding-0.6B, ~3224 MiB VRAM). Foreground by default (DETACH=1 to background); host-run alternative to the compose embed profile — don't run both.
dev-vllm-embed:
	@./scripts/dev.sh vllm-embed $(DETACH_FLAG)

#: Bring the infra containers (db + redis + jaeger) up in the foreground; Ctrl+C stops them. Run `make up` to detach instead.
dev-infra:
	$(COMPOSE) up db redis jaeger

#: Bring the infra containers (db + redis + jaeger) up detached; idempotent, safe to re-run. Prerequisite for the `dev-*` targets.
up:
	$(COMPOSE) up -d db redis jaeger

#: Bring up the self-hosted Langfuse tracing stack (langfuse-web/worker + clickhouse/minio/redis/postgres). Detached; idempotent. Needs the LANGFUSE_* vars in .env (see .env.example). UI on http://localhost:3000.
up-langfuse:
	$(COMPOSE) --profile langfuse up -d langfuse-web langfuse-worker

#: Stop the Langfuse stack only — db/redis/jaeger keep running; volumes preserved. Re-up with `make up-langfuse`.
down-langfuse:
	$(COMPOSE) --profile langfuse stop langfuse-web langfuse-worker langfuse-db langfuse-redis langfuse-clickhouse langfuse-minio

#: Start the vLLM chat + embed containers (full local LLM stack). Requires `make up` already running.
up-vllm:
	$(COMPOSE) --profile vllm --profile embed up -d

#: Stop the vLLM chat + embed containers (preserves volumes).
down-vllm:
	$(COMPOSE) --profile vllm --profile embed down

#: Stop the infra containers (preserves volumes).
down:
	$(COMPOSE) down

#: Tail infra logs from db / redis / jaeger.
logs:
	$(COMPOSE) logs -f --tail=100

#: Open a psql shell against the dev Postgres — `psql -h 127.0.0.1 -p 5433 -U postgres -d nyx`; prompts for the POSTGRES_PASSWORD from .env.
dev-pgsql-inspect:
	psql -h 127.0.0.1 -p 5433 -U postgres -d nyx

#: Open a redis-cli shell against the dev Redis — `redis-cli -h 127.0.0.1 -p 6380`.
dev-redis-inspect:
	redis-cli -h 127.0.0.1 -p 6380

#: Show GPU state via nvidia-smi — memory, utilization, and per-process usage (useful to confirm the host vLLM targets freed VRAM).
dev-gpu-inspect:
	@PATH="$$PATH:/usr/lib/wsl/lib" nvidia-smi

#: Watch GPU telemetry live — `nvidia-smi dmon` at 1s refresh (power, temp, sm%/VRAM-bandwidth%, clocks, VRAM used). COUNT=<n> takes n samples then exits; DELAY=<s> overrides the interval; Ctrl+C stops.
dev-gpu-watch:
	@PATH="$$PATH:/usr/lib/wsl/lib" nvidia-smi dmon -s pucvmet $(if $(DELAY),-d $(DELAY)) $(if $(COUNT),-c $(COUNT))

# --- Golden-set eval (evals/) -----------------------------------------------
# EVAL_FILTER (promptfoo --filter-pattern) is a diagnostic lever: a filtered
# run reports only the sampled cases, so its exit code is not a gate — run the
# unfiltered line before trusting a green.
# Serial by contract (one 4B vLLM). These targets never start services: they
# preflight the URLs below and stop with a message when the stack is down.
# Defaults point at the side-stack convention (scripts/e2e-side-stack.sh):
# Go :8180, agent :9100. Override with EVAL_GO_API_URL / EVAL_AGENT_URL.

EVAL_GO_API_URL ?= http://localhost:8180
EVAL_AGENT_URL ?= http://localhost:9100
EVAL_WEB_SEARCH ?= on
# Overall floor derived from the 2026-10-16 baseline (0.73) minus margin; per-case
# floors stay authored and carry the intent (1.0 deterministic shapes, 0.66 best-effort).
EVAL_MIN_RATE ?= 0.70
EVAL_MODEL ?= Qwen/Qwen3-4B-Instruct-2507-FP8
EVAL_PROMPTFOO_VERSION ?= 0.124.1
EVAL_PYTHON ?= backend/agent_py/.venv/bin/python
EVAL_CONFIG ?= evals/promptfooconfig.yaml
EVAL_CORPUS_GLOB ?= evals/cases/main.yaml
EVAL_FILTER ?=
EVAL_NO_FAIL ?=

#: Run the golden-set regression line serially against a running stack (defaults to the side stack; does NOT start services).
eval-golden:
	@$(MAKE) --no-print-directory eval-run EVAL_CONFIG=evals/promptfooconfig.yaml EVAL_CORPUS_GLOB=evals/cases/main.yaml

#: Run the security (injection) line — same preflight and stack as eval-golden; failure rate tracked separately.
eval-security:
	@$(MAKE) --no-print-directory eval-run EVAL_CONFIG=evals/promptfooconfig.security.yaml EVAL_CORPUS_GLOB=evals/cases/security.yaml

eval-run:
	@set -e; \
	go_code=$$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$(EVAL_GO_API_URL)/api/feeds" || true); \
	agent_code=$$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$(EVAL_AGENT_URL)/healthz" || true); \
	if [ "$$go_code" = "000" ] || [ "$$agent_code" != "200" ]; then \
		echo "eval preflight failed — Go API $(EVAL_GO_API_URL)/api/feeds => $$go_code, agent $(EVAL_AGENT_URL)/healthz => $$agent_code"; \
		echo "start a stack first, e.g. ./scripts/e2e-side-stack.sh up (this target never starts services)"; \
		exit 1; \
	fi; \
	stamp=$$(date -u +%Y%m%dT%H%M%SZ); \
	dir="$$(pwd)/evals/artifacts/$$stamp"; \
	mkdir -p "$$dir/samples"; \
	echo "eval $$stamp → $$dir (config $(EVAL_CONFIG), EVAL_WEB_SEARCH=$(EVAL_WEB_SEARCH))"; \
	$(EVAL_PYTHON) evals/validate.py; \
	EVAL_RUN_STAMP=$$stamp EVAL_ARTIFACTS_DIR="$$dir" \
	EVAL_GO_API_URL=$(EVAL_GO_API_URL) EVAL_AGENT_URL=$(EVAL_AGENT_URL) EVAL_WEB_SEARCH=$(EVAL_WEB_SEARCH) \
	EVAL_BUDGET_STACK=$(EVAL_BUDGET_STACK) EVAL_MODEL=$(EVAL_MODEL) \
	npx -y promptfoo@$(EVAL_PROMPTFOO_VERSION) eval --config $(EVAL_CONFIG) \
	  --max-concurrency 1 --no-progress-bar $(if $(EVAL_FILTER),--filter-pattern "$(EVAL_FILTER)",) \
	  --output "$$dir/promptfoo.json" || pf=$$?; \
	if [ ! -s "$$dir/promptfoo.json" ]; then echo "promptfoo produced no results (exit $${pf:-1})"; exit 1; fi; \
	if [ -n "$${pf:-}" ] && [ "$$pf" != "100" ]; then \
		echo "promptfoo exited $$pf (0 = all passed, 100 = assertion failures; anything else means the run itself failed) — refusing to grade a partial run"; \
		exit 1; \
	fi; \
	EVAL_MODEL=$(EVAL_MODEL) \
	$(EVAL_PYTHON) evals/bin/check_rate.py "$$dir/promptfoo.json" \
	  --corpus-glob "$(EVAL_CORPUS_GLOB)" --artifacts-dir "$$dir" --min-rate $(EVAL_MIN_RATE) \
	  $(if $(EVAL_NO_FAIL),--no-fail,)
