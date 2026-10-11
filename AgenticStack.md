# RAG app lack abilities

## P0 核心骨架: 从单向 DAG 转为 Loop 与 Tool Routing
1、工具调用 Tool Calling & Structured Output
LLM 能主动调用外部 API、数据库、计算器、代码执行器等
Tool vs MCP?

2、多步执行循环 Dynamic Routing / Multi-Step Loop
ReAct（思考→行动→观察→再思考）vs Plan-and-Execute 模式?
Plan-and-Execute 嵌套 ReAct?

3、Task Routing Agent 任务规划与分解
把模糊目标拆成可执行的子任务,且按任务复杂度调用不同模型(大模型、小模型、专用模型)

## P1 生产工程化: 状态机、容错与可观测性
1、状态管理 State Management
多 Agent 之间传递的上下文必须有清晰的数据结构（如基于 Channel 或 Context 传递 State）

2、容错 Fault Tolerance
针对单个子 Agent 调用超时或 LLM 吐出非法 JSON 时的 Retry、Fallback 机制

3、可观测性 Observability
- 埋点记录 Trace ID
- 每个 Agent 的 Input/Output
- Token 消耗
- 工具调用耗时
p.s. 可用 OpenTelemetry / Langfuse API

## P2 高级自治: 记忆与自我反思
1、记忆系统 Cross-Session Memory 
短期会话记忆 + 长期知识记忆，跨会话保持状态
- 用户偏好
- 历史 Agent 执行结果结晶

2、自我评估反思 Self-Reflection(CRAG/Adaptive)
Agent 执行完子任务后，自我评估输出是否符合要求，决定是否重新检索或调其他工具

# user case example
用户输入：“昨天某广告计划转化率为什么跌了？”

Agent 自主规划：
调用 BI 工具查该计划近 7 天转化率趋势。
发现异常后，调用日志工具拉取投放日志。
调用规则引擎判断可能原因（预算、出价、素材、竞争）。
如果需要，再检索知识库中的历史。

最后生成一份带证据链的报告

整个过程用 Go 实现编排循环，工具调用失败有重试，结果可追溯。

## 风险
- **延迟**:N× 单轮延迟。要按 step 流式(不是按 run),否则长任务用户要等很久。
- **成本**:N× token 消耗。开放流量前加每用户 token 预算上限。
- **工具安全**:原生 tool dispatch 是新的信任边界 —— 入参校验要对齐 MCP 现有强度。
- **上下文膨胀**:长 run 撑爆模型上下文。上下文压缩(旧步骤摘要)暂不做,除非被显式拉进来。
- **vLLM tool_calls 一致性**:跑非 OpenAI 模型时可能行为分裂。两边 provider 都要做集成测试。