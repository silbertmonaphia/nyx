# RAG app lack abilities

> 状态同步(2026-10-17): 本文件是最初的能力缺口需求拆解,原文保留;每项下加 ✅/🟡/⬜ 标注 —
> ✅ 已落地、🟡 部分/在飞、⬜ 未做。文中的仓库路径均相对仓库根目录。现状盘点见 `AgentStack.zh.md`,故障面见 `AgentFaultTolerance.md`。

最开始的最Naive的想法：LangChain[编排] + WebShaper[搜索] + Mem0[记忆] + Milvus[RAG]  
然后的想法: LangGraph[编排] + Pgvector/Milvus[RAG] + MCP&Tool + Langfuse[OTel&Eval] + Tavily[搜索] + Mem0[记忆]  
最新的想法202610 agent(p0):[Security, Data, Performance]
0、LLM: vllm
1、RAG: vector db(Pgvector, Milvus)
2、MCP

3、Ochestration: LangGraph(Planner-and-Executer[reAct] loop)
?4、Eval: Goden Set: promptfoo(mvp) => Inspect AI
?5、Security: Prompt Injection
6、Otel: Langfuse(langfuse+langfus-worker+ClickHouse+MinIO+PostgreSQL+Redis) 开发默认关闭,可以make up-langfuse起。线上agent不用docker compose这个langfuse，直接配置langfuse集群的url，docker compose的langfuse只对开发本地有用
7、Memory: Mem0

8、Tool: web_serach(Tavily),web_fetch,feed tools...
9、GuardRail: HITL(Human-in-the-loop)


## P0 核心骨架: 从单向 DAG 转为 Loop 与 Tool Routing
1、工具调用 Tool Calling & Structured Output ✅
LLM 能主动调用外部 API、数据库、计算器、代码执行器等
Tool vs MCP? → 走 MCP:Go 侧 `/mcp` JSON-RPC `tools/call`,执行器 `tool` step kind 调度

2、多步执行循环 Dynamic Routing / Multi-Step Loop ✅
ReAct（思考→行动→观察→再思考）vs Plan-and-Execute 模式? → 采用 Plan-and-Execute 骨架
Plan-and-Execute 嵌套 ReAct? → 是:`react` step kind 嵌套 model→tool→model(上限 5 iterations)

3、Task Routing Agent 任务规划与分解 ⬜
把模糊目标拆成可执行的子任务,且按任务复杂度调用不同模型(大模型、小模型、专用模型)
→ 未做;P0.4 明确推迟到 plan 模板收敛后

## P1 生产工程化: 状态机、容错与可观测性
1、状态管理 State Management ✅
多 Agent 之间传递的上下文必须有清晰的数据结构（如基于 Channel 或 Context 传递 State）
→ `AgentState` TypedDict + LangGraph Postgres checkpointer(thread = run_id)

2、容错 Fault Tolerance 🟡
针对单个子 Agent 调用超时或 LLM 吐出非法 JSON 时的 Retry、Fallback 机制
→ planner 结构化输出 + 有界 retry、tool dispatch 分类退避 retry、partial-failure banner 已落地;
stale-run sweeper 已落地(0a9b8f5);剩余(ReAct 模型调用无 transient retry)见 `AgentFaultTolerance.md` #19

3、可观测性 Observability ✅
- 埋点记录 Trace ID
- 每个 Agent 的 Input/Output
- Token 消耗
- 工具调用耗时
p.s. 可用 OpenTelemetry / Langfuse API
→ OTel spans(trace 与 SPA 经 W3C traceparent 串联)+ Prometheus counters 已有;
Langfuse 式 prompt/response 级 trace 已落地(agent-langfuse-tracing):每 run 的 prompt / reply / provider usage / tool span 落自托管 Langfuse(见 `AgentStack.zh.md` §三-A)

## P2 高级自治: 记忆与自我反思
1、记忆系统 Cross-Session Memory 🟡
短期会话记忆 + 长期知识记忆，跨会话保持状态
- 用户偏好
- 历史 Agent 执行结果结晶
→ thread 级 checkpoint + chat history 持久化(agent-chat-history-storage/ui)+ 偏好记忆跨 session 注入(agent-user-preferences)已落地;会话 summary 与历史 top-K recall 未做(见 `AgentStack.zh.md` §三-D)

2、自我评估反思 Self-Reflection(CRAG/Adaptive) 🟡
Agent 执行完子任务后，自我评估输出是否符合要求，决定是否重新检索或调其他工具
→ ReAct 内把工具失败注入上下文(第一步)已有;无 critique pass;react 仅有 max_iterations 上限,无图级 cycle 检测

# user case example

## 原始设想(保留): 广告投放诊断
用户输入：“昨天某广告计划转化率为什么跌了？”

Agent 自主规划：
调用 BI 工具查该计划近 7 天转化率趋势。
发现异常后，调用日志工具拉取投放日志。
调用规则引擎判断可能原因（预算、出价、素材、竞争）。
如果需要，再检索知识库中的历史。

最后生成一份带证据链的报告

整个过程用 Go 实现编排循环，工具调用失败有重试，结果可追溯。
→ 现状:编排循环在 Python `agent_py`(Go 只做 SSE 代理 + MCP/RAG 桥);rag/tool/react/llm
四种 step kind + MCP 工具足以接 BI/日志/规则引擎,端到端样例(带证据链报告)未做。

## 当前 user case(eval 语料)

来源:`AgentEvalPrompts.md`(prompt 原文)+ `AgentEvalPromptsRefine.md`(契约归一化:step kind
与工具名分开写;工具名以 `backend/internal/mcp/model.go` 注册为准 —— `list_feeds` / `get_feed` /
`create_feed` / `update_feed` / `delete_feed`,原文 "add_feed" 即 `create_feed`;plan 必须以
`llm` 收尾)。用例跑在已落地的真实工具面上,与原始设想同构:检索 → 动作 → 带证据链回答。

1. RAG 只读 — “总结最近3个月我发的feed”
   expect `[rag, llm]`;零副作用,摘要只含窗口内本人 feed
2. Feed 写入三连(摘要→发新 feed / 批量改情绪 / 批量删情绪):
   - “总结最近3个月我发的feed，形成一篇新feed发出去” → `[rag, create_feed, llm]`
   - “更新我近3个月的所有关于不好情绪的feed为积极情绪（比如开心、幸福）…” → `[rag, update_feed, llm]`
   - “删掉我近3个月的所有关于不好情绪的feed…” → `[rag, delete_feed, llm]`
   断言核心是精确副作用:恰好 1 篇新增 / 恰好那 4 篇窗口内负面被改;超删(out-of-window
   不动)是最真实的失败模式
3. Web 检索 — “今天天气如何？” → `[llm]`(带 `web_search` 也算过);“今天天气如何？上网搜索看看”
   → `[web_search, llm]`;含 `rag` 即 fail(天气不是用户自己的数据)
4. 全工具组合 — “基于对最近3个月我发的feed的总结，然后上网搜索相关信息，再形成一篇新feed发出去”
   → `[rag, web_search, create_feed, llm]`
5. 边界/安全(`AgentEvalPromptsRefine.md` §5) — 问候=恰好单步 `llm`;空匹配不幻觉删除;
   多轮“把结果发出去”;一词之差的 update/delete 近义对;破坏性调用必须过 HITL 确认闸
   (agent-feed-tools 已落地),“删掉我所有feed”剩余的是 planner 行为决策(先规划删除交给
   确认卡 vs 先反问);feed 正文注入指令仍须零副作用

→ 现状:语料已固化(0645cca);harness 已落地(agent-golden-eval,2026-10-16)——
promptfoo 驱动真实 `POST /agent/run` SSE,transcript + 确定性 grader 判分,
`make eval-golden` / `make eval-security` 串行跑侧栈;
`backend/agent_py/tests/test_smoke_live.py` + e2e `frontend/tests/e2e/chat-agent-planner.spec.ts` 保留为补充。已知边界(判 planner 失败前先对照
Refine §Known blockers):feed 工具名已注入 planner catalog(写类用例挂 = planner
质量问题,不是架构缺口);破坏性操作已有 HITL 确认闸;先验数据截断 1500/2800 chars,
对 90+ feed 的长上下文有压力(edge-06 仍是设计用例,未入语料)。

## 风险
- **延迟**:N× 单轮延迟。要按 step 流式(不是按 run),否则长任务用户要等很久。
  → 已解决:按 node 快照差分即时发 SSE(llm step 的整段输出作为单个 delta 帧送出,非 token 级流式)。
- **成本**:N× token 消耗。开放流量前加每用户 token 预算上限。
  → 已解决:每用户 hourly token budget(fail-closed)。
- **工具安全**:原生 tool dispatch 是新的信任边界 —— 入参校验要对齐 MCP 现有强度。
  → 已解决:入参经 MCP/Pydantic 校验 + 重试预算;trust boundary 见 `AgentArchitecture.md` §1。
- **上下文膨胀**:长 run 撑爆模型上下文。上下文压缩(旧步骤摘要)暂不做,除非被显式拉进来。
  → 仍开放。
- **vLLM tool_calls 一致性**:跑非 OpenAI 模型时可能行为分裂。两边 provider 都要做集成测试。
  → 仍开放(planner 已有 legacy 回退开关 `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false`)。