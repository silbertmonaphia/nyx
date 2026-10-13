import { test, expect, type Page } from '@playwright/test';

/**
 * Browser-level verification for `agent-langfuse-tracing`.
 *
 * What unit / integration tests can't catch (this file exists to fill the gap):
 *   - the full user path: SPA chat → Go proxy → Python agent → vLLM, with the
 *     Langfuse callback handler riding the live graph invocation;
 *   - the trace actually arriving in a REAL Langfuse instance through the
 *     OTLP exporter (async: SDK batch → langfuse-web → Redis → worker →
 *     ClickHouse — hence the poll loop, not a single assertion);
 *   - the trace being addressable the way an operator uses it: filtered by
 *     the verified user id, with generations carrying prompt/reply/usage and
 *     the tool structure visible.
 *
 * Assertions run against Langfuse's own public read API — the consumer-side
 * parse of the wire contract (not just the producer's in-process shape).
 *
 * Assumes the dev stack is up: `make up` (db/redis/jaeger) + `make dev`
 * (backend :8080, frontend :5173, Python agent :9000) with a reachable LLM
 * provider AND the agent started with PYTHON_AGENT_LANGFUSE_ENABLED=true
 * pointed at a running Langfuse instance (`make up-langfuse`, UI :3000).
 * playwright.config.ts only manages the frontend dev server; the rest are
 * preconditions.
 *
 * Env knobs (defaults match the dev bootstrap in the root `.env.example`):
 *   NYX_E2E_LANGFUSE_URL         default http://localhost:3000
 *   NYX_E2E_LANGFUSE_PUBLIC_KEY  default pk-lf-nyx-dev-agent
 *   NYX_E2E_LANGFUSE_SECRET_KEY  default sk-lf-nyx-dev-agent
 * (NYX_E2E_ prefix on purpose: the bare LANGFUSE_* names are the Langfuse
 * SDK's own env vars — an operator who exports real credentials for the
 * agent would otherwise have this spec silently pick them up and
 * Basic-auth them to whatever URL is configured.)
 *
 * Flake posture: the planner is a live model. The prompt instructs the tool
 * explicitly (same posture as chat-agent-planner.spec.ts); if the configured
 * model declines to emit the tool step, the tool-span assertions fail — that
 * is the intent. Trace ingestion is async; the spec polls with a bounded
 * timeout before failing.
 */

const LANGFUSE_URL = process.env.NYX_E2E_LANGFUSE_URL ?? 'http://localhost:3000';
const LANGFUSE_PUBLIC_KEY =
  process.env.NYX_E2E_LANGFUSE_PUBLIC_KEY ?? 'pk-lf-nyx-dev-agent';
const LANGFUSE_SECRET_KEY =
  process.env.NYX_E2E_LANGFUSE_SECRET_KEY ?? 'sk-lf-nyx-dev-agent';

const INGESTION_TIMEOUT_MS = 60_000;
const POLL_INTERVAL_MS = 2_000;

// Fresh user per test — see chat-rag.spec.ts `makeTestUser` for why a
// module-level constant re-registered in beforeEach collides (409).
function makeTestUser() {
  return {
    username: `e2e_langfuse_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const TEST_USER = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Login \/ Register/i }).click();
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(TEST_USER.username);
  await page.getByLabel(/Password/i).fill(TEST_USER.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

/** The verified numeric user id the agent receives via X-Nyx-User-Id: the
 * JWT `sub` claim, read from the SPA's sessionStorage token store (the same
 * value Langfuse should have attributed the trace to). */
async function currentUserId(page: Page): Promise<string> {
  const token = await page.evaluate(() => {
    const raw = window.sessionStorage.getItem('nyx-token-store');
    if (!raw) return null;
    return (JSON.parse(raw) as { accessToken?: string }).accessToken ?? null;
  });
  expect(token, 'no access token in sessionStorage').toBeTruthy();
  const payload = token!.split('.')[1];
  const decoded = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8'));
  return String(decoded.sub);
}

interface LangfuseObservation {
  id: string;
  traceId: string;
  type: string;
  name: string;
  userId: string;
  sessionId: string;
  isRootObservation?: boolean;
  input?: string;
  output?: string;
  usageDetails?: Record<string, number>;
}

async function fetchObservations(
  filter: { userId?: string; traceId?: string },
  fromIso: string,
): Promise<LangfuseObservation[]> {
  const url = new URL(`${LANGFUSE_URL}/api/public/v2/observations`);
  url.searchParams.set('fromStartTime', fromIso);
  url.searchParams.set('toStartTime', new Date(Date.now() + 60_000).toISOString());
  url.searchParams.set('fields', 'basic,io,usage');
  url.searchParams.set('limit', '200');
  if (filter.userId) url.searchParams.set('userId', filter.userId);
  if (filter.traceId) url.searchParams.set('traceId', filter.traceId);
  const auth = Buffer.from(
    `${LANGFUSE_PUBLIC_KEY}:${LANGFUSE_SECRET_KEY}`,
  ).toString('base64');
  const res = await fetch(url, { headers: { Authorization: `Basic ${auth}` } });
  expect(res.ok, `langfuse read API ${res.status} — is the stack up?`).toBeTruthy();
  const body = (await res.json()) as { data: LangfuseObservation[] };
  return body.data ?? [];
}

/** Poll until `ready` is satisfied or the ingestion budget expires; returns
 * the last fetch either way so the caller's assertion reports the real state. */
async function pollUntil(
  fetchPage: () => Promise<LangfuseObservation[]>,
  ready: (observations: LangfuseObservation[]) => boolean,
): Promise<LangfuseObservation[]> {
  const deadline = Date.now() + INGESTION_TIMEOUT_MS;
  let observations: LangfuseObservation[] = [];
  while (Date.now() < deadline) {
    observations = await fetchPage();
    if (ready(observations)) return observations;
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
  return observations;
}

test.describe('Chat / Langfuse tracing (browser-level)', () => {
  // Run + async ingestion poll: well past the default 30s.
  test.setTimeout(180_000);

  test('a chat run lands in Langfuse with generations, usage, and tool spans', async ({
    page,
  }) => {
    const fromIso = new Date(Date.now() - 60_000).toISOString();

    await registerAndOpenChat(page);
    const userId = await currentUserId(page);

    const input = page.getByTestId('chat-input');
    await input.fill(
      'Use the list_feeds tool to list my feeds, then summarize what you found.',
    );
    await page.getByTestId('chat-send').click();

    // Stream terminates cleanly (the cancel button is only mounted
    // mid-stream; its absence after settling is the finish signal — same
    // convention as chat-agent-planner.spec.ts).
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 60_000,
    });
    // No error frame on the happy path.
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // The run completes server-side (SSE done) before ingestion matters:
    // wait for the assistant reply to render first.
    const reply = page.getByTestId('chat-message-assistant').last();
    await expect(reply).not.toHaveText(/^\s*$/, { timeout: 10_000 });

    // Resolve the run: poll by user id until the trace root materializes
    // (user attribution rides the root observation; children carry it only
    // via the trace), then poll the full tree by traceId until the
    // generations are in (ingestion is batched — the root can land first).
    const roots = await pollUntil(
      () => fetchObservations({ userId }, fromIso),
      (obs) => obs.length > 0,
    );
    expect(
      roots.length,
      `no observations for user ${userId} within ${INGESTION_TIMEOUT_MS}ms — ` +
        'is the agent running with PYTHON_AGENT_LANGFUSE_ENABLED=true?',
    ).toBeGreaterThan(0);
    const traceId = roots[0].traceId;

    const observations = await pollUntil(
      () => fetchObservations({ traceId }, fromIso),
      (obs) => obs.filter((o) => o.type === 'GENERATION').length >= 2,
    );
    const flat = observations
      .map((o) => `${o.name}:${o.type}`)
      .join(', ');

    // Identity: the trace carries the caller's user id and a non-empty
    // session (the run_id). The attribution anchor is the observation the
    // user id actually rides on (several spans can carry
    // isRootObservation=true — the agent's own step observations among
    // them — so the flag is not a unique root marker).
    const root = observations.find((o) => o.userId) ?? roots[0];
    expect(root.userId).toBe(userId);
    expect(root.sessionId, 'trace lost its session id').toBeTruthy();
    const sessions = new Set(observations.map((o) => o.sessionId).filter(Boolean));
    expect(sessions.size, `expected exactly one session; got ${[...sessions]}`).toBe(1);

    // Generations: planner + executor model calls with prompts, replies, and
    // provider-reported usage.
    const generations = observations.filter((o) => o.type === 'GENERATION');
    expect(
      generations.length,
      `expected planner + executor generations; got ${flat}`,
    ).toBeGreaterThanOrEqual(2);
    for (const gen of generations) {
      expect(gen.input, `generation ${gen.name} lost its input`).toBeTruthy();
      expect(gen.output, `generation ${gen.name} lost its output`).toBeTruthy();
    }
    const withUsage = generations.filter(
      (g) => (g.usageDetails?.total ?? 0) > 0,
    );
    expect(
      withUsage.length,
      'no generation carried provider-reported token usage',
    ).toBeGreaterThan(0);

    // Tool structure: the run's plan/tool shape is visible in the trace.
    const names = observations.map((o) => o.name);
    expect(names, flat).toContain('agent.step.plan');
    expect(names, flat).toContain('agent.step.tool');
    expect(names, flat).toContain('agent.tool_call');
  });
});
