# 安全

English version: [SECURITY.md](SECURITY.md)

## 报告漏洞

请通过本仓库的 GitHub Security Advisories（私密披露）上报。72 小时内回复确认。请勿为安全缺陷提公开 issue。

## 审计

### 2026-08-26 — 全量安全审查

**范围。** 提交 `c9674d4` 的整个仓库：后端（Go）、前端（React）、Dockerfile、compose 文件、CI、依赖清单、`.env*` / `.gitignore` / `.dockerignore` / `.husky`。

**方法。** 5 个并行的只读扫描，覆盖 auth/JWT、输入/错误/SQL、CORS/响应头/密钥、前端/XSS/token、依赖/Docker。

**结论。** 无 critical 级发现。12 个 high、12 个 medium、14 个 low。Cookie auth 落地版（`c9674d4`）结构稳固；主要弱点是 CORS 默认值、登录侧信道时序、JWT claim 强化、缺失的安全响应头、以及开发基础设施端口开放。

## 发现

状态图例：☐ 未解决 · ☑ 已修复 · ◌ 不修（附原因）。

### High（高）

| # | 状态 | 区域 | 文件 | 问题 | 修复 |
|---|---|---|---|---|---|
| H1 | ☑ (`2fd84e5`) | CORS | `backend/internal/middleware/cors.go:33-69`、`backend/internal/platform/config/config.go:103` | 默认 `CORSAllowedOrigins=*`；结合 `Authorization` 响应头回显，第三方源可发起携带凭据的跨站请求 | 默认置空；prod 强制要求显式白名单 | 本批已修复 —— 默认现为 `""`；`.env.example` 列出 dev/staging/prod 示例；新增测试覆盖空白名单拒绝路径 |
| H2 | ☑ | Auth | `backend/internal/user/service.go:152-166` | 登录侧信道时序 —— bcrypt 仅对存在的用户名执行 | 在 `ErrUserNotFound` 分支针对已知错误哈希执行一次伪 `bcrypt.CompareHashAndPassword` | 已修复：`init()` 时以 DefaultCost 预算伪哈希；`Login` 在未找到分支调用 `CompareHashAndPassword(dummyBcryptHash, password)`；新增 `TestDummyBcryptHashReady` 锁定不变式 |
| H3 | ☑ | JWT | `backend/internal/platform/auth/jwt.go:94-97` | Keyfunc 直接返回密钥但不检查 `token.Method`；未使用 `jwt.WithValidMethods` | 添加 `WithValidMethods([]string{"HS256"})` 并在 keyfunc 中断言 `*jwt.SigningMethodHMAC` | 已修复：keyfunc 中加入 `WithValidMethods` 与冗余的 `*jwt.SigningMethodHMAC` 断言；`TestJWT_RejectsUnexpectedAlg` 锁定 alg=none 拒绝行为 |
| H4 | ☑ | JWT | `backend/internal/platform/auth/jwt.go:79-119` | 未签发也未校验 `iss` / `aud` / `nbf` claim | 使用 `WithIssuer` / `WithAudience` 签发并校验 | 已修复：硬编码 `Issuer=nyx-auth` / `Audience=nyx-api` 常量；`GenerateToken` 写入 `iss/aud/sub/nbf/iat`；`ValidateToken` 使用 `WithIssuer` + `WithAudience` + `WithExpirationRequired`；新增测试覆盖外部 issuer / audience 拒绝路径 |
| H5 | ☑ | Auth | `backend/internal/user/repository.go:36-38` + `backend/internal/platform/api/map.go:46-55` | Register 对 `ErrUsernameTaken` 与 `ErrEmailTaken` 返回不同消息（与 login 折叠的 `ErrInvalidCredentials` 不对称） | 折叠为统一的通用 `ErrUserAlreadyExists` | 已修复：新增 `ErrUserAlreadyExists` 哨兵；`Register` 在到达 handler 前折叠细粒度哨兵；`TestRegister_CollapsesUniqueViolationsToUserAlreadyExists` + `TestRegisterHandler_UsernameTakenReturns409` 改为断言 `"User already exists"`。Email 不再是唯一列，邮箱冲突分支已删除（无枚举信号需折叠） |
| H6 | ☑ | Frontend / Infra | `frontend/nginx.conf` | 未配置 CSP / X-Frame-Options / Referrer-Policy / Permissions-Policy / X-Content-Type-Options / HSTS | 添加上述响应头；CSP 采用 `default-src 'self'` 严格策略 | 已修复：服务器级 `add_header` 指令：CSP（`default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'` —— 行内样式是 `index.html` 中静态的 FOUC 防护块；style-src 上的 `'unsafe-inline'` 风险远小于 script-src 上的同等配置，且改用 hash 易脆）、`X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: strict-origin-when-cross-origin`、`Permissions-Policy` 禁用 camera/mic/geo/payment/usb 等、HSTS（1y、includeSubDomains）。带 `always` 使响应头同时作用于 SPA 与代理响应 |
| H7 | ☑ | Frontend | `frontend/src/services/api.ts:228-256`、`frontend/src/components/app/ToastContainer.tsx` | 服务端 `ErrorResponse.error` 被原样渲染为 toast | 限制长度、剥离控制字符、显示通用消息 + request id；完整 payload 入日志 | 已修复：5xx 响应不再回显后端 `error` 字段 —— toast 显示 `"Server error (<status>) (ref: <x-request-id>)"`（通用文案加 request id 便于用户引用）。4xx 例外：API 契约失败且消息可控、面向用户的字符串（`"User already exists"`、`"Feed not found"`、huma 校验消息……）**会**回显，以便用户可对失败做处置（如换用户名），而不是怀疑后端宕机。完整 payload（剥离控制字符、限长 2 KB）通过 `logger.error('api.error', {...})` 上报给运维。新增 `buildServerErrorToast` + `sanitiseForLog` 辅助函数；测试覆盖 4xx-回显 / 5xx-通用 / 缺失-error-字段 三条路径 |
| H8 | ☑ | Frontend | `frontend/src/services/api.ts:104-117` | 仅当 401 携带 `WWW-Authenticate: error_description="expired"` 时才触发 refresh；非过期的 401 强制登出 | 对任意 401 都尝试 refresh（除 `_retried` / `skipAuthRefresh` 外） | 已修复：refresh 触发条件仅由 `!skipRefresh`（即 `_retried` 或 `skipAuthRefresh`）控制。原 `WWW-Authenticate=expired` 子串匹配会在畸形/伪造 token 时强制登出，令可恢复会话的用户被锁出。single-flight + replay 逻辑未变。`api.test.ts` 测试已更新，分别用裸 401（无 challenge）和非过期 `invalid_token` challenge 驱动该路径 |
| H9 | ☑ | Docker | `backend/Dockerfile:21` | `FROM alpine:latest` —— 浮动 tag | 固定为 `alpine:3.20@sha256:<digest>` |
| H10 | ☑ | Docker | `frontend/Dockerfile:8` | 用 `npm install` 而非 `npm ci` | 改用 `npm ci --no-audit --no-fund` |
| H11 | ☑ | Docker | `backend/Dockerfile:18` | `go build` 缺失 `-trimpath -ldflags="-s -w -buildid="` | 添加上述 flag |
| H12 | ☑ | Docker | `docker-compose.yml:13-14, 27` | DB `5433` 与 Redis `6379` 绑定 `0.0.0.0`，默认账号 `postgres/postgres` | 改为绑定 `127.0.0.1` |

### Medium（中）

| # | 状态 | 区域 | 文件 | 问题 | 修复 |
|---|---|---|---|---|---|
| M1 | ☑ | Auth | `backend/internal/user/service.go:265-292` | Logout 撤销整个 refresh-token 族（杀掉所有设备） | 每行记录 `session_id`；仅撤销所请求的 session | 修正分析：原风险被高估 —— 每次 login/registration 自行为新族盖章（`family_id` = 首行 id），因此一个族对应一个设备的登录链，并非所有会话。`Logout` 现调用 `RevokeRefreshTokenFamily(row.FamilyID)`，可杀掉轮换出的后代（含攻击者已轮换过的被盗 token），而其他设备的族保持登录。新增 `POST /api/logout-all`（`Service.LogoutAll` + `RevokeRefreshTokensByUser`），用于 token 失陷场景。测试：`TestLogout_RevokesFamily`、`TestLogoutAll_RevokesEveryFamilyForUser`、`TestLogoutAll_RepoErrorPropagates`、`TestLogoutHandler_NoContent`、`TestLogoutHandler_ReadsTokenFromBody`、`TestLogoutAllHandler_*`，以及真实 Postgres 上的 `TestRefreshTokensIntegration` 子测试 `LogoutFamilyIsolation` + `RevokeAllForUser`（后者同时断言其他用户 token 不受影响） |
| M2 | ☑ | Auth | `backend/migrations/000008_create_refresh_tokens.up.sql` + `user/service.go` | 无后台清理过期/已撤销 refresh 行；无每用户上限 | 后台 goroutine 修剪；为每个用户的活跃族设上限 | 已修复：每用户上限 = 10，由 `mintRefreshToken` 通过 `enforceRefreshCap` 强制（插入前撤销多余的旧条目）。`internal/user/cleanup.go` 中新增后台清理 goroutine，通过新的 `PurgeRefreshTokensOlderThan` 查询删除 7 天前已撤销/过期的行。新增 SQL 查询：`CountActiveRefreshTokensByUser`、`ListOldestActiveRefreshTokensByUser`、`PurgeRefreshTokensOlderThan`（已通过 sqlc 重生成）。goroutine 在 main.go 的优雅关闭中挂接生命周期 |
| M3 | ☑ | Auth | `backend/internal/middleware/ratelimit.go:78-108` | 全局每 IP 限流（10 rps / 突发 20）对 login 同等适用 | `/api/login`、`/api/register` 单独更严的限流；按用户名锁定 | 部分修复：通过新的 `AuthRateLimit` huma.Middleware 在 `/api/login`、`/api/register` 操作粒度施加更严的限流（5 rps / 突发 10）。按用户名锁定**延期**（◌ 不修）—— 计划中的多副本部署安全落地需要 Redis 后端状态；每 IP 限流是 MVP 防线。已记录为跟进项 |
| M4 | ☑ | JWT | `backend/internal/platform/auth/jwt.go:63-65` | 启动失败的错误字符串包含 `len=%d`（密钥长度泄露） | 去掉格式化说明符 | 已修复：错误字符串现为静态 `"auth: refusing insecure JWT secret"`；`TestNewTokenService_ErrorHidesSecretLength` 锁定不变式 |
| M5 | ☑ | Auth | `backend/internal/platform/config/config.go:78` | 默认占位符 41 字节；会通过任何未来降到 32 的校验 | 拒绝该字面量；强制运维显式提供 | 此前已修复：`config.Load` 第 209-211 行调用 `auth.IsDefault(cfg.JWTSecret)` 并以明确错误拒绝；`auth.NewTokenService` 独立重复该检查。此处记录以保持完整 |
| M6 | ☑ | Input | `backend/internal/feed/huma_handler.go:134`，`repository.go:101-104` | `Q` 查询无长度上限；多 MB 的查询词直接打到 Postgres | 添加 `maxLength:"200"` | 已修复：`getFeedsInput.Q` 上添加 `maxLength:"200"` tag；huma 在 handler 执行前对超长输入返回 400 |
| M7 | ☑ | Input | `backend/internal/user/model.go:25-28` | `LoginRequest` 密码字段没有 `minLength` / `maxLength` | 在 bcrypt 之前服务端限长（如 128 字节） | 已修复：`RegisterRequest.Password` 现 `maxLength:"128"`（min 6 保留）；`LoginRequest.Password` 现 `maxLength:"128"`。上限同时覆盖 72 字节 bcrypt 截断及未来切换到 Argon2id 的情形 |
| M8 | ☑ | Frontend | `frontend/src/store/authStore.ts` | `loginAxios.post('/logout')` 没有 `baseURL`；开发环境命中 `localhost:5173/logout`（404） | 设置 `baseURL` 以匹配 `VITE_API_URL` | 已修复：`loginAxios` 现以 `baseURL: VITE_API_URL \|\| 'http://localhost:8080/api'` 创建，使 `/logout` 在 dev 与 prod 均解析到后端。Bearer 切换后，请求体携带 `{refresh_token: <rt>}`，`withCredentials` 为 `false`。新增测试同时锁定默认值与请求体 |
| M9 | ☑ | Docker | `k8s/secrets.yaml` + `k8s/backend-deployment.yaml` + `k8s/postgres-statefulset.yaml` + `backend/docker-entrypoint.sh` | prod 环境使用 `sslmode=disable`；`DB_URL` 由 `POSTGRES_PASSWORD` 经 shell 展开插值（`docker inspect` 中可见）；缺少从挂载密钥文件到后端环境变量驱动的桥接 | Kubernetes 中，`POSTGRES_PASSWORD` 由 `k8s/postgres-statefulset.yaml` 中 `secretKeyRef` 提供；`DB_URL` + `JWT_SECRET` 由 `k8s/backend-deployment.yaml` 中 `secretKeyRef` 提供 —— 这些值在受 git 跟踪的 manifest 中从不以 env 字面量出现（`k8s/secrets.yaml` 已被 `.gitignore`；只跟踪 `k8s/secrets.yaml.example`，实际值保存在集群的 secret store 中）。建议存储的 DB_URL 使用 `sslmode=verify-full`；在 TLS 终止网格后可接受 `require`。`backend/docker-entrypoint.sh` 作为 `backend/Dockerfile` 的 ENTRYPOINT 提供 —— 通用 `/run/secrets/{db_url,jwt_secret}` → env 桥接，适配任何挂载密钥文件的编排器（在 k8s 中因 `secretKeyRef` 内联而当前为 no-op，但保留以纵深防御，并保证未来若回到 compose 文件密钥模式也无需代码改动）。`config.Load()` 拒绝空的 `DB_URL` 与空的 `JWT_SECRET`（`TestLoadRejectsEmptyDBURL`、`TestLoadRejectsEmptyJWTSecret`）；空 `POSTGRES_PASSWORD` 环境变量下 postgres 拒绝 bootstrap（无 `:-default` 占位）。长期：后端应原生支持 `DB_URL_FILE` / `JWT_SECRET_FILE`，届时可移除该 shim |
| M10 | ☑ | Docker | `frontend/.dockerignore` | `COPY . .` 将 tests、docs、`.env`、`coverage.out` 一并打入构建镜像 | `frontend/.dockerignore` 排除 `node_modules`、`.env*`、tests、IDE 配置、docs。后端镜像构建上下文已较精简（Go build 仅拉取 `go.mod`/`go.sum` + 源码目录，暴露面小） |
| M11 | ☑ | Docker | 两个 Dockerfile | 无 `HEALTHCHECK` 指令 | 后端：专用 `/api/livez` 浅探针（不触达 DB/Redis —— 深 `/api/health` 会把瞬时基础设施抖动放大为重启级联）；前端：`wget --spider` 探测 `/`。前端 Dockerfile 在 sed 替换后追加 `nginx -t` 语法护栏，让错误配置出现在构建日志而非运行时 |
| M12 | ☑ | Docker | 两份 compose 文件 | 无 `cap_drop` / `read_only` / `security_opt` | 添加加固 flag |
| M13 | ☑ | Backend | `backend/internal/platform/config/config.go:validateLLMBaseURL`，`backend/internal/llm/vllm/client.go:newHTTPClient` | `LLM_BASE_URL` 接受任意 scheme / host / userinfo；配置错误的 URL 可能命中内部服务（SSRF） | 方案白名单（http/https）、拒绝 userinfo、host IP 类白名单（loopback/RFC1918/link-local/multicast/unspecified 一律拒绝，除非 `LLM_ALLOW_PRIVATE_URL=true`）。vLLM provider 的 `http.Transport.DialContext` 在每次连接上重新执行 IP 类检查，关闭一次性启动 `LookupIP` 会留下的 DNS rebinding 窗口。OpenAI provider 沿用 SDK 默认 transport、无每次拨号检查 —— 已记录剩余风险；prod 中运维仍应设置 `LLM_ALLOW_PRIVATE_URL=false`，除非上游网络可控 |
| M14 | ☑ | Backend | `backend/internal/llmsettings/service.go:validateUserURL`，`backend/internal/llm/openai/client.go` | 每用户 BYOK 的 URL 由用户提供（聊天/Embedding/Tavily），服务端与 agent 会主动拨号 —— 构成 SSRF 面 | 自部署场景下经运营者决定接受：用户 URL 仅做 parse + scheme 白名单（http/https）、拒绝 userinfo、要求 host，且**刻意不做**私网 IP 拦截（localhost vLLM / 局域网 Ollama / `host.docker.internal` 是主要用例）。用户配置统一走 openai 实现，其 SDK transport 无逐次拨号的 IP 类检查。`web_fetch` 保留既有的私网目标拒绝；`web_search` 接受用户的 Tavily base URL（设计上刻意不对称）。密钥 AES-256-GCM 静态加密、永不明文回传；明文只存在于请求内存与 Go→agent 内网跳。端点仅在 `LLM_ENABLED` 门下存在。 |
| M14 | ☑ | 供应链 | `docker-compose.yml:vllm` | 自托管模型服务时对上游制品的信任：HuggingFace 仓库（可能被替换/撤回/投毒）、HF revision 哈希、vLLM 镜像 tag | 固定 vLLM 镜像为 `vllm/vllm-openai:v0.6.3.post1`。默认 `VLLM_MODEL=Qwen/Qwen2.5-3B-Instruct` 为 Apache-2.0 且无 gating。运维切换至 gated 模型（Llama / Mistral / Gemma）时，**必须**先在模型卡上接受许可，再设置 `HUGGING_FACE_HUB_TOKEN`。`vllm_cache` 命名卷在 `compose down` 后保留已下载的权重 —— 将模型 revision 提升到稳定环境前，运维应人工检查快照 |
| M15 | ☐ | Auth | `backend/queries/users.sql:47-68`、`backend/internal/user/service.go:296-327` | 轮换与撤销的竞态：`RotateRefreshToken` 在服务预检查之后无条件插入新行，在途的 refresh 可能在并发 family/user 撤销清扫之后完成 INSERT，从而留下一个有效 token —— `/api/logout-all` 仅返回 204，无硬保证 | 在轮换时要求旧行仍处于未撤销态，或在 refresh 处检查每用户撤销 epoch | 已记录的剩余风险：窗口为 SELECT → CTE 间一次 refresh 往返。闭环需要重构轮换 CTE，使旧行的 `UPDATE ... WHERE revoked_at IS NULL RETURNING` 喂给 INSERT，且零行映射回 `ErrRefreshTokenReuse`。已记录为跟进项 |

### Low（低）/ 信息性

| # | 状态 | 区域 | 文件 | 问题 | 修复 |
|---|---|---|---|---|---|
| L1 | ☑ | Auth | `backend/internal/middleware/auth.go` | Cookie 上线完成后曾移除 `Authorization: Bearer` fallback；后续因 API 改用 token 兼容原生客户端（iOS / Android / Unity / Unreal / 主机 SDK），`Authorization: Bearer` 再次成为**唯一**鉴权来源 | 以 Bearer 替换 cookie；cookie 管线已移除 | Bearer 现为唯一鉴权传输。`auth.go` 仅读取 `Authorization: Bearer` 并拒绝其他 scheme；`middleware.NewHumaAuth` 在 huma 操作级路径上镜像。SPA + 原生 + 游戏 + 主机客户端的 wire 合同一致。`TestAuth_BearerHeaderAccepted` + `TestAuth_MalformedBearerReturns401` 覆盖新合同 |
| L2 | ◌ 不修 | Auth | `backend/internal/user/refresh.go:42-45` | Refresh token 以裸 SHA-256 存储 | 考虑以 HMAC pepper 纵深防御 | 已评估并延期。威胁模型：拿到 refresh-tokens 表读权限的攻击者可尝试离线爆破。32 字节 crypto/rand 熵 + SHA-256 即使无 pepper 也不可爆破；pepper 仅在窄场景（**哈希**泄露而 **pepper**（env var）未泄露）下有帮助，但二者同部署，因此不太可能。引入 pepper 需新增强制 env var 并做破坏性迁移（现有 refresh 哈希会失效）。在出现具体攻击向量之前延期 |
| L3 | ☑ | JWT | `backend/internal/platform/auth/jwt.go:69-77` | 密钥保留在堆上；无 finalizer 置零 | 用于合规的 finalizer（信息性） | 已修复：`NewTokenService` 将密钥复制到新切片并安装 `runtime.SetFinalizer`，在 service 不可达时将副本置零。依 NIST SP 800-57 §5.3.6 best-effort |
| L4 | ☑ | Logging | `backend/internal/middleware/logging.go:32-55` | 原始 query string 入日志（zerolog 已做 JSON 转义；仅 PII 范围） | 记录保留范围 | 已修复：`Logging` 的文档注释现已说明 PII 范围 —— zerolog 已转义（防日志注入），但 query string 可能携带 PII（搜索词、误将密码置于 URL）；运维须相应配置可观测管道的保留/访问策略 |
| L5 | ☑ | Frontend | `frontend/vite.config.ts:13-48` + `frontend/Dockerfile:20` | 源映射被生成并拷贝到 prod 镜像 | 设置 `build.sourcemap = false` | 已修复：`vite.config.ts` 中 `build.sourcemap: false`；已确认 prod 构建不输出 `.map` 文件 |
| L6 | ☑ | Frontend | `frontend/src/main.tsx:35-46` | 无 React error boundary | 用回退 boundary 包裹 `<App>` | 已修复：在 `components/app/` 下新增 `ErrorBoundary` 类组件，在 `main.tsx` 中包裹 `<QueryClientProvider>`。捕获到的错误通过结构化 logger 上报（`react.error` 含完整 message/stack/component-stack）；回退 UI 不展示诊断细节。`ErrorBoundary.test.tsx` 覆盖 happy path、默认回退、自定义回退与 logger 调用 |
| L7 | ☑ | Frontend | `frontend/src/store/authStore.ts:37-127` | 持久化的 `user` 在 token 过期时可能与后端不一致 | 启动时通过服务端 `GET /api/me` 往返同步 | 已修复：新增后端 `GET /api/me` 接口（需鉴权，返回 access token 主体信息）。SPA 上新增 `useAuthReconciliation` hook，挂在 mount：200 通过 `setAuth` 同步；401 调用 `logout()` 清理本地状态；网络/5xx 失败时保留持久化 user（防止网络抖动将有效会话的用户登出）。该探针使用裸 axios（非包装的 `api`），避免响应拦截器的 logout + toast 副作用触发。后端：新增 `Service.GetByID` + `Handler.Me` + 5 项测试；前端：`useAuthReconciliation.test.ts`（5 项测试） |
| L8 | ☑ | Frontend | `frontend/src/features/feeds/components/FeedList.tsx:135` | 空状态时搜索词未限长/未转义直接回显 | 在 input 上加 `maxLength={200}` | 已修复：`App.tsx` 中搜索 `Input` 加 `maxLength={200}`（与后端 `Q` 上限一致，参见 M6） |
| L9 | ☑ | Frontend | `frontend/src/App.tsx:81-90,242-249` | 双击导致删除对话框触发两次 | 在 `deleteFeed.isPending` 时禁用按钮 | 已修复：确认按钮现 `disabled={deleteFeed.isPending}`，在途时可见标签切换为 "Deleting…"。`executeDelete` 在 `deleteFeed.isPending` 为 true 时短路返回（关闭对话框的 React re-render 是异步的，连续双击可能用陈旧的 `confirmDeleteId` 进入 handler）。`App.test.tsx` 新增测试覆盖禁用态与重命名按钮 |
| L10 | ☑ (`e228968`) | Process | `.github/workflows/gitleaks.yml`、`frontend/.husky/pre-commit` | CI 与 pre-commit 缺少密钥扫描 | 新增 `gitleaks-action` | 已修复：新增 `.github/workflows/gitleaks.yml`，在每次 push + PR 上运行 `gitleaks/gitleaks-action@v3.0.0`（按 L11 固定 SHA）：push 跑全量历史，PR 跑 merge-base diff。`frontend/.husky/pre-commit` 在 lint-staged 之前扩展运行 `gitleaks protect --staged --redact --verbose` —— 本地安装为可选（CI 为权威关），缺失二进制不阻塞 commit |
| L11 | ☑ (`a2e609e`) | Process | `.github/workflows/ci.yml` | Action 固定至主版本（`@v4`、`@v5`），未固定至 commit SHA | 固定至 SHA | 已修复：`ci.yml` + `gitleaks.yml` 中全部四个第三方 action 均固定至 commit SHA，并附内联 `# vX.Y.Z` 注释与文件顶部固定策略块：`actions/checkout@ea165f8d... (v4.6.2)`、`actions/setup-go@40f1582b... (v5.6.0)`、`actions/setup-node@49933ea52... (v4.4.0)`、`actions/upload-artifact@11d5960a3... (v4.4.0)`、`gitleaks/gitleaks-action@e0c47f4f... (v3.0.0)`。SHA 升级为刻意的 code-review 步骤 |
| L12 | ☑ (`2026-10-15`) | Repo | `k8s/secrets.yaml` | 已 gitignore，但磁盘上存在 base64 占位符；确认从未部署 | 2026-10-15 修正：`f483125` 将该文件从**工作树**移除（`.gitignore` 第 38–39 行），并未清除历史 —— blob 在所有分支的 `cf51b14` 处仍可达；全部值复核为 dev 占位符（postgres/postgres、`nyx`、compose 内部 DSN、dev JWT 占位符）—— 无需轮换；历史发现在 `.gitleaks.toml` 中带原因 allowlist。2026-10-15 运维确认：按策略仓库 k8s Secret 文件为示例 —— 生产部署从配置中心拉取真实值，因此这些 blob 从未是可部署物料。若仓库未来公开，git filter-repo + 全分支 force-push 的历史清洗仍可按需启用 |
| L13 | ☑ | Repo | `frontend/package.json:36` | `lucide-react@^1.7.0` 是一个不寻常的版本说明符 | 在 npm 上核实 | 已在 npm 仓库验证（`npm view lucide-react`）：包所有者为 Eric Fennis，`repository.url=https://github.com/lucide-icons/lucide.git` —— 官方上游。`1.7.0` 存在于已发布版本列表，`^1.7.0` semver 解析到当前 `latest` tag（`1.39.0`）。`^1.x.y` 的 caret 允许 `>=1.7.0 <2.0.0` —— 对图标库是安全范围。无需变更 |
| L14 | ☑ (`initial`) | Process | 仓库 | 无 `SECURITY.md`（本文件） | 本次提交解决 | 原始审计提交解决 |

## 已验证安全

- **SQL 注入** —— 所有查询均经 sqlc；`ILIKE` 通配符在 Go 侧拼接后作为单参数绑定；无 `fmt.Sprintf` 进入 `pgx.Query`。迁移文件为纯 DDL。
- **错误信息泄露** —— `api.MapError` + `ClassifyAndLog` 不会将 `err.Error()` 序列化进 `details`。已在每个调用点验证（`feed/huma_handler.go:210-232`、`user/huma_handler.go:152-203`）。
- **Token 存储** —— 裸 token 不入库（SHA-256 哈希）也不入日志（grep 已确认）。Refresh token 为 32 字节随机数。
- **Cookie 标志** —— 历史说明：cookie 鉴权已退役，改用 Bearer token（RFC 6750）。Cookie 不再承载鉴权状态；`Access-Control-Allow-Credentials` 故意永不设置（Bearer 是普通 Authorization 头，非凭据）。`__Host-` 前缀逻辑 + `SameSite=None → Secure` 跨字段校验现已过时并已移除。
- **JWT 密钥** —— 若为默认字面量或 `< 32` 字节则在启动时拒绝；`auth.NewTokenService` 独立重复该检查。
- **Bcrypt** —— `DefaultCost`（10）；1 MiB 请求体上限避免鉴权前 DoS。
- **Refresh-token 轮换** —— 原子 CTE 在一条语句中标记 `revoked_at` + `replaced_by_id`，无 TOCTOU 竞态。
- **XSS 暴露面** —— `frontend/src/**` 中零 `dangerouslySetInnerHTML` / `innerHTML` / `eval` / `javascript:` URL / 动态 `href`（grep 已确认）。当前无需 DOMPurify；如新增 MD 渲染需重新评估。
- **Single-flight refresh** —— 通过共享 `refreshing` Promise + `_retried` 标志正确处理（`api.ts:26-127`）。
- **Zustand 迁移** —— `migrate` 从 cookie 前 payload 中剥离所有 token 形态字段（fail-safe）。
- **状态变更方法** —— 全部为 POST/PUT/DELETE；无状态变更 GET。
- **依赖** —— 当前版本无已知在野利用 CVE（`golang-jwt v5.3.1`、`chi v5.3.1`、`pgx v5.10.0`、`golang.org/x/crypto v0.54.0`、`axios 1.13.6`、React 19.2、Vite 8、Tailwind 4.2）。
- **仓库卫生** —— `.env` 已 gitignore，无硬编码密钥，多阶段 Dockerfile，非 root USER，prod compose 将 DB/Redis/Jaeger 隔离在内网。
- **日志注入** —— zerolog 对用户输入做 JSON 转义；通过 `X-Request-ID` 的响应拆分被 `net/http` 响应头校验阻断。
- **授权** —— 无对象级漏洞。Feed create/update/delete 需鉴权且按 owner 限定：每条查询均带 `WHERE user_id = @user_id`，跨 owner 写入映射为 404（单一 `feed.ErrNotFound`，不泄露 `ErrForbidden`）；`GET /api/feeds` 需鉴权，仅返回调用者自己的行。`feeds.user_id` 列以 FK 指向 `users.id` 且无级联（用户通过 `deleted_at` 软删除；未来硬删除路径必须先重分配或删除该用户的 feeds）。
- **按用户的 LLM 凭据** —— `user_llm_settings` 以 `user_id` 为主键并 `ON DELETE CASCADE`，读写一律经 JWT 校验后的 `reqctx.UserIDFromContext` 走 `WHERE user_id = @user_id`，API key 落库加密、响应只回掩码、明文仅可写。前端侧配置弹窗按身份隔离：`authStore.user.id` 变化（含登出→登录）时表单状态重置，弹窗打开状态由身份持有而非布尔量，清除上一个身份的 `llm-config` 查询缓存，且身份已变后返回的保存/清除结果一律丢弃 —— 常驻组件不得把上一用户的 key 带进下一用户的会话。该保证的边界是单标签页：`authStore.user` 未做跨标签同步，共享浏览器配置下另一个标签页可能通过 `BroadcastChannel` 握手采纳了另一身份的 token，却仍保留旧的 `user`，直到它自己的 401 触发登出。该缺口作为后续项跟踪。

## 数据处理

Python agent 运行时（聊天记录、派生偏好、运行元数据、langGraph checkpoint 状态、审计轨迹）对主体数据的处理集中在 `docs/agent-deployment.md` 的 "GDPR 数据处理：导出与删除" 一节 —— 包括数据清单（每张表存储内容、保留窗口、删除覆盖）、`GET /agent/me/export` + `DELETE /agent/me` 的运维手册，以及 kill switch。此处重申安全相关的不变式：

- **信任边界。** agent 的入站请求由共享的 `AGENT_SERVICE_TOKEN` 认证：纯 ASGI 中间件对除精确路径 `/healthz` 外的所有路由要求 `X-Nyx-Service-Token`，按字节做常量时间比较（非 ASCII 值干净地返回 401），并在任何 handler 之前拒绝；token 值不写日志。Go 层校验 JWT 并注入数值型 `X-Nyx-User-Id`。缺失/非整数头返回 400（绝不隐式使用 user 0），任何接口不接受跨用户参数，且无管理员越权。agent 路径开启时，Python 服务在缺少 ≥ 32 字节 token 时拒绝启动；Go API 保持运行，但所有 agent 路由 fail-closed（503）。该跳把凭据放在请求头里，因此传输层必须保护它：`PYTHON_AGENT_URL` 若以明文 `http://` 指向公网可达主机，启动时即被拒绝（loopback 与无点号的私网服务名仍放行；带点号的私网名可用 `AGENT_ALLOW_PRIVATE_URL=true` 显式放行）。URL 被拒时 API 保持运行，所有 agent 路由 fail-closed（503）。
- **删除是物理的。** `DELETE /agent/me` 在 chat sessions/messages、preferences、run metadata、langGraph checkpoint threads 上物理删除行（无墓碑），并追加一行审计记录。
- **审计行仅承载计数，绝不承载内容。** `agent.data_audit_log.payload_summary` 按结构为 `dict[str, int]` —— 删除路径不得为刚删除的数据制造影子副本。
- **备份恢复规则。** 恢复仅限 DR 场景；已记录的删除（在 `agent.data_audit_log` 中 `action = 'erase'` 可查）必须在恢复副本重新对外服务之前重新应用。

## 建议修复顺序

1. **后端加固** —— H1–H5、M1–M7、L1–L4 —— 已落地：`2fd84e5`（H1）、`93b2ef2`（H2–H5、M1–M7、L1、L3、L4）；M5 自初始提交沿用
2. **前端加固** —— H6–H8、M8、L5–L9 —— 已落地（各 SHA 见表）
3. **Docker + compose** —— H9–H12、M9–M12、L12 —— 已落地（见上表）
4. **流程** —— L10–L11、L13 —— 已落地（本批）

将 ☐ 改为 ☑，并在 issue 列追加修复 commit SHA 即可勾选该行。
