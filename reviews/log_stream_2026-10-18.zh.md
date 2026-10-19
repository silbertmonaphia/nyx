# Nyx 日志流现状与 12-Factor 对照（2026-10-18）

## 当前日志流

```
┌─ Go backend (process) ──────────────────────────────┐
│ zerolog → JSON lines → os.Stdout (unbuffered)       │
│   request log: request_id, method, path, status,    │
│   bytes, duration, client_ip, trace_id, span_id     │
│   error log:   Warn/Error + request_id              │
└──────────────────┬──────────────────────────────────┘
                   │ stdout
┌─ Python agent (process) ────────────────────────────┐
│ stdlib logging → 纯文本 → stderr (root=WARNING,     │
│   nyx.agent.* / agent.* = INFO)                     │
│ uvicorn access log → stdout (纯文本)                │
│ OTel spans → Jaeger；Prometheus → :9001/metrics     │
└──────────────────┬──────────────────────────────────┘
                   │ stdout/stderr
┌─ SPA / nginx ──────────────────────────────────────┐
│ browser: logger.ts → JSON line → console.*          │
│          Sentry（仅 3 处 captureException）          │
│          OTel spans → /otlp/ → Jaeger               │
│ nginx:   access/error → /dev/stdout|stderr（镜像默认）│
└──────────────────┬──────────────────────────────────┘
                   │
        Docker json-file driver（无 logging 配置）
        dev: scripts/dev.sh `2>&1 | tee /tmp/nyx-*.log`
```

后端日志入口：`zerolog` 全局 logger，输出重定向到 stdout，时间戳用 Unix 秒（`backend/cmd/api/main.go:42-43`）。每请求一行在 `backend/internal/middleware/logging.go:45`，trace context 有效时补 `trace_id`/`span_id`；panic 在 `recoverer.go:25` 记录并带 request_id；错误响应的内部细节走 `api.ClassifyAndLog` 只打日志不上线（`platform/api/response.go:38`）。SSE `/api/chat` 也走同一条 Logging 中间件，所以 duration 是整条流的总时长。

Agent 日志在 `agent_py/agent/main.py:90-113`：root 设为 WARNING（压掉 httpx 等第三方 INFO），`nyx.agent.*` 和 `agent.*` 提到 INFO，格式是 `%(asctime)s %(levelname)s %(name)s: %(message)s`，纯文本。审计日志（GDPR export/erase）写 Postgres `agent.data_audit_log`（`agent/audit.py`），属于合规数据，不是运行日志流。

前端 `services/logger.ts` 输出单行 JSON（含 `trace_id`/`span_id`）到 console；`main.tsx:17-33` 注册了 `window.error` 和 `unhandledrejection`；Sentry 只覆盖 ErrorBoundary、axios 5xx、unhandled rejection 三个点。OTel 只导 trace，不导日志。

## 对照 12-Factor XI（Logs）

核心原则：日志是事件流；应用不应自己管理日志文件；每个进程把无缓冲事件流写到 stdout，由执行环境负责捕获、汇聚、路由和保留。

| 原则 | 现状 | 评价 |
|---|---|---|
| 应用不写/轮转日志文件 | Go、agent、SPA 都不写文件；dev 的 `/tmp/nyx-*.log` 是 `scripts/dev.sh:245-260` 用 `tee` 捕获的 | ✅ |
| 每进程无缓冲写到 stdout | Go 直接写 stdout ✅；agent 应用日志走 **stderr**，uvicorn access 走 stdout，两股流 | ⚠️ |
| 执行环境捕获并汇聚 | Docker 默认 json-file（每个容器一份，未配 `max-size`/`max-file`）；dev 用 tee；**没有 Fluent Bit/Vector/Loki/OTel Logs 之类的汇聚层** | ⚠️ |
| 统一事件流、可关联 | Go 是 JSON；agent/uvicorn/nginx 是纯文本；字段集合不一致 | ⚠️ |
| 本地开发在前台看流 | `make dev` 前台或 detach 后 tee 到 `/tmp`；`make logs` 只 tail 基础设施容器 | ✅ |

结论：**核心纪律是符合的**——应用不管理文件、输出到进程 stdout/stderr、由 Docker/启动脚本捕获。缺的是 Factor XI 后半段“由执行环境统一汇聚、路由到最终目的地并管理保留”的那一层：目前停在 Docker per-container json-file，且没有轮转上限。

## 主要差距（按优先级）

1. **没有日志汇聚/路由层，也没有 Docker 日志轮转上限。** `docker-compose.yml` 里没有任何 `logging:` 配置，默认 json-file 无界增长；`--profile app` 长期运行会吃盘。Jaeger 只存 trace，Langfuse 只存 LLM trace，Sentry 只收前端异常——日志没有单一入口。
2. **跨运行时关联断裂。** Go 请求日志有 `request_id`/`trace_id`，但 chat 代理转发到 agent 时只设 `X-Nyx-User-Id` + `Authorization`（`backend/internal/chat/agent_proxy.go:153`、`sessions_proxy.go:181`），没有转发 `X-Request-ID`；而 agent 在 `main.py:690` 读这个 header 并放进 error frame。结果 agent 侧 request_id 为 null，两边日志无法按请求串起来；agent 日志本身也不注入 trace_id。
3. **格式不统一。** Go 是 JSON，agent/uvicorn/nginx 是纯文本。要接 Loki/ClickHouse 之类的系统，必须先在采集端做解析或改 agent 的 formatter（`python-json-logger`/structlog + logging filter 注入 trace/request id）。
4. **没有日志级别配置。** 代码里没有 `zerolog.SetGlobalLevel`，也没有 `LOG_LEVEL`；agent 级别硬编码在 `configure_logging()`。12-Factor III（Config）角度不理想，运维无法按环境降噪。
5. **前端日志的实际归宿。** `logger.info/warn` 只到 console，不进 Sentry；生产浏览器 console 不保证被采集。目前 Sentry 只覆盖异常，OTel 只覆盖 trace——warn 级事件（如 `chat.session.*_failed`）在生产没有落点。
6. **PII 面。** Logging 中间件原样记录 query string，`SECURITY.md` L4 已记录；统一采集后需要 shipper 侧脱敏/保留策略，否则 PII 会扩散到聚合系统。
7. **小问题：** `recoverer.go` 用 `Bytes("stack", debug.Stack())`，zerolog 会把 `[]byte` base64 编码——panic 栈在 JSON 里是可解码的 base64 而不是可读文本，排障时多一步。

如果要向标准 12-Factor 日志流靠拢，最小改动顺序是：compose 加轮转 → 统一 agent 为 JSON + 注入 trace/request id → 代理转发 `X-Request-ID` → 加 `LOG_LEVEL` → 再接一个采集器（OTel Collector 的 logs pipeline 或 Fluent Bit → Loki）。
