# RAG app lack abilities

> 状态同步(2026-10-14): 本文件是最初的能力缺口需求拆解,原文保留;每项下加 ✅/🟡/⬜ 标注 —
> ✅ 已落地、🟡 部分/在飞、⬜ 未做。现状盘点见 `AgentStack.zh.md`,故障面见 `AgentFaultTolerance.md`。

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
stale-run sweeper 在飞;剩余(ReAct 模型调用无 transient retry、cycle detection)见 `AgentFaultTolerance.md` #19

3、可观测性 Observability 🟡
- 埋点记录 Trace ID
- 每个 Agent 的 Input/Output
- Token 消耗
- 工具调用耗时
p.s. 可用 OpenTelemetry / Langfuse API
→ OTel spans(trace 与 SPA 经 W3C traceparent 串联)+ Prometheus counters 已有;
Langfuse 式 prompt/response 级 trace 未做(见 `AgentStack.zh.md` §三-A)

## P2 高级自治: 记忆与自我反思
1、记忆系统 Cross-Session Memory ⬜
短期会话记忆 + 长期知识记忆，跨会话保持状态
- 用户偏好
- 历史 Agent 执行结果结晶
→ thread 级 checkpoint 已有;跨 session recall 未做(P2.1 排期)

2、自我评估反思 Self-Reflection(CRAG/Adaptive) 🟡
Agent 执行完子任务后，自我评估输出是否符合要求，决定是否重新检索或调其他工具
→ ReAct 内把工具失败注入上下文(第一步)已有;无 critique pass / 循环检测

# user case example
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