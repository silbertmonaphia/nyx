# Nyx Agent 技术栈 — 已有 vs 行业对照

本文档对照 `backend/agent_py/` 的实际代码与社区主流 agent 开发生态,
盘点 Nyx 已具备的能力、下一切片要补的活、以及结构性缺口。
详细 roadmap 见 `FUTURE_AGENT.md`,架构学习笔记见 `AgentArchitecture.md`,
故障面索引见 `AgentFaultTolerance.md`。

---

## 一、已经有的

### 1. 编排层 (LangGraph Plan-and-Execute)

- **StateGraph** (`agent/graph.py:97`) — `START → planner → executor → END`
- **planner** (`agent/planner.py`) — 单次 LLM 调用,通过 OpenAI `response_format.json_schema` 显式 bind (`strict: false`) 输出 Pydantic `Plan`,配合 `JsonOutputParser | RunnableLambda(Plan.model_validate)`。`PYTHON_AGENT_PLANNER_RETRY_MAX=1` 的有界重试兜底截断 / schema drift;prompt 注入用户偏好与 live MCP 工具目录(registry 不可用时退化为无工具 prompt)
- **executor** (`agent/executor.py`) — 顺序遍历 `Plan.steps`,四种 step kind:
  - `rag` → 调 Go `/internal/rag/retrieve`
  - `tool` → `ToolDispatcher`(先本地 `web_search`/`web_fetch`,否则 Go MCP `tools/call` JSON-RPC)
  - `llm` → 单次模型补全,作为一个 `delta` 流出去
  - `react` → 嵌套子图 (`model → tool → model`),上限 `step.max_iterations` 或 `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`
- SSE wire contract:12 个 kind(`run_id` 为 run-state 新增、`confirm_request` 为 agent-feed-tools 新增;其余 10 个与 Go 旧版字节级兼容,旧客户端零感知)

### 2. 持久化 / 状态

- **LangGraph Postgres checkpointer** (`langgraph-checkpoint-postgres>=3.0`),`thread_id = run_id` 维度的 checkpoint 恢复
- **`agent.runs` 元数据表** (`agent/run_metadata.py`,alembic 管理) — `user_id` / `status` / `total_tokens` / `error_code` / `last_heartbeat_at` / 时间戳,可索引查询
- **会话内 resume** — `POST /agent/run` 带 `run_id` 分三种:命中 live `agent.runs` 行(非终态)→ 从 checkpoint 续跑;否则若该 id 命中 live `chat_sessions` 行 → 同 session 续轮(新 graph、`persist_run_state=False`、不新建 run 行);否则新 run。心跳超时被 sweeper 回收后按终态行处理(新 run)
- **stale-run sweeper**(已落地 0a9b8f5,`agent/sweeper.py`)— 运行中每 30s touch `last_heartbeat_at`(失败仅 WARN),每 300s 扫描,心跳静默超 900s 的 `running` 行翻成 `failed`/`error_code='stale_run'`;该 timeout 同时是 crashed-run 的 resume 窗口。`PYTHON_AGENT_SWEEP_ENABLED=false` 是回滚开关
- **retention cron** (`agent/retention.py`) — 每小时清理终态行 30 天前数据(`DELETE ... RETURNING id`),再逐行 `adelete_thread` GC LangGraph checkpoint(单行 GC 失败仅 WARN,删行照旧);chat retention 按 session 回收、GDPR erasure 也调用 `adelete_thread`,checkpoint GC 不再只有 run-retention 一条路径。sweeper 有意保留被回收行的 checkpoint,由 retention 随行一并回收

### 3. 资源治理

- **每用户 hourly token budget** (`agent/budget.py`) — Redis 计数器 `agent:budget:u={user_id}:h={utc_hour}`,`INCRBY` 越界 `DECRBY` 回滚,**fail-closed**(Redis 故障 → `budget_unavailable` 终止)
- **每 IP 速率限制** (`agent/ratelimit.py`) — `/agent/run` 100 req/s + burst 200

### 4. 可观测

- **OTel spans** — `agent.run` / `agent.step.{plan,tool,llm,react}` / `agent.planner.structured|retry|legacy` / `agent.budget` / `agent.run.retention` / `agent.run.sweep` / `agent.chat.retention` / `agent.audit.retention` / `agent.sessions.*` / `agent.approvals.decide` / `agent.gdpr.export|erase` → OTLP/HTTP → Jaeger,跟 Go 后端同链路,SPA 通过 W3C `traceparent` 串联
- **Prometheus counters** (`agent/observability.py`) — `agent_runs_total` / `agent_steps_total` / `agent_tool_calls_total` / `agent_tokens_total` / `agent_planner_parse_failures_total{retry}` / `agent_tool_retries_total` / `agent_run_retention_deleted_total` / `agent_runs_swept_total` / `agent_chat_retention_deleted_total` / `agent_chat_prefs_deleted_total` / `agent_audit_retention_deleted_total`,`:9001` 暴露
- **`/healthz`** — 同时 ping Redis 和 LLM `GET {base}/models` 取最差结果

### 5. 安全 / 隔离

- **网络层 trust boundary** — Python service **不验 JWT**,Go 在 SPA 边界验完通过 `X-Nyx-User-Id` 转发;`Authorization` bearer 原样 passthrough 给 Go `/mcp` 与 `/internal/rag/retrieve` 二次验。端口 9000 不对外映射,只有 `:9001` (metrics) 漏给 host
- **safe-error mapping** — `api.ClassifyAndLog` 在 Go 侧统一抹掉内部错误细节

### 6. 测试基建

- `pytest-asyncio` (`asyncio_mode = "auto"`)
- `fakeredis` + `httpx.MockTransport` — 无需活网络
- `testcontainers[postgres]` 跑真库场景
- 跨 runtime SSE wire-contract 测试 — 解析前端实际 `frontend/src/features/chat/services/chatService.ts`,不只测生成端

---

## 二、切片状态(截至 2026-10-17)

| 项 | 状态 | 落点 |
|---|---|---|
| **tool-args retry budget** (结构化工具调用 + 验证 + retry) | 已落地 | `executor._reprompt_for_corrected_call`(agent-tool-dispatch-retry,归档 2026-10-13) |
| **tool execution transient retry** (timeout / network 指数退避) | 已落地 | `executor._dispatch_tool_with_retries` 分类 arg/transient/non_retryable,退避 250ms→5s(同切片) |
| **partial-failure banner** (失败步骤可视化) | 已落地 | `frontend/src/features/chat/components/PartialFailureBanner.tsx`(同切片) |
| **stale-run sweeper** (心跳/lease 回收 `running` 僵尸行) | 已落地(0a9b8f5) | `agent/sweeper.py` + 迁移 `0003_add_run_heartbeat` |
| **adelete_thread on retention** (LangGraph checkpoint_* 真 GC) | 已落地(0a9b8f5) | `agent/retention.py`(同切片;旧注释已修正) |
| **`chat_sessions` / `chat_messages` 表** + per-thread 历史注入 | 已落地 | Slice 1/2(`agent-chat-history-storage` + `agent-chat-history-ui`,归档 2026-10-14);SPA 经 Go `/api/chat/sessions*` 代理到 Python `/agent/sessions*`,侧边栏 flag 门控;剩会话 summary 与跨 session recall |
| **`/agent/runs` 查询端点** | 未做 | `agent.run_metadata.list_runs_for_user` 已经写好,只缺 endpoint(sessions 端点已由 Slice 2 落地) |
| **Task routing** (意图 → 计划模板) | 未做 | P0.4 明确推迟到 plan 模板收敛后 |

---

## 三、结构性缺口 (vs 社区主流)

### A. LLM-level observability — OTel 不够用

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **Langfuse** / **LangSmith** / **Arize Phoenix** — prompt/response 落盘、conversation 回放、cost dashboard | **已落地(agent-langfuse-tracing)**:自托管 Langfuse(默认关闭,`PYTHON_AGENT_LANGFUSE_ENABLED=true` 开启;`make dev-up-langfuse`,UI :3000)经 LangChain callback 把每 run 的 prompt / reply / provider usage 落成 generation,tool / step 另有 span。Jaeger 侧保持 metadata-only |

**观测分层**(排障时按层选面):元数据(span 时序 / 计数 / `error_code`)→ Jaeger + Prometheus,常开;agent 语义(plan / step 序列 / 工具 args / usage / 终态)→ SSE 帧契约,eval runner 逐 case 落盘成 artifact,是 eval 排障的主战场;模型层(raw prompt / completion 回放)→ Langfuse,dev 默认关,怀疑模型层时 `make dev-up-langfuse` 按需开——agent 侧只是 `PYTHON_AGENT_LANGFUSE_HOST` + key pair,compose 栈仅服务 dev;生产若开则 URL 指向自托管 Langfuse 集群(属 rollout 决策)。Langfuse 不是 eval 的前置依赖,是显微镜不是照明。

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
| **Mem0** / **Zep** / **Letta** (MemGPT) / **LangChain CompositeBackend** | checkpointer + chat history 持久化(AgentMemoryRoadmap Slice 1/2)+ 偏好记忆(Slice 3,`PYTHON_AGENT_PREFS_ENABLED`)+ GDPR 封口(Slice 4)已落地归档;还差会话 summary 与跨 session top-K recall |

### E. Guardrails / 安全

| 社区方案 | 我们有什么 / 没有 |
|---|---|
| **NeMo Guardrails** / **Guardrails AI** / **Lakera** (prompt injection / PII 过滤) | Pydantic 校验 planner 输出;golden-eval security 注入线(`evals/cases/security.yaml`,9 条用例:RAG / 工具结果注入、prompt 泄漏、跨用户删除)把注入面变成可回归测量;程序化防线已有 mutating-tool HITL 确认门(agent-feed-tools)+ Go 侧 per-user MCP tool 授权。仍未做注入过滤 / PII / 审核层,残余拦截靠 system prompt 纪律 |

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

## 四、按 ROI 排的落地顺序(状态截至 2026-10-17)

**已落地(原列表第 1、4、5 项 + Golden Set 套件)**:
- **Langfuse 自托管** (A 类) — `agent-langfuse-tracing` 切片:callback 挂在 graph_config 一处接缝即得结构化 trace(step/tool 观测另加 wrap),planner 调优从盲调变成数据驱动;配套 `-rollout` 兄弟(PM 执行)
- `adelete_thread` 联动 retention + stale-run sweeper — 同属 `agent-stale-run-sweeper` 切片(已落地 0a9b8f5,配套 `-rollout` 兄弟;旧文档与代码不符的注释已一并修正)
- **Golden Set/Eval 套件** (A 类延伸) — `agent-golden-eval` 切片(实现落地):`evals/` promptfoo 套件,`make eval-golden` / `make eval-security`(串行、不启服务);custom provider 打 `POST /agent/run` 真 SSE,确定性 grader,主回归线与 `security` 注入线分开出率,基线报告入库。v0 = 21 条用例(五形态:rag-only / tool-only / 多步 / 拒答 / budget-unavailable);react 形态与扩容到 30-50 条留 v0.1。Inspect AI 升级路径不变(同一份数据集可共存);红队生成仍可用 `promptfoo redteam`

**仍然排队的(按 ROI,2026-10-17 重排)**:
  1. **Eval 强制执行点** — golden / security 两条线目前是纯手动 make 目标,`.github/workflows/` 零引用,prompt 安全网靠"有人记得跑"。拆两半落地:CI 加 `make eval-unit` 跑 `evals/tests/` 的 11 个 harness 单测(GPU-free,防 grader / corpus schema / wire-contract 腐烂,现状零接线);真模型的 `eval-security` 落在改动时刻——pre-push hook 或 drift-check 式 skill,检测 agent-py / prompt / eval cases 变更且 side stack 活着时跑(9 条,几分钟;栈没开降级为警告不硬卡)。GitHub-hosted runner 无 GPU 起不了 vLLM,self-hosted nightly 依赖 box 常开,均不设
  2. **每用户并发 run 上限** — 治理缺口:per-IP rate limit 管吞吐、hourly budget 管用量,没有东西限制同时 running 的 run 数;单卡 4B vLLM 下一个用户多开即可排队占满 GPU,其他用户全饿。修法照抄 budget 计数器模式(Redis `agent:running:u={id}` INCR/DECR + 上限返 429),约一天的活
  3. **Prompt registry + A/B harness** (F 类) — prompt 是 4B 小模型场景的主要质量杠杆,改动高频且现在是盲改(`PLANNER_SYSTEM_PROMPT` 硬编码在 `planner.py`,零版本化);前置已齐(Langfuse 落盘 + Golden Set 量化),剩余工作量小:prompt 外置 + 变体选择器 + eval 套件对比。同时解开 Task routing(P0.4)的锁——它推迟到 plan 模板收敛,而收敛靠可测量才快
  4. **Guardrails 子集:Langfuse 数据卫生** (E 类) — 注入面已有三层纵深:mutating 工具确认门(`executor.py`,agent-feed-tools D5/D6)+ Go 侧 per-user MCP 授权 + security eval 回归线,残余风险有界(泄漏 system prompt 给用户本人、操纵用户本要点头的动作);NeMo 式全量过滤层不划算(加延迟 / 误杀 / 依赖)。值得做的子集:prompt / response 全文进自托管 Langfuse 前的 PII 卫生,可并入第 3 项
  5. **LiteLLM seam** (B 类) — agent 路径目前没有 provider failover(Go `llm.Router` 只盖旧 Go 路径,Python agent 直连 vLLM 单点,vLLM 崩则 agent 全瘫),LiteLLM 的 router/fallback 给 agent 补上 Go 已有的韧性;仍是接任何第二家 provider 前的排序约束。降位理由:vLLM tool-call 咬人的痛已被 tool-args retry budget 止了一半(malformed call 有界 re-prompt),且无接第二家 provider 的计划时它纯是保险

**降出队列**:
- **GPTCache / Redis LangCache** (C 类) — 命中率假设未验证:最终 prompt 拼 RAG 上下文 + 历史,exact-match 几乎不命中;feeds per-user 使缓存 key 必须带 `user_id`,跨用户复用归零;语义缓存 false-hit 对 tool 流危险。前置动作是拉 Langfuse 数据统计相似 query 占比,数据不支持就除名
- **跨 session summary + recall** (D 类剩余) — roadmap 原话 not needed for v1;prefs 记忆(Slice 3)已给 planner 跨 session 连续性;四个 `*-rollout` 兄弟仍活跃,是 PM 的活,不占开发队列
- **`/agent/runs` 查询端点** — 不占队列位:`list_runs_for_user` 已写好,哪天 UI 要 run 历史,顺手补 endpoint(约一小时)

> code-execution 沙箱(G)不变:按需开启,现有工具面(feed CRUD / RAG / web)没有执行任意代码的场景。

---

## 五、参考

- `backend/agent_py/README.md` — Python service 入口
- `backend/agent_py/pyproject.toml` — 依赖锁定
- `FUTURE_AGENT.md` — P0/P1/P2 路线图与风险
- `AgenticStackNaive.zh.md` — agent 能力维度的原始需求拆解 (P0/P1/P2)
- `AgentArchitecture.md` — `agent_py` 现状架构笔记(英文,含 "What's not done" 章节)
- `AgentFaultTolerance.md` — 故障面索引(英文)
