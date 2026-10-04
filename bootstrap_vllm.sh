# Bootstrap Vllm in wls2 (chat model on :8000). The Nyx embedder
# also needs a separate embedding endpoint on :8001 — Qwen3-8B-FP8
# is chat-tuned and does NOT serve /v1/embeddings, so every embed
# call would 404. Start a second vllm serve on :8001 (see the
# suggested command below) and point backend/.env at:
#   LLM_EMBEDDING_BASE_URL=http://localhost:8001/v1
#   LLM_EMBEDDING_MODEL=Qwen/Qwen3-Embedding-0.6B
#   LLM_EMBEDDING_ALLOW_PRIVATE_URL=true
#   EMBEDDING_DIMENSIONS=1024

export HF_ENDPOINT=https://hf-mirror.com # not work in wsl2 unset it works
export HF_HUB_DISABLE_IPV6=1
export HUGGING_FACE_HUB_TOKEN=

vllm serve Qwen/Qwen3-8B-FP8 --host 0.0.0.0 --port 8000 --tensor-parallel-size 1 --gpu-memory-utilization 0.7 --max-model-len 4096 # 14043MiB - 3224MiB = 10,819MiB vram

vllm serve Qwen/Qwen3-4B-Instruct-2507-FP8 --host 0.0.0.0 --port 8000 --tensor-parallel-size 1 --gpu-memory-utilization 0.9 --max-model-len 4096 #  6970MiB vram

vllm serve Qwen/Qwen3-Embedding-0.6B --port 8001 --enforce-eager --gpu-memory-utilization 0.1 --max-model-len 256 # 3224MiB vram

# Suggested second vllm (different shell / different port):
# vllm serve Qwen/Qwen3-Embedding-0.6B --host 0.0.0.0 --port 8001 --tensor-parallel-size 1 --gpu-memory-utilization 0.15 --max-model-len 512 --enforce-eager

# test with simple question
curl -v --noproxy '*' http://127.0.0.1:8000/v1/chat/completions -H "Content-Type: application/json" -d '{"model": "Qwen/Qwen3-8B-FP8","messages": [{"role": "user", "content": "测试？"}]}'

curl -v --noproxy '*' http://127.0.0.1:8000/v1/chat/completions -H "Content-Type: application/json" -d '{"model": "Qwen/Qwen3-4B-Instruct-2507-FP8","messages": [{"role": "user", "content": "测试？"}]}'

# test if the model support embeddings
curl -s -X POST http://127.0.0.1:8000/v1/embeddings -H "Content-Type: application/json" -d '{"model":"Qwen/Qwen3-Embedding-0.6B","input":["hi"]}' | head -c 300


# test the seperated embedding model is working
curl localhost:8001/v1/embeddings -d '{"model":"Qwen/Qwen3-Embedding-0.6B","input":["你好"]}'
curl -fsS http://127.0.0.1:8001/v1/models | python3 -c "import json,sys; print(json.load(sys.stdin)['data'][0]['id'])"