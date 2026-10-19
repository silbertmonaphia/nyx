# PostgreSQL DML 审查 — 2026-10-18

## 范围与方法

审查了读写行数据的所有 SQL 语句与事务:

- **Go 后端** —— `backend/queries/*.sql` 以及执行它们的 repository / service 调用点、refresh token 轮换与清理路径、RAG 索引器/检索器,以及在已有数据之上应用 DDL 的迁移(`000015`)。
- **Python agent** —— `agent/chat_store.py`、`prefs.py`、`run_metadata.py`、`audit.py`、`retention.py`、`api/chat_sessions.py`、`api/gdpr.py`,以及涉及数据安全的 Alembic 链,与 checkpointer/pool 的连接装配。
- **跨 runtime** —— golang-migrate 与 Alembic 之间的 schema/版本表归属、`search_path`、共享的 `AsyncPostgresSaver` 连接。

方法:对照真实 SQL 做代码溯源,再加一次在临时 `pgvector/pgvector:pg17` 容器(PostgreSQL 17.11,pgvector 0.8.6)上的实地复现。容器随后停止,没有修改任何仓库文件。

已运行的验证命令:

- `go build ./...` —— 通过
- `SKIP_CONTAINERS=true go test ./...` —— 通过
- `golangci-lint run --timeout=5m` —— 通过
- `sqlc diff` —— 通过(生成代码无漂移)
- `make migration-check` —— 通过
- Python agent 测试套件(无 DB DSN):427 通过,151 跳过。跳过的集合包含所有依赖 DB 的套件(chat store、prefs、GDPR 存储、retention、run metadata、会话 API、graph 持久化),因此以下 Python 并发相关发现是基于 READ COMMITTED 语义的静态分析,而非实地复现。

配套文档:`reviews/postgresql_ddl_2026-10-18.md` 已覆盖索引/类型/约束设计。本审查聚焦于查询语义、事务边界、竞态以及迁移的数据安全;重叠的索引发现以交叉引用代替重复。

**总体结论:建议 reject。** 两个高危缺陷(一个迁移在非空数据上会让启动崩溃;一个 refresh token 轮换存在破洞,会让重放检测失效),外加六个中危的合规/正确性问题。

---

## 高危

### H1. `000015` 在任何非空 `feed_embeddings` 表上都失败;新部署会启动死循环

`backend/migrations/000015_resize_feed_embedding.up.sql:26-29`(down:.down.sql:6-9)

```sql
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings
    ADD COLUMN embedding vector(1024) NOT NULL;
```

`DROP COLUMN` 删除列后会保留所有行。紧接着无默认值的 `ADD COLUMN ... NOT NULL` 在非空表上立即失败:

```
ERROR: column "embedding" of relation "feed_embeddings" contains null values
```

在 PostgreSQL 17.11 上实地复现成功。迁移文件顶部的注释声称该操作会"清空所有行"——并不会。`backend/cmd/api/main.go:120` 在迁移失败时调用 `log.Fatal`,因此曾经使用过 RAG 的部署在升级后将无法启动。down 迁移针对 `vector(1536)` 的形态完全一样。

CI 抓不到这个:testcontainers 与集成测试都从空库开始,因此列被加到零行上,迁移自然成功。

修复:列交换前先删除派生行,两个方向都要:

```sql
DELETE FROM feed_embeddings;
DROP INDEX IF EXISTS feed_embeddings_embedding_hnsw_idx;
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings ADD COLUMN embedding vector(1024) NOT NULL;
CREATE INDEX feed_embeddings_embedding_hnsw_idx
    ON feed_embeddings USING hnsw (embedding vector_cosine_ops);
```

嵌入向量可通过 RAG backfill 重建。再加一个 CI job,在用上一版本预填数据的库上(而非空库)应用迁移链。

### H2. Refresh token 轮换并非原子;并发重放会产生多个有效 token

`backend/queries/users.sql:47-68`、`backend/internal/user/service.go:286-348`

`RotateRefreshToken` **无条件**插入 replacement,驱动的 `UPDATE` 仅按 id 匹配:

```sql
WITH new_row AS (
    INSERT INTO refresh_tokens (...)
    VALUES (...) RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id,
    revoked_at     = COALESCE(refresh_tokens.revoked_at, now())
FROM new_row
WHERE refresh_tokens.id = @old_id
RETURNING new_row.*;
```

没有 `AND revoked_at IS NULL`。`Service.Refresh` 先读取旧行(`GetRefreshTokenByHash`),检查 `row.RevokedAt == nil`,再轮换——经典的 TOCTOU。两个并发的 refresh 请求持有同一个 token,都观察到 `NULL`,都插入新行,且都成功。实地复现:对同一旧行执行两次 CTE,结果是两行 `revoked_at IS NULL`,也就是一个 token 衍生出两个有效会话,而重放检测分支从未触发。旧行的 `replaced_by_id` 指向第二个 replacement,轮换链也断了。

已有测试未覆盖这个场景:service 测试是顺序的,且对 repository 打了桩;集成测试只轮换一次。

修复:让旧行的更新作为驱动,并让插入依赖于它:

```sql
WITH old_row AS (
    UPDATE refresh_tokens
    SET revoked_at = now()
    WHERE id = @old_id AND revoked_at IS NULL
    RETURNING *
), new_row AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, replaced_by_id, expires_at)
    SELECT @user_id, @token_hash, @family_id, old_row.id, @expires_at
    FROM old_row
    RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id
FROM new_row
WHERE refresh_tokens.id = @old_id
RETURNING new_row.*;
```

第二个并发轮换会阻塞在行锁上,重新评估 `revoked_at IS NULL`,匹配零行,不再插入,并返回 `pgx.ErrNoRows`;service 必须把它映射回重放分支(吊销家族 + `ErrRefreshTokenReuse`)。加一个基于真实数据库的双 goroutine 测试,断言恰好一条新的 active 行。

剩余竞态(较低严重度):`RevokeRefreshTokenFamily` 看不到轮换并发刚插入的 token,因此 logout 与 refresh 抢跑仍可能留下一条有效 token。要关掉这点,需要锁住 family(或加一个 family 级的吊销时间戳)。

---

## 中危 —— Go 后端

### M1. 带过滤的 HNSW 扫描会让 RAG 检索结果不足

`backend/queries/rag.sql:25-37`、`backend/migrations/000014_add_feed_embeddings.up.sql:38-43`

`RetrieveFeedPassages` 在按 cosine 距离排序后再过滤 `fe.user_id`、`f.user_id` 与 `f.deleted_at`。在默认 `hnsw.iterative_scan=off` 下,pgvector 从 **全局** 索引收齐 `ef_search` 候选后,SQL 过滤才开始执行,因此其他用户的行可能耗尽候选集。

实地复现:为某用户一行、为另一用户 2000 行,`ORDER BY embedding <=> $1 LIMIT 5` 对目标用户返回 **零** 行。`hnsw.iterative_scan=relaxed_order` 与 `strict_order`(配 `max_scan_tuples=100000`)在本次复现中也没有找回该行。

用户可见影响:RAG 静默地少检索了实际存在的段落,而恰好在小语料用户身上,回答会丢掉上下文。

修复选项(按把握度排序):

1. 在排序前先物化用户行:
   ```sql
   WITH user_embeddings AS MATERIALIZED (
       SELECT fe.feed_id, fe.embedding, f.title, f.description
       FROM feed_embeddings fe
       JOIN feeds f ON f.id = fe.feed_id
       WHERE fe.user_id = $2 AND f.user_id = $2 AND f.deleted_at IS NULL
   )
   SELECT ..., (1 - (embedding <=> $1))::float8 AS score
   FROM user_embeddings
   ORDER BY embedding <=> $1
   LIMIT $3;
   ```
   每用户语料都小,在 `user_id` btree 上做精确 KNN 成本很低。
2. 在真实查询计划上验证 `hnsw.iterative_scan = relaxed_order`,再决定依赖它。
3. 按用户对嵌入表做分区(代价更高)。

同时修正迁移注释中"post-filter 开销可忽略"的描述。

### M2. `page` 无上限会溢出 `int32`;负 `OFFSET` 返回 500

`backend/internal/feed/huma_handler.go:190`、`backend/internal/feed/repository.go:34,142`、`backend/internal/tool/feeds.go:127-138`

`Page` 只有 `minimum:"1"` 没有 maximum;只有 `pageSize` 被夹紧。`offset := (page - 1) * pageSize` 用 `int` 计算,然后被 `toInt32` 截断。`GET /api/feeds?page=200000000&page_size=20` 得到 3,999,999,980,折回成负的 `int32`;PostgreSQL 报 `ERROR: OFFSET must not be negative`,被映射成 500。MCP tool 路径套用同一套归一化,同样可达。代码注释声称 handler 端的 cap 让值落在 `int32` 内,这是错的。

修复:在两个入口都限制 `page`(例如 1,000,000),或转换前把 offset 钳到 0;越界输入应返回 400。若改了 Huma tag,记得重新生成 `api/openapi.json`。

### M3. feed 列表的索引与列表谓词不匹配

`backend/migrations/000006_add_movies_indexes.up.sql`

DDL 审查中已经指出:没有 `(user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL`;`idx_movies_active ON (id) WHERE deleted_at IS NULL` 与主键冗余;`movies → feeds` 重命名后,索引名仍陈旧。当下数据量下这只是排序问题,不算正确性 bug。

### M4. refresh token 清理会误删最近刚吊销的行;列无索引

`backend/queries/users.sql:111-120`

```sql
DELETE FROM refresh_tokens
WHERE created_at < @cutoff
  AND (revoked_at IS NOT NULL OR expires_at < now());
```

保留锚点是 `created_at`,因此一行十天前创建、一分钟前刚吊销,在下一次每小时扫描就会被删除——与"已吊销行保留供事件调查"的注释相反。`created_at` 上无索引,自引用外键(`replaced_by_id ON DELETE SET NULL`)上也没有索引,所以每次清理都在扫。

修复:将保留锚点改为 `COALESCE(revoked_at, expires_at)`,并加上 `(created_at)` 与 `(replaced_by_id)` 索引(DDL 审查已列出这两个索引)。

### M5. `GetAll` "count/page 一致"的注释与代码不符

`backend/internal/feed/repository.go:126-130,135-215`

`BeginTx(ctx, pgx.TxOptions{})` 使用 READ COMMITTED;每条语句都取新快照,因此并发插入可以让 `total` 与返回的分页(包括 `has_more`)不一致。改用 `pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}` 做只读,或修正注释。

### M6. pg_trgm "温和跳过"实际上啥也没跳过(实地复现)

`backend/migrations/000006_add_movies_indexes.up.sql:16-22`,`000007_add_movies_description_trgm_index.up.sql` 中相同模式

`DO` 块捕获 `CREATE EXTENSION pg_trgm` 抛出的 `insufficient_privilege`,并把它降级为 NOTICE,但紧随其后的 `CREATE INDEX ... USING GIN (title gin_trgm_ops)` 是无条件执行的。以非超级用户实地复现:NOTICE 先出,然后迁移以 `operator class "gin_trgm_ops" does not exist` 中止。把建索引放进受保护块内(或放进第二个用 `pg_extension` 检查守卫的块),或用一个可操作的报错信息显式失败。

### M7. 其他 Go 低危发现

- **`CREATE EXTENSION vector` 未受保护**(`000014_add_feed_embeddings.up.sql:27`)。托管 Postgres 缺少 pgvector 或缺 CREATE 权限时,即使 RAG 关闭,启动时所有迁移也会失败。文档写明硬性前置条件,或用显式的可操作错误包裹。
- **Down 迁移没有真正回滚 up:** `000002` 按 title 删除种子行(可能误删同名用户行);`000009`/`000010` 一旦存在 NULL email 回滚失败;`000012` 会留下占位用户;`000016` 是 no-op。
- **`make sqlc-diff` 漏掉 RAG 包**(`backend/Makefile:43-46` 只 diff 了 `internal/feed/db internal/user/db`;CI 跑同一目标)。改用 `sqlc diff`,或加入 `internal/rag/db`。同时 `sqlc.yaml` 中 RAG schema 列表停留在 `000014`,导致生成的代码按 `vector(1536)` 校验,而运行时是 `vector(1024)`。
- **`BackfillOnce` 是进程级、用户级的 `sync.Once`**(`backend/internal/rag/service.go:207-220`)。失败的 backfill,或超过 `maxBackfill × maxBackfillIterations` 个 feed 的用户,需等到进程重启才会重试。内联注释所谓"下一次 chat 请求会重试"是错的。在 H1 修复清空嵌入表后,这点影响更大。
- 跨类型的 ID(`users.id INTEGER` 对 `feeds.user_id BIGINT`)合法但不一致;DDL 审查已覆盖。

---

## Python agent

### P1. 高危(合规)—— GDPR erase 会与进行中的 checkpoint 写入产生竞态

`backend/agent_py/agent/api/gdpr.py:223-279`

erase 路径会删除 `agent.runs` 与 `agent.chat_sessions`,然后对 run id 与 session id 的并集调用 `checkpointer.adelete_thread`。同一用户一个已经在飞的 SSE run 不会被取消或围栏。其后每一个图步骤都会再次为已删除的 thread 写 `langgraph.checkpoint_*`。

这些行包含会话状态,但命名该 thread 的 `agent.runs` / `chat_sessions` 行已经没了。导出路径或随后的保留/erase 轮次都无法再发现该 id,而 retention 只 GC 它刚刚执行的 delete 涉及的 id。因此被擦除的内容会无限期残留。

修复:三选一 ——

- 跟踪每个用户的活跃 run id,在 erase 时取消/延期/再次 GC;
- 让 erase 路径写入一个 per-user tombstone,run 生命周期遵守它;
- 给 retention cron 增加孤儿 thread 扫描:GC `langgraph` 中既无 `agent.runs` 行也无 `chat_sessions` 行的 thread(这也覆盖失败 checkpoint GC 留下的悬空 thread)。

加一个 erase 在 stream 进行中执行的测试。(静态分析;实地复现不可行,因为 Python DB 套件需要 `TEST_ASYNC_PG_DSN`。)

### P2. 中危 —— 偏好 upsert 可能复活已擦除的行

`backend/agent_py/agent/prefs.py:175-193`

```sql
INSERT INTO chat_user_prefs (user_id, key, value, source_session_id)
SELECT $1, $2, $3::jsonb, $4::uuid
WHERE $4::uuid IS NULL OR EXISTS (
    SELECT 1 FROM chat_sessions s WHERE s.id = $4::uuid
)
ON CONFLICT (user_id, key) DO UPDATE SET ...
```

docstring 称该守卫"无法在检查与写入之间被竞态击穿"——可以。`EXISTS` 只是裸的 READ COMMITTED 快照,既没加锁,也没检查 `s.user_id = $1` 或 `s.deleted_at IS NULL`。

竞态:detached 偏好抽取任务(`[DONE]` 后触发,`main.py:1279-1289`)在 `delete_all_for_user` 尚未提交时取快照,看到旧的 session 行,执行 insert;erase 的 `DELETE FROM chat_user_prefs WHERE user_id = $1` 已经扫过或排在它之前,二者都提交成功。`ON CONFLICT DO UPDATE` 变体会等到已擦除的行,在冲突解除后插入。`delete_orphaned` 在下一次 retention tick 上收走该行,**仅当 chat retention 启用时**才会;否则永久残留。

修复:在 `EXISTS` 中加 `AND s.user_id = $1 AND s.deleted_at IS NULL`,并与 erase 串行(对 session 行 `FOR KEY SHARE`,或双方持有的 per-user advisory lock)。加一个双连接竞态测试。

### P3. 中危 —— `record_message` 对未净化的文本会让 stream 失效

`backend/agent_py/agent/chat_store.py:431-451,468-479`

retry-on-`DataError` 路径只会用 `steps = NULL` 重试;当 `steps_json is None` 时重新抛出。`content` 与 `tool_calls` 未做净化,每个调用点只捕获 `ChatStoreUnavailable`(`main.py:900-913` 在首个 `run_id` 帧前;`main.py:1222-1242` 在 shielded finalize 任务中)。

一个含 `\u0000`(合法 JSON)的 JSON 请求会产生带 NUL 的 Python 字符串;asyncpg 抛 `CharacterNotInRepertoireError`(已验证是 `DataError` 的子类)。孤立 surrogate 也会抛 `UnicodeEncodeError`。结果:用户消息写入在流开始前失败,或 finalize 任务在流之后抛错,而不是文档所述的尽力而为行为。

修复:在信任边界处剥离 NUL / 用 `errors="replace"` 编码,并把残留的 `DataError` / `UnicodeEncodeError` 转译为 `ChatStoreUnavailable`(在 steps-only 重试之后)。为 content、steps、tool_calls 中的 `\x00` 各加测试。

### P4. 低危 —— `list_messages` 没有提供文档声称的 404 保证

`backend/agent_py/agent/chat_store.py:335-341,349-368`

docstring 称该事务能让并发的 purge 不把"应该是 404"的结果变成 `[]`。在 READ COMMITTED 下,每条语句取新快照,ownership probe 也不加锁,因此并发 GDPR erase 可以在 probe 与消息读取之间提交,结果返回 `200 {"messages": []}` 而不是 404。用 `SELECT 1 ... FOR SHARE` 锁住 probe,或用 REPEATABLE READ。

### P5. 低危 —— `record_message` 的 INSERT 没有用户范围限制

`backend/agent_py/agent/chat_store.py:432-451`

INSERT 仅靠外键约束,`user_id` 守卫放在后续的 `UPDATE` 上,其受影响行数被忽略,且失败时不会回滚 insert。当下没有可被利用的请求路径——resume 判定(`main.py:837-859`)是用户范围的,`POST /agent/sessions` 在服务端生成 UUID——但任何未来调用者都会破坏这个不变量。更糟的是,`tests/test_chat_store.py:122-132` 把"跨用户 insert 成功"固化为预期。

修复:`INSERT INTO chat_messages (...) SELECT ... FROM chat_sessions WHERE id=$1 AND user_id=$N AND deleted_at IS NULL RETURNING id`,把零行映射为 `ChatStoreUnavailable`,并把测试翻成断言拒绝。

### P6. 低危 —— Saver 的 teardown 是静默 no-op

`backend/agent_py/agent/main.py:481-485,536-539`

已安装的 `AsyncPostgresSaver` 没有 `__aenter__` / `__aexit__`(已用安装的 langgraph 包核对);`from_conn_string` 是 `@asynccontextmanager`。lifespan 调用 `await checkpointer.__aexit__(...)`,用裸 `except` 捕获产生的 `AttributeError`,从不退出 stash 的 `saver._cm`——psycopg 连接在 shutdown 时没有关闭。修复:`await checkpointer._cm.__aexit__(None, None, None)`,且不要吞掉意料之外的错误。

### P7. 低危 —— 流开始前的 `get_run` 未做防御

`backend/agent_py/agent/main.py:838`

`get_run` 在重试后仍可能抛 `RunMetadataUnavailable`;邻近的 resume 路径调用(`get_session`、`create_run`)都是尽力而为。resume 期间一次短暂的 pool 抖动会返回 500,而非退化为走 fresh run,在 `run_id` 帧之前。捕获并视为 fresh(warn-log)。

### P8. 低危 —— Alembic 日志会输出完整 DSN,包括密码

`backend/agent_py/migrations/env.py:86`

`logger.info("alembic: DSN %s", url)` 一旦加了任何 INFO 级 logging 配置(或 `fileConfig`),就会外泄凭据。当下应用把 root 设为 WARNING,CLI 无 logging 配置,因此是潜伏的、不是激活的。删掉这一行,或用 `hide_password=True` 渲染。

### P9. 低危 —— retention 谓词缺少支撑索引

- `backend/agent_py/agent/run_metadata.py:268-274` —— `ended_at < cutoff AND status IN ('completed','failed')`;只有 `(user_id, started_at DESC)`,每个 tick 都是顺序扫描。
- `backend/agent_py/agent/chat_store.py:640-647` —— `last_active_at < cutoff AND deleted_at IS NULL`;现有偏索引以 `user_id` 开头,是全表扫描。
- `backend/agent_py/agent/chat_store.py:666-673` —— `deleted_at IS NOT NULL AND deleted_at < cutoff`;偏索引恰好排除了这些行,是全表扫描。
- `backend/agent_py/agent/prefs.py:319-330` —— 对 `chat_user_prefs` 做 anti-join;`source_session_id` 没有索引,全表扫描。

两阶段 retention 也都是单条无界的 `UPDATE` / `DELETE`。加 DDL 审查中列出的偏索引;若表可能长成大表,用 `LIMIT` / `ctid` 循环做分批。

### P10. 中危(测试/CI)—— agent 测试套件不在 CI 中运行

`.github/workflows/ci.yml:89-154` 只在空库上跑两次 `alembic upgrade head`。没有 `backend/agent_py` 的 `pytest` job;没有 `TEST_ASYNC_PG_DSN` 时每个依赖 DB 的套件都自我 skip(本地 427 通过 / 151 跳过)。schema check 只能证明链能跑,不能证明任何查询/事务行为正确。加一个 Postgres-service 的 pytest job,至少包括上面 erase / rotation 竞态的一个双连接测试。

### P11. 其他 Python 低危发现

- `_normalize_database_url`(`backend/agent_py/agent/config.py:319-369`)接受 `postgresql+asyncpg://`(这是 CI 中 Alembic 用的 scheme);asyncpg 在应用启动时拒绝它。统一 scheme,或抛清晰的配置错误。
- `create_run` 在被重试的函数内生成 UUID(`run_metadata.py:139-149`)。服务器已经提交后若发生 `PostgresConnectionError`,重试会留下一条"幽灵"的 `running` 行(新 id)。应在 retry loop 外面生成 UUID;同样的"重复插入"类问题在 `record_message` 与 `audit.record` 提交后断连时也存在。
- `_with_retry` 捕获任何 `TimeoutError`(包括操作自身抛出的),并把它误归为 pool exhaustion。建议仅捕获 `asyncio.TimeoutError`,且作用域限定在 `pool.acquire` 周围。

---

## 跨 runtime

- **Schema 隔离是干净的。** Go 持有 `public.*`(`schema_migrations` 经 golang-migrate);Python 持有 `agent.*`(`alembic_version` 经 `version_table_schema="agent"`)与 `langgraph.*`。Python 的 `search_path` 是 `langgraph,agent`,排除了 `public`,因此任何非限定的 Go 表名都会失败关闭。不存在表名或版本表冲突。
- **DSN 中 `options=-c search_path=langgraph,agent` 对 asyncpg 是有效的:** 未知查询参数会变成 `server_settings`,PostgreSQL 的启动包识别 `options`;Alembic 在 `env.py` 中再次 SET 自己的 search_path。基于 psycopg 的 saver 会剥离该 option,并在 `setup()` 前显式设置 search_path,正确。
- **共享一个 `AsyncPostgresSaver` 连接并发运行是安全的:** 安装的 langgraph `_cursor()` 把每次操作都包在 `self.lock`(`asyncio.Lock`)中,因此并发运行是序列化而非交错事务。子代理曾标记的风险在已安装源码中不复现。
- 没有 Python 代码引用 `public.*` 表。

---

## 确认无误之处

- 无字面拼接 SQL。Go 端 sqlc 参数化贯穿始终;Python 只从常量列名+绑定参数构建 SQL(`_RUN_COLUMNS`、两段 `list_sessions`),唯一的 DDL 标识符插值是常量 schema 名。
- `feeds` 查询中的 owner scoping 是完备的;每一次读写都带 `user_id` 过滤,跨 owner 访问收敛为 `ErrNotFound`。MCP/tool 路径走 `feed.Service` 并带 `reqctx` scoping,无直接 SQL。
- sqlc 注解与生成代码同步(`sqlc diff` exit 0);`RETURNING *` 列序与生成 struct 匹配;可空列正确使用 `pgtype`。
- refresh token 自 stamp(`InsertRefreshToken` + `StampRefreshTokenFamily`)正确包在事务中。
- `000016` 的 `setval(pg_get_serial_sequence('users','id'), COALESCE(MAX(id),1), MAX(id) IS NOT NULL)` 同时正确处理空与非空两种情况。
- pgxpool 配置、`AfterConnect` 中的 `pgxvec.RegisterTypes`、`ErrNoRows` 映射、迁移 runner 中的 `ErrNoChange` 处理都无误。
- cache-aside 不变量成立:每次 cache 调用都用 `log.Warn` 吞错,cache key 包含 `user_id`(`feeds:u=%d:q=%s:p=%d:s=%d:o=%s`)。注意 `CLAUDE.md` 中仍写着旧的、不带 user 的 key 格式。
- Alembic 链:单一 head `0008_chat_message_steps`,线性 `down_revision` 链接,每张表/索引明确在 `agent` schema 中,`timestamptz` / `jsonb` / `uuid` / identity / `ON DELETE CASCADE` 使用正确。
- `asyncpg.create_pool` 在使用前已 `await`(`build_pool`),匹配 asyncpg 0.32+ 契约。

---

## 优先级建议

### P0 —— 上线阻塞项

1. 修 `000015` 的 up/down:列交换前先 `DELETE FROM feed_embeddings;`;加一个"种子升级版库"的 CI 测试。
2. 让 `RotateRefreshToken` 原子化(`revoked_at IS NULL` 作为驱动谓词);零行时映射到重放分支;加并发重放测试。

### P1 —— 安全 / 合规 / 数据完整性

3. 把 GDPR erase 与进行中的 run 隔离,或在 retention 中增加孤儿 thread 扫描(Python P1)。
4. 在 prefs 的 `EXISTS` 守卫中加 `user_id` + `deleted_at`,并与 erase 串行(Python P2)。
5. 把 `chat_messages` 的 INSERT 限定 `user_id`;修那个把"跨用户 insert 成功"固化的测试(Python P5)。
6. 在 `record_message` 中净化 NUL/孤立 surrogate,并把残留 `DataError` 转译为 `ChatStoreUnavailable`(Python P3)。
7. 在 HTTP 与 MCP 入口都限制 `page` / 守住 offset 转换(Go M2)。

### P2 —— 并发与规模下的正确性

8. 用每用户物化的精确扫描(或在真实计划上验证 iterative scan)替换带过滤的 HNSW 检索(Go M1)。
9. 对 count/page 与 ownership-probe 事务使用 REPEATABLE READ(或 `FOR SHARE`)(Go M5、Python P4)。
10. 把 refresh token 保留锚到 `COALESCE(revoked_at, expires_at)`,并补齐缺失的索引(Go M4)。
11. 加 retention 索引,并考虑把扫描分批(Python P9)。
12. 在 CI 中运行 agent pytest 套件(配 `TEST_ASYNC_PG_DSN`),包括并发测试(Python P10)。

### P3 —— 加固 / 清理

13. 修 pg_trgm 守卫;守卫/文档化 vector 扩展;修复 down 迁移的不对称性(Go M6/M7)。
14. 把 `make sqlc-diff` 扩展到 `internal/rag/db`,更新 `sqlc.yaml` 到最新迁移,并让 `BackfillOnce` 不再是"一次性、不重试"的进程级闸门(Go M7)。
15. 修 saver teardown、流前的 `get_run` 处理、DSN 日志,以及非幂等的 insert 重试(Python P6/P7/P8/P11)。

---

## 复现附录

四个 Go 端复现全部在 `pgvector/pgvector:pg17`(PostgreSQL 17.11,pgvector 0.8.6)上完成。

**1. `000015` 在非空表上的失败**

```sql
CREATE TABLE feed_embeddings (
    feed_id integer PRIMARY KEY,
    user_id bigint NOT NULL,
    embedding vector(1536) NOT NULL,
    chunk_text text NOT NULL,
    content_hash bytea NOT NULL
);
INSERT INTO feed_embeddings VALUES (1, 1, array_fill(0.1, ARRAY[1536])::vector, 'x', '\x00');
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings ADD COLUMN embedding vector(1024) NOT NULL;
-- ERROR: column "embedding" of relation "feed_embeddings" contains null values
```

**2. Refresh 轮换重放**

```sql
-- old row id = 1, family_id = 1
WITH new_row AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, replaced_by_id, expires_at)
    VALUES (1, '\x02', 1, 1, now() + interval '7 days') RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id, revoked_at = COALESCE(revoked_at, now())
FROM new_row WHERE refresh_tokens.id = 1 RETURNING new_row.id;
-- succeeded

-- run the same statement again with '\x03'
-- succeeded again → two rows with revoked_at IS NULL
```

**3. 带过滤 HNSW 返回数量不足**

```sql
-- 2,000 rows for user 2, one row for user 1; HNSW index on embedding
SET enable_seqscan = off;
SET hnsw.ef_search = 40;
SELECT count(*) FROM (
    SELECT id FROM ragtest
    WHERE user_id = 1
    ORDER BY embedding <=> (SELECT array_agg(random())::vector FROM generate_series(1, 1024))
    LIMIT 5
) q;
-- 0
```

**4. 负 OFFSET**

```sql
SELECT 1 OFFSET -1;
-- ERROR: OFFSET must not be negative
```

产生该值的 page 上溢:

```text
page = 21474837, page_size = 100
offset = (page - 1) * page_size = 2,147,483,600
int32(offset)                       = -2,147,483,696
```
