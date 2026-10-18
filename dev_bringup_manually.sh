# Infra(postgresql, redis, obervability-otel:jaeger) 
sudo docker compose up -d # `-d`` run as background

# LLM
make dev-vllm-main
make dev-vllm-embed

# App
make dev-agent
make dev-backend
make dev-frontend

# inspect infra
make dev-pgsql-inspect
make dev-redis-inspect

# Langfuse default down
# Langfuse up (langfuse is python agent otel)
# sudo docker compose --profile langfuse up -d
# Langfuse down(untouch db/redis/jaeger)
# sudo docker compose --profile langfuse down
