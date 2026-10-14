Slice 1 e2e evidence (2026-10-14, side agent on :9100 against the running Postgres + vLLM):
- send curl POST /agent/run (X-Nyx-User-Id: 999, messages=[{user,'hi'}]); observed SSE frames: run_id -> note{fresh_run} -> plan (1 llm step) -> step_start -> delta ('Hello! How can I help you today?') -> step_end -> done -> terminator
- SELECT * FROM agent.chat_sessions WHERE user_id=999: 1 row, id=<run_id uuid>
- SELECT * FROM agent.chat_messages WHERE session_id=<run_id>: 2 rows (user 'hi' + assistant 'Hello! ...', tokens=1000)
- backdate last_active_at to 400 days ago + 2 chat_iteration() calls with grace=0 + 50ms sleep: counter agent_chat_retention_deleted_total incremented by 1; chat_sessions row gone (FK CASCADE carried the messages)
- metrics endpoint exposes both agent_run_retention_deleted_total and agent_chat_retention_deleted_total
