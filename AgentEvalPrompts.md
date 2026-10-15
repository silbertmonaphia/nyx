# MCP RAG(list_feed) tool evel
1.总结最近3个月我发的feed
expect: planner:[rag, llm]
behavior: None

# MCP Feed(add_feed,update_feed,delete_feed) tool eval 
1.总结最近3个月我发的feed，形成一篇新feed发出去
expect: planner:[rag, llm, create_feed]
behavior: 添加feed

2.更新我近3个月的所有关于不好情绪的feed为积极情绪（比如开心、幸福）,我不想别人觉得我是个负面情绪的人
expect: planner:[rag, llm, update_feed]
behavior: 更新feed

3.删掉我近3个月的所有关于不好情绪的feed,我不想别人觉得我是个负面情绪的人
expect: planner:[rag, llm, delete_feed]
behavior: 删除feed

# Web Search Tool evel
1.今天天气如何？
expect: planner:[rag, web_search, llm]
Behavior: None
p.s. 所以 weather 用例必须声明"web 开/关"两个运行模式(PYTHON_AGENT_WEB_SEARCH_ENABLED=true/false)，期望也不同。

2.今天天气如何？上网搜索看看
expect: planner:[web_search, llm]
Behavior: None

# ALL tool eval
1.基于对最近3个月我发的feed的总结，然后上网搜索相关信息，再形成一篇新feed发出去
expect: planner:[rag, web_search, llm, create_feed]


# 注意
1、关于对LLM不确定的概率模型的测试要用Eval Golden Set
2、Eval 报告的是率，不是单次 pass/fail，和传统软件测试那样的确定性测试不同
3、这个文件建议正式化为 eval 套件（一个 live、tag 化的 harness，输入就是这个 prompt 列表 + 配置矩阵），跑 nightly/发布前。要AI把它落地成可执行的 eval spec

# Golden Set 补充知识

资源分配： 在中大型 Agent 项目中，团队通常会投入 20%~30% 的精力专门用于评估集的构建、维护和自动化评估管线（Evaluation Pipeline）的开发。

既要有 60% 的日常常规任务，也要有 30% 的复杂多任务，以及 10% 的恶意攻击/极端异常。

答案必须经过领域专家（SME）的审核，确保其 100% 正确。

每一个 Case 都要有明确的评估标准（如：必须包含某些关键词、输出格式必须是 JSON 等）。易于利用 LLM-as-a-judge 评测。可自动化 (Automated)

一个合格的 Golden Set，通常是一个结构化的三元组（或四元组），它必须包含以下核心要素：
• Input（输入）： 用户的原始请求（即你说的 Prompt/Query）。
• Context（上下文，可选）： Agent 运行此时此刻必须依赖的外部信息（如特定的知识库文档、当前的用户画像数据）。
• Expected Output / Ground Truth（标准答案）： 核心所在。这可以是专家写好的完美回复、期望得到的标准 JSON 结构，或者是必须正确调用的 API 名称。
• Evaluation Criteria（评估规则）： 明确告诉评测系统怎么算对（如：必须包含关键词 X、不能包含政治词汇 Y、事实准确度打分必须大于 4 分等）。

为了防止对着 Golden Set 调优导致系统变僵硬(Overfitting)，业界通常采用以下策略：
1. 分集治理（Train/Dev/Test Split）：
	• Dev Set（开发集）： 50 个 Case，天天对着它调 Prompt、改代码。
	• Golden Set（测试集）： 200 个 Case，锁在保险箱里。只有在每周发布大版本、或者模型整体升级时，才跑一次。开发人员平时不准看这个集合的具体内容。
2. 动态更新（Dynamic Golden Set）： 每周从线上真实用户的失败报错中，抽取 5% 的新 Case 替换掉旧的 Case，让黄金集“活”起来。
3. 模糊评测（LLM-as-a-judge）： 评估时不采用死板的字符串完全匹配，而是用另一个更高级的模型（如 GPT-4o 或 Claude 3.5 Sonnet）去理解语义。只要意思对、逻辑通，就给高分，从而鼓励 Agent 保持泛化能力。

在实际的工业界落地中，Golden Set 绝对不需要、也不可能完全靠人工手写。如果全靠人工纯手写，不仅成本高昂，而且效率极低，根本赶不上 Agent 快速迭代的速度。
现代 AI 团队普遍采用 “合成数据（Synthetic Data） + 人工校验（Human-in-the-loop）” 的混合模式。

## 核心技术选型与生成方案

构建 Golden Set，行业内目前主要有以下 4 种技术选型和工具流：

方案 A：用户真实数据抽样（最推荐的基石）

• 做法： 从线上日志（Logs）、历史客服对话记录、或者测试期间用户的输入中，抽取真实的 Query。然后通过大模型进行初步清洗，最后交由人工（业务专家）为这些真实 Query 编写或修正“标准答案（Ground Truth）”。
• 优点： 100% 反映真实业务场景和真实用户的说话习惯。
• 技术选型： LangSmith, Phoenix (Arize), Langfuse 等 LLM 观测平台。它们自带“一键加入数据集（Add to Dataset）”的功能。

方案 B：LLM 自动化合成（扩充多样性的利器）

• 做法： 给定一些种子数据（Seed Data）或你的业务文档（KB），让一个能力极强的大模型（如 GPT-4o 或 Claude 3.5 Sonnet）根据演进算法（Evol-Instruct）或者特定模板，自动批量生成各种难度的输入和理想的输出。
• 优点： 速度极快，成本低，可以短时间内生成成千上万条包含各种边界情况（Edge Cases）的数据。
• 技术选型：
	• Ragas / DeepEval： 专门用于 RAG（检索增强生成）和 Agent 的评估框架，内置了根据文档自动生成 Question-Context-Answer 三元组的功能。
	• TextGrad / Distilabel： 工业级的合成数据与评测数据集生成框架。

方案 C：基于模拟器（User Simulator）的对抗生成

• 做法： 让大模型扮演一个“刁钻的用户”或“黑客”，去疯狂攻击和调戏你的 Agent，在对战过程中自动沉淀出复杂的、包含多轮对话的 Golden Set。
• 优点： 特别适合测试 Agent 的鲁棒性（Robustness）和抗拒绝能力。

## 黄金集构建的“最佳工作流”

在实际操作中，团队通常遵循 70% 机器生成 + 30% 人工校验 的黄金法则：
 [业务文档/种子数据] 
       │
       ▼
 [LLM 自动合成/演进] (使用 Ragas 或 DeepEval) ──> 生成 1000 条候选 Case
       │
       ▼
 [数据清洗与去重] (通过嵌入向量过滤掉相似度过高的 Case)
       │
       ▼
 [人类专家校验 (P0)] ──> 剔除逻辑错误的，精选出 100-200 条核心 Golden Set

## 评估基础设施的工具链选型（Tooling Landscape）

当你的 Golden Set 准备好后，你需要一个框架把它跑起来并自动打分。目前市面上主流的开源与商业选型如下：
DeepEval,Ragas,Promptfoo