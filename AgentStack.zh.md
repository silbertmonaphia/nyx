# Nyx Agent 技术栈 — 已有 vs 行业对照

本文档对照 `backend/agent_py/` 的实际代码与社区主流 agent 开发生态,
盘点 Nyx 已具备的能力、下一切片要补的活、以及结构性缺口。
详细 roadmap 见 `FUTURE_AGENT.md`,架构学习笔记见 `AgentArchitecture.md`,
故障面索引见 `AgentFaultTolerance.md`。

---

## 一、已经有的

### 1. 编排层 (LangGraph Plan-and-Execute)

- **StateGraph** (`agent/graph.py:80`) — `START → planner → executor → END`
- **planner** (`agent/planner.py`) — 单次 LLM 调用,通过 OpenAI `response_format.json_schema` 显式 bind (`strict: false`) 输出 Pydantic `Plan`,配合 `JsonOutputParser | RunnableLambda(Plan.model_validate)`。`PYTHON_AGENT_PLANNER_RETRY_MAX=1` 的有界重试兜底截断 / schema drift
- **executor** (`agent/executor.py`) — 顺序遍历 `Plan.steps`,四种 step kind:
  - `rag` → 调 Go `/internal/rag/retrieve`
  - `tool` → 调 Go MCP `tools/call` (JSON-RPC)
  - `llm` → 单次模型补全,作为一个 `delta` 流出去
  - `react` → 嵌套子图 (`model → tool → model`),上限 `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`
- SSE wire contract:11 个 kind(run_id 为 run-state 新增;其余 10 个与 Go 旧版字节级兼容,旧客户端零感知)

### 2. 持久化 / 状态

- **LangGraph Postgres checkpointer** (`langgraph-checkpoint-postgres>=3.0`),`thread_id = run_id` 维度的 checkpoint 恢复
- **`agent.runs` 元数据表** (`agent/run_metadata.py`,alembic 管理) — `user_id` / `status` / `total_tokens` / `error_code` / `last_heartbeat_at` / 时间戳,可索引查询
- **会话内 resume** — `POST /agent/run` 带 `run_id` 命中未终态行 → 从 checkpoint 续跑;心跳超时被 sweeper 回收后按终态行处理(新 run)
- **stale-run sweeper**(已落地 0a9b8f5,`agent/sweeper.py`)— 运行中每 30s touch `last_heartbeat_at`(失败仅 WARN),每 300s 扫描,心跳静默超 900s 的 `running` 行翻成 `failed`/`error_code='stale_run'`;该 timeout 同时是 crashed-run 的 resume 窗口。`PYTHON_AGENT_SWEEP_ENABLED=false` 是回滚开关
- **retention cron** (`agent/retention.py`) — 每小时清理终态行 30 天前数据(`DELETE ... RETURNING id`),再逐行 `adelete_thread` GC LangGraph checkpoint(单行 GC 失败仅 WARN,删行照旧)——全服务唯一的 checkpoint GC 路径;sweeper 有意保留被回收行的 checkpoint,由 retention 随行一并回收

### 3. 资源治理

- **每用户 hourly token budget** (`agent/budget.py`) — Redis 计数器 `agent:budget:u={user_id}:h={utc_hour}`,`INCRBY` 越界 `DECRBY` 回滚,**fail-closed**(Redis 故障 → `budget_unavailable` 终止)
- **每 IP 速率限制** (`agent/ratelimit.py`) — `/agent/run` 100 req/s + burst 200

### 4. 可观测

- **OTel spans** — `agent.run` / `agent.step.{plan,tool,llm,react}` / `agent.planner.structured|retry` / `agent.budget` / `agent.run.retention` / `agent.run.sweep` → OTLP/HTTP → Jaeger,跟 Go 后端同链路,SPA 通过 W3C `traceparent` 串联
- **Prometheus counters** (`agent/observability.py`) — `agent_runs_total` / `agent_steps_total` / `agent_tool_calls_total` / `agent_tokens_total` / `agent_planner_parse_failures_total{retry}` / `agent_tool_retries_total` / `agent_run_retention_deleted_total` / `agent_runs_swept_total`,`:9001` 暴露
- **`/healthz`** — 同时 ping Redis 和 LLM `GET {base}/models` 取最差结果

### 5. 安全 / 隔离

- **网络层 trust boundary** — Python service **不验 JWT**,Go 在 SPA 边界验完通过 `X-Nyx-User-Id` 转发;`Authorization` bearer 原样 passthrough 给 Go `/mcp` 与 `/internal/rag/retrieve` 二次验。端口 9000 不对外映射,只有 `:9001` (metrics) 漏给 host
- **safe-error mapping** — `api.ClassifyAndLog` 在 Go 侧统一抹掉内部错误细节

### 6. 测试基建

- `pytest-asyncio` (`asyncio_mode = "auto"`)
- `fakeredis` + `httpx.MockTransport` — 无需活网络
- `testcontainers[postgres]` 跑真库场景
- 跨 runtime SSE wire-contract 测试 — 解析前端实际 `chatService.ts`,不只测生成端

---

## 二、切片状态(截至 2026-10-14)

| 项 | 状态 | 落点 |
|---|---|---|
| **tool-args retry budget** (结构化工具调用 + 验证 + retry) | 已落地 | `executor._reprompt_for_corrected_call`(agent-tool-dispatch-retry,归档 2026-10-13) |
| **tool execution transient retry** (timeout / network 指数退避) | 已落地 | `executor._dispatch_tool_with_retries` 分类 arg/transient/non_retryable,退避 250ms→5s(同切片) |
| **partial-failure banner** (失败步骤可视化) | 已落地 | `frontend/src/features/chat/components/PartialFailureBanner.tsx`(同切片) |
| **stale-run sweeper** (心跳/lease 回收 `running` 僵尸行) | 已落地(0a9b8f5) | `agent/sweeper.py` + 迁移 `0003_add_run_heartbeat` |
| **adelete_thread on retention** (LangGraph checkpoint_* 真 GC) | 已落地(0a9b8f5) | `agent/retention.py`(同切片;旧注释已修正) |
| **`/agent/runs` 查询端点** + 对话历史持久化 | 未做 | `agent.run_metadata.list_runs_for_user` 已经写好,只缺 endpoint |
| **`chat_sessions` / `chat_messages` 表** + per-thread 历史注入 | 未做 | P2.1 路线图 |
| **Task routing** (意图 → 计划模板) | 未做 | P0.4 明确推迟到 plan 模板收敛后 |

---

## 三、结构性缺口 (vs 社区主流)

### A. LLM-level observability — OTel 不够用

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **Langfuse** / **LangSmith** / **Arize Phoenix** — prompt/response 落盘、conversation 回放、cost dashboard | **已落地(agent-langfuse-tracing)**:自托管 Langfuse(默认关闭,`PYTHON_AGENT_LANGFUSE_ENABLED=true` 开启;`make up-langfuse`,UI :3000)经 LangChain callback 把每 run 的 prompt / reply / provider usage 落成 generation,tool / step 另有 span。Jaeger 侧保持 metadata-only |

**观测分层**(排障时按层选面):元数据(span 时序 / 计数 / `error_code`)→ Jaeger + Prometheus,常开;agent 语义(plan / step 序列 / 工具 args / usage / 终态)→ SSE 帧契约,eval runner 逐 case 落盘成 artifact,是 eval 排障的主战场;模型层(raw prompt / completion 回放)→ Langfuse,dev 默认关,怀疑模型层时 `make up-langfuse` 按需开——agent 侧只是 `PYTHON_AGENT_LANGFUSE_HOST` + key pair,compose 栈仅服务 dev;生产若开则 URL 指向自托管 Langfuse 集群(属 rollout 决策)。Langfuse 不是 eval 的前置依赖,是显微镜不是照明。

### B. Provider 抽象与路由

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **LiteLLM** / **Portkey** — 多 provider 统一 client、cost routing、retry、circuit-break | 只有 `langchain-openai` 一个 client。多 provider fallback、Anthropic 原生 SDK、按成本/延迟路由都得自己造。Go 侧有 `internal/llm.Provider` seam,Python 侧没继承过来 |

### C. LLM response cache

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **GPTCache** / **Redis LangCache** / **Helicone semantic cache** | Redis 只承担 rate-limit + hourly budget。同 prompt 直出没缓存,高频 query 成本没砍 |

### D. 跨 session 长期记忆

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **Mem0** / **Zep** / **Letta** (MemGPT) / **LangChain CompositeBackend** | 只有 Postgres checkpointer (thread 维度)。跨 session 没法 recall 用户偏好 |

### E. Guardrails / 安全

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **NeMo Guardrails** / **Guardrails AI** / **Lakera** (prompt injection / PII 过滤) | 只有 Pydantic 校验 planner 输出。Python 内层对 prompt injection、内容审核、敏感词 0 防御,全靠 Go chat handler 那侧挡 |

### F. 现代 agent framework 选择

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| LangGraph (我们的选择) / **PydanticAI** (pydantic 官方,type-safe 与我们的栈撞得很齐) / **Agno** / **DSPy** (prompt 编译优化) | LangGraph 仍是 state-machine 最稳的,但 prompt 没有 registry / 版本化 / A/B harness,改一行 prompt 没法量化谁好 |

### G. Code-execution 沙箱 (当工具涉及 untrusted 代码时)

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **e2b** / **Daytona** / **Modal sandboxes** | 没有。需要执行 arbitrary code 的工具要么拒、要么经 MCP 自管 |

### H. Browser / Computer Use

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **Browser-Use** / **Skyvern** / **Stagehand** / Anthropic Computer Use | 经 MCP 抽象。`tools/list` 里加一个 MCP server 就有了 — 这是架构优势 |

### I. SDK 分发

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| 官方 Python / TypeScript SDK / LangChain integration | 没有。第三方接入 `/agent/run` 都得自己拼 SSE |

---

## 四、按 ROI 排的落地顺序(状态截至 2026-10-15)

**已落地(原列表第 1、4、5 项)**:
- **Langfuse 自托管** (A 类) — `agent-langfuse-tracing` 切片:callback 挂在 graph_config 一处接缝即得结构化 trace(step/tool 观测另加 wrap),planner 调优从盲调变成数据驱动;配套 `-rollout` 兄弟(PM 执行)
- `adelete_thread` 联动 retention + stale-run sweeper — 同属 `agent-stale-run-sweeper` 切片(已落地 0a9b8f5,配套 `-rollout` 兄弟;旧文档与代码不符的注释已一并修正)

**仍然排队的(按 ROI)**:
  1. **Golden Set/Eval 套件** (A 类延伸) — 没有 golden set 就没有回归线,改 `PYTHON_AGENT_PLANNER_RETRY_MAX` / prompt 都不知道真不真。选型已定:promptfoo 起步(custom provider 打 `POST /agent/run` 真 SSE,`promptfoo view` 跨 run diff,CI 可集成);轨迹级严格评估再升 Inspect AI(同一份 JSONL 数据集,可共存);Braintrust 排除(控制面 SaaS-only、数据出域,自托管仅 Enterprise 数据面,免费档 14 天留存撑不起回归线);DeepEval / Ragas / autoevals(OSS)只作 grader 指标库备选。与 Langfuse 解耦(见 A 类观测分层):eval 排障的主战场是 runner 逐 case 落盘的 SSE 全帧 artifact,不是 Langfuse。grader 以确定性断言为主(本地 4B judge 噪声大),v0 先 30-50 条覆盖六种 plan 形态(rag-only / tool-only / 多步 / react / 拒答 / budget-unavailable);红队/注入集另挂 `security` tag——重点是经 web_search / RAG 内容的间接注入(断言:不执行 delete_feed 等破坏性工具、不漏 system prompt),direct 注入留少量探针;可借 promptfoo redteam 生成,失败率与主回归线分开跟踪
2. **LiteLLM seam** (B 类) — 不接的话每个新 provider 都要在 `langchain-openai` 与 vLLM `hermes` parser 之间打补丁,vLLM tool-call 一致性问题会反复咬人
3. **GPTCache / Redis LangCache** (C 类) — 同 prompt 直出秒回,降本路径里 cost-per-token 直接砍
4. **Prompt registry + A/B harness** (F 类) — Langfuse 已落地,prompt 版本化 / 分流现在有落点(配 Langfuse 用)

> 更长的视野:跨 session 记忆(D)与 guardrails(E)是高门槛低频次,P2.1 已经在排期;code-execution 沙箱(G)按需开启。

---

## 五、参考

- `backend/agent_py/README.md` — Python service 入口
- `backend/agent_py/pyproject.toml` — 依赖锁定
- `FUTURE_AGENT.md` — P0/P1/P2 路线图与风险
- `AgenticStackNaive.zh.md` — agent 能力维度的原始需求拆解 (P0/P1/P2)
- `AgentArchitecture.md` — `agent_py` 现状架构笔记(英文,含 "What's not done" 章节)
- `AgentFaultTolerance.md` — 故障面索引(英文)
