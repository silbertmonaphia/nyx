#!/usr/bin/env bash
# Bootstrap vLLM with NVIDIA GPU passthrough on WSL2 / Linux.
# Run from the repo root as root:  sudo ./scripts/bootstrap-vllm-gpu.sh
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "must run as root: sudo $0" >&2; exit 1; }
cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.."
[[ -f docker-compose.yml ]] || { echo "docker-compose.yml not found at $(pwd)" >&2; exit 1; }

# Resolve nvidia-smi to an absolute path before the check: WSL2 (path:
# /usr/lib/wsl/lib/nvidia-smi) and CUDA-toolkit installs (path:
# /usr/local/cuda/bin/nvidia-smi) live outside root's default PATH, so a
# bare `nvidia-smi -L` returns 127 under `sudo` even when the GPU is
# visible to the invoking user. `command -v` covers the user's PATH;
# the explicit fallbacks cover the sudo case.
nsmi=$(command -v nvidia-smi 2>/dev/null || true)
[[ -z $nsmi ]] && for p in /usr/lib/wsl/lib/nvidia-smi /usr/local/cuda/bin/nvidia-smi /usr/bin/nvidia-smi; do
  [[ -x $p ]] && { nsmi=$p; break; }
done
[[ -n $nsmi ]] || { echo "nvidia-smi not found (looked in \$PATH, /usr/lib/wsl/lib, /usr/local/cuda/bin, /usr/bin)" >&2; exit 1; }
"$nsmi" -L >/dev/null 2>&1 || { echo "no NVIDIA GPU visible (nvidia-smi failed — is the driver loaded?)" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "docker compose plugin missing" >&2; exit 1; }
command -v apt-get >/dev/null || { echo "this script targets Debian/Ubuntu (apt-get missing)" >&2; exit 1; }
[[ -f .env ]] || { echo ".env not found at $(pwd); copy .env.example first" >&2; exit 1; }

# 1. Install nvidia-container-toolkit (idempotent).
if ! dpkg -s nvidia-container-toolkit >/dev/null 2>&1; then
  install -d /usr/share/keyrings
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit
fi

# 2. Register the nvidia runtime with dockerd (preserves existing daemon.json keys).
nvidia-ctk runtime configure --runtime=docker

# 3. Restart dockerd. Tries systemd unit names, then SysV `service`, then a
#    manual restart for any system where dockerd is unmanaged (WSL2 without
#    systemd, OpenRC, runit, containerized CI runners, …).
if command -v systemctl >/dev/null; then
  systemctl restart docker.service 2>/dev/null \
    || systemctl restart docker 2>/dev/null \
    || service docker restart 2>/dev/null \
    || { pkill -x dockerd 2>/dev/null || true; sleep 3; nohup dockerd </dev/null >/tmp/dockerd.log 2>&1 & }
else
  service docker restart 2>/dev/null \
    || { pkill -x dockerd 2>/dev/null || true; sleep 3; nohup dockerd </dev/null >/tmp/dockerd.log 2>&1 & }
fi
for _ in {1..30}; do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || { echo "dockerd did not come back up; check /tmp/dockerd.log" >&2; exit 1; }

# 4. Sanity-check: confirm a container can actually see the GPU.
docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L

# 5. Flip the LLM_* keys in .env to the values docker-compose.yml expects
#    for the `vllm` profile. Permissive regex handles `KEY=v`, `# KEY=v`,
#    `export KEY=v`, and `KEY = v` so re-runs stay idempotent. Appends
#    only if the key has no line at all. .env is backed up first so a
#    malformed input never costs the user their JWT/DB credentials.
#    Timestamped filename keeps a history of the last 5 runs.
bak=".env.bak.$(date +%Y%m%d%H%M%S)"
cp -p .env "$bak"
ls -1t .env.bak.[0-9]* 2>/dev/null | tail -n +6 | xargs -r rm --
for kv in \
  LLM_ENABLED=true \
  LLM_PROVIDER=vllm \
  LLM_BASE_URL=http://vllm:8000/v1 \
  LLM_MODEL=Qwen/Qwen3-4B-Instruct-2507 \
  LLM_ALLOW_PRIVATE_URL=true \
  LLM_EMBEDDING_PROVIDER=vllm \
  LLM_EMBEDDING_BASE_URL=http://vllm-embed:8000/v1 \
  LLM_EMBEDDING_MODEL=Qwen/Qwen3-Embedding-0.6B \
  LLM_EMBEDDING_ALLOW_PRIVATE_URL=true \
  EMBEDDING_DIMENSIONS=1024; do
  k=${kv%%=*}
  sed -i -E "s|^[[:space:]]*#?[[:space:]]*(export[[:space:]]+)?${k}[[:space:]]*=.*|${kv}|" .env
  grep -qE "^[[:space:]]*${k}=" .env || printf '%s\n' "$kv" >> .env
done

# 6. Bring up the stack. Down first to clear stale network state from a
#    previous interrupted run (e.g., dockerd restart between runs can
#    leave the compose-managed network in an inconsistent state, and
#    `up` then fails with "network X not found"). Tolerate the down
#    failing — it returns non-zero when nothing is running.
docker compose --profile vllm --profile embed down --remove-orphans 2>/dev/null || true
docker compose --profile vllm --profile embed up --build -d
# `docker compose ps` without a service arg also shows the `backend`
# container with `Skipped: optional dependency "vllm"` (compose.yml:137
# sets `required: false`), which is just backend waiting for vllm and
# unrelated to vllm's own health. Filter to vllm only.
docker compose ps vllm vllm-embed

# 7. Wait for vLLM to become healthy (compose healthcheck on /v1/models).
#    Compose healthcheck total budget = start_period (600s) + retries (60) ×
#    interval (10s) = 1200s = 20 min. Wait 25 min so we catch BOTH `healthy`
#    AND `unhealthy` outcomes; otherwise we time out before compose does
#    and report a fake failure for a container that's still capturing
#    cudagraphs (cold capture of V2 Model Runner sizes up to 512 takes
#    ~10-15 min on Blackwell + WSL2 GPU-PV because each capture
#    round-trips through the DXGI shim on Blackwell + WSL2 GPU-PV).
#    On any non-healthy exit, dump the vllm log inline so the operator
#    doesn't have to run a second command. Exit code reflects the outcome
#    so CI wrappers can detect failure.
dump_vllm_log() {
  local svc=$1
  echo "----- last 80 lines of $svc log -----"
  docker compose logs --no-color --tail=80 "$svc" 2>&1 || echo "(could not read $svc log)"
  echo "----- end $svc log -----"
}
wait_healthy() {
  local svc=$1
  local budget_min=$2
  local attempts=$((budget_min * 12))   # 5s interval → 12 attempts/min
  echo
  echo "Waiting for $svc to become healthy (up to ${budget_min} min)..."
  for _ in $(seq 1 $attempts); do
    cid=$(docker compose ps -q "$svc" 2>/dev/null | head -1 || true)
    status=unknown
    [[ -n $cid ]] && status=$(docker inspect --format='{{.State.Health.Status}}' "$cid" 2>/dev/null || echo unknown)
    case "$status" in
      healthy)   echo "✓ $svc is healthy."; return 0 ;;
      unhealthy) echo "! $svc is unhealthy."; dump_vllm_log "$svc"; return 1 ;;
    esac
    sleep 5
  done
  echo "! $svc did not become healthy within ${budget_min} min."
  dump_vllm_log "$svc"
  return 1
}
# Chat container: long cold start (model load + cudagraph capture).
wait_healthy vllm 25 || exit 1
# Embed container: smaller model, much faster cold start. The embed
# service can race the chat service's cudagraph capture without
# affecting it (separate container, separate memory slice), so we
# don't gate the embed wait on the chat one.
wait_healthy vllm-embed 10 || exit 1
exit 0