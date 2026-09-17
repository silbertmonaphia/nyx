# Root dev orchestrator. `make dev` starts the backend + frontend
# against the local docker-compose infra (db / redis / jaeger). Run
# from the repo root; requires `go`, `npm`, and the `docker compose`
# plugin on PATH.

.PHONY: dev up down logs status stop help

COMPOSE := docker compose

# Tiny self-documenting help: scan the comment block above each
# `#: <description>` marker and emit `  <target>  <description>`.
help:
	@awk 'BEGIN{FS=":"} /^#:/{desc=$$2; sub(/^ */,"",desc); next} \
	  /^[a-z][a-z0-9-]*:/{ if (desc!="") { printf "  \033[36m%-7s\033[0m %s\n", $$1, desc; desc="" } }' $(MAKEFILE_LIST)
	@echo
	@echo "infra logs:    tail -f /tmp/nyx-backend.log  /tmp/nyx-frontend.log"

#: Start the infra containers (db / redis / jaeger). Idempotent.
up:
	$(COMPOSE) up -d db redis jaeger

#: Stop the infra containers (preserves volumes).
down:
	$(COMPOSE) down

#: Tail infra logs from db / redis / jaeger.
logs:
	$(COMPOSE) logs -f --tail=100

#: Show running infra containers + dev backend/frontend processes.
status:
	@$(COMPOSE) ps
	@echo "--- dev processes ---"
	@pgrep -af 'go run ./cmd/api' || echo "(backend not running)"
	@pgrep -af 'vite'             || echo "(frontend not running)"

#: Run backend + frontend with shared Ctrl+C cleanup. Assumes `make up` has been run.
dev:
	@./scripts/dev.sh

#: Stop the dev backend + frontend started by `make dev`. No-op if nothing is running.
stop:
	@for f in /tmp/nyx-backend.pid /tmp/nyx-frontend.pid; do \
	  [ -f "$$f" ] && kill "$$(cat $$f)" 2>/dev/null && rm -f "$$f" || true; \
	done