# Root dev orchestrator. `make dev` runs backend + frontend + (when
# AGENT_ENABLED=true in backend/.env) the Python agent service in
# the background with shared Ctrl+C cleanup. `make dev-backend` /
# `make dev-frontend` / `make dev-agent` run a single half in the
# foreground by default — output streams live to your terminal and
# Ctrl+C kills it. Pass DETACH=1 to any single target to background
# it instead (output also tees to /tmp/nyx-*.log). Run from the
# repo root; requires `go`, `npm`, `python3`, and the `docker
# compose` plugin on PATH. Backend invocations assume `make up`
# has been run.

.PHONY: dev dev-backend dev-frontend dev-agent up down up-vllm down-vllm logs status stop help

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
	@echo "DETACH=1:      background the dev-{backend,frontend,agent} target instead of foreground."

#: Stop the dev backend + frontend + agent started by any `make dev*` target. No-op if nothing is running.
stop:
	@for f in /tmp/nyx-backend.pid /tmp/nyx-frontend.pid /tmp/nyx-agent.pid; do \
	  [ -f "$$f" ] && kill "$$(cat $$f)" 2>/dev/null && rm -f "$$f" || true; \
	done

#: Show running infra containers + dev backend/frontend/agent processes.
status:
	@$(COMPOSE) ps
	@echo "--- dev processes ---"
	@pgrep -af 'go run ./cmd/api' || echo "(backend not running)"
	@pgrep -af 'vite'             || echo "(frontend not running)"
	@pgrep -af 'agent.main'        || echo "(agent not running)"

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
