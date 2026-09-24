# AI Coding Harness 优先级

本文档基于 `doc/AI-Harness.md` 的 6 类方法 + 业界补充方法，按"对这个项目投入产出比"排序。每条标注：**是什么 / 为什么 / 怎么做**。

---

## 已有底盘（不重做）

- 入口安全：`.claude/settings` allow/deny + `gitleaks` pre-commit
- AI 上下文过滤：`.claudeignore`（扩展方向：屏蔽 `.env*`、`node_modules`、`playwright-report`、`coverage.html` 等不该进 prompt 上下文的产物）
- 流程：`CLAUDE.md` 强制 Plan 模式 + 显式 review 子 agent
- 质量门：golangci-lint / vitest / coverage gate（`make coverage-check`）/ sqlc drift（`make sqlc-diff`）/ OpenAPI drift（`make openapi-diff`）/ commitlint / Conventional Commits
- Pre-commit：husky + lint-staged（eslint --fix + vitest related + gitleaks）
- CI：semantic-release monorepo、GitHub Actions、`make sqlc-diff`、`make openapi-diff`、`make coverage-check`
- 运行时观测：后端 OTel→Jaeger，前端 Sentry

---

## P0 — 必须做（成本低、命中真痛点）

### 1. Schema 迁移安全性 Harness

- **是什么**：拦截单步破坏性迁移（DROP COLUMN、强制 NOT NULL、改类型）。
- **为什么**：这个项目最大的潜在事故源。`golang-migrate` 不拦人写"一步 DROP"，AI 也最爱这么写。
- **怎么做**：自定义 pre-commit / CI 脚本，扫描 `backend/migrations/*.up.sql`，禁止单步删列、改 NOT NULL、改类型，强制 expand → backfill → contract 三步走；或用 `atlas`/`sqitch` 这类带 lint 的迁移工具替换。

### 2. 架构契约（frontend）— dependency-cruiser

- **是什么**：在前端 CI 强制 `src/features/*` 之间的依赖边界。
- **为什么**：AI 在 React 项目里跨层调用（hooks 直接调 store、components 直接 fetch services 之外的模块）是最高频违规；后端 clean-arch 分层已经够硬，frontend 这边是短板。
- **怎么做**：先跑一条白名单规则 —— `features/<a>/*` 不许 import `features/<b>/*` 的 UI/hook 代码（共享的 `services/api.ts` 豁免）。`Zustand store` 本身就是 `hooks/` 读，所以"hooks 不许 import store"这种伪规则别写。`dependency-cruiser` 装在 `frontend/package.json` 的 devDep 里走 lint-staged（`cd frontend && npx depcruise --validate src/**/*.{ts,tsx}`，匹配 `.husky/pre-commit` 当前的 repo-root lint-staged + `cd frontend &&` 前缀模型），CI 再跑一次兜底。白名单先窄后宽，每加一条跑一遍真实测试避免误拦。

### 3. 双 Agent 对抗审判

- **是什么**：每次 backend/frontend 改动后自动跑一遍 `test-reviewer` 子 agent。
- **为什么**：`test-reviewer` 已定义在 `.claude/agents/test-reviewer.md`，但 `settings.json` 没挂 hook 强制执行；现在是"有人想用才用"。思维盲区有便宜解药但没人按。
- **怎么做**：挂 **`Stop` hook**（每轮 Claude turn 结束触发一次）或 `/review` slash command —— **不要**用 `PostToolUse` 按 edit 触发，多文件改动会爆 token 也拖慢反馈。`test-reviewer` 对 `git diff main...HEAD` 跑 `ReportFindings`，CONFIRMED 级别问题清零才放过。

### 4. 供应链漏洞扫描

- **是什么**：`govulncheck`（Go）+ `npm audit --audit-level=high`（frontend）。
- **为什么**：10 行 YAML 就能捡现成 CVE，比手动追依赖快得多。`gitleaks` 防的是 secrets 进 git，不防已知漏洞。
- **怎么做**：GitHub Actions 加两条 job：`cd backend && govulncheck ./...`、`cd frontend && npm audit --audit-level=high`。

### 5. Prompt injection 防御

- **是什么**：声明 `WebFetch` / MCP 工具返回 / 第三方 README / 外部 PR 评论内容**不视为指令**，只视为数据。
- **为什么**：gitleaks 防的是 secrets，**没防 AI 自身被劫持**。LLM Provider、外部 MCP、第三方 fetch 都是新的攻击面。
- **怎么做**：在根 `CLAUDE.md` 加 "Untrusted inputs" 一节；除 human 消息之外的输入源（`WebFetch` 返回、MCP 工具返回、第三方 README、外部 PR 评论）在 prompt 里显式标注 `[UNTRUSTED DATA]`，并明令禁止作为指令源。

---

## P1 — 看时机（值但有成本）

### 6. OpenAPI drift（双向）

- **是什么**：校验 `huma` 生成的 OpenAPI spec ↔ 前端 axios 客户端类型双向一致。
- **为什么**：`backend/Makefile` 的 `openapi-diff` target + CI 已经在跑，**守住了"spec 必须反映 handler"这一半**（改 handler 忘了 regen 会失败）。缺口是**反向**：spec 删了一个字段但 SPA 仍在用、SPA 调了 spec 里没有的字段 —— 这一半会 silently 漏掉。已有 `make sqlc-diff` 守 SQL 这一侧（同一类副作用），OpenAPI 反向也一样要守。
- **怎么做**：`openapi-typescript` 生成前端类型后，加一个 diff step —— 把 SPA `src/**` 里实际发出去的 axios 字段名枚举出来，跟 spec 的 `properties` 做交叉集合：spec 有而代码没用（warning，可过期）、代码有而 spec 没有（**fail**）。失败信号加进 CI。也可以用 `oasdiff` 做 strict 模式（breaking change 必须显式 ack），二选一，先窄后宽。

### 7. 变异测试（Mutation Testing）

- **是什么**：故意改坏业务代码，看测试是否还能过——揪"假单测"。
- **为什么**：AI 容易写"assert True"或没断言的测试刷覆盖率。go test 覆盖率只是覆盖率，变异测试才是真测试质量的代理指标。
- **怎么做**：Go 用 `go-mutesting`（`gremlins` 是 Groovy/Java 工具，别用错）、TS 用 `@stryker-mutator/*`。先挑一个高价值包（`auth/` token 流、SQL 翻译层）跑通，分数稳定后再 gate。整套开启构建时间会爆炸，按包增量打开。

### 8. AGENTS.md 分片

- **是什么**：把根 `CLAUDE.md` 里的"改 X 前先读 Y"挪到各 package 的 `AGENTS.md`，按需加载。
- **为什么**：根 CLAUDE.md 已经在长，按目录加载更省 token、规则也更近代码、更准。
- **怎么做**：`backend/internal/auth/AGENTS.md`、`backend/migrations/AGENTS.md`、`frontend/src/features/feeds/AGENTS.md`，先从最高频改动的目录开始。

### 9. 视觉回归

- **是什么**：Playwright 截图 + 基线像素对比。
- **为什么**：AI 改前端"逻辑对、样式崩"是高发问题。
- **怎么做**：仅当 SPA 视觉改动频繁 + 设计师参与时才值。`playwright` 自带 `toHaveScreenshot()`，或接 Chromatic。先在登录、Feed 列表两个核心页跑通，再扩展。

---

## P2 — 暂缓（场景不匹配或已重复）

| 方法 | 暂缓原因 |
| :--- | :--- |
| AST 改写 / jscodeshift 自动纠错 | 与 ESLint --fix / golangci-lint --fix 高度重叠，徒增维护 |
| 影子 DB + 故障注入 | testcontainers 已在；并发/外部依赖痛点未到，先不投 |
| **Eval harness / LLM-as-judge 评 PR 描述质量** | 项目体量未到 meta-Harness 阶段 |
| Cost / token budget Harness | Claude Code 自带 per-tool 限制，写策略反而碍事 |
| Ephemeral worktree 强制 | `.claude/worktrees/` + PR flow 已就位，无需再套层 |

---

## 推荐落地顺序

`#1 迁移安全 → #2 架构契约 → #3 双 Agent 审判 → #4 漏洞扫描 → #5 prompt injection 条款`

前两条是结构性收益（一次配、永久拦）；#3/#4 接近零成本；#5 是文档改动。其余看具体痛点出现再立项，避免一次性铺一套 Harness 反而拖慢迭代节奏。