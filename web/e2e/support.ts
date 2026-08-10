import { expect, test as base, type APIRequestContext, type Page } from '@playwright/test';

export const GATEWAY = process.env.GATEWAY_URL ?? 'http://localhost:8083';
export const TASKING = process.env.TASKING_API_URL ?? 'http://localhost:8080';

/**
 * `test`, extended to fail when the page could not reach our own APIs.
 *
 * THE GUARD THIS SUITE EXISTS FOR. The first defect these tests found was CORS:
 * the gateway sent no Access-Control-Allow-Origin, so the browser fetched every
 * read, received it, and then refused to hand it to the page. Health checks were
 * green, curl worked, every unit test passed, and the UI rendered nothing. The
 * only evidence was in a console nobody reads.
 *
 * Asserting on console text would mean maintaining an allow-list of benign
 * messages, which rots. A failed request to gateway or tasking-api has no benign
 * case: it is a blocked origin, a wrong port, or a service that is down. So that
 * is what is asserted, and it is asserted for every test automatically — an
 * opt-in guard is one the next spec forgets.
 */
export const test = base.extend({
  page: async ({ page }: { page: Page }, use: (page: Page) => Promise<void>) => {
    const failures: string[] = [];
    page.on('requestfailed', (request) => {
      const url = request.url();
      if (!url.startsWith(GATEWAY) && !url.startsWith(TASKING)) return;
      // An aborted request is the page navigating or the test ending while a
      // stream is open — /v1/events is deliberately long-lived, and tearing it
      // down is not a failure.
      const error = request.failure()?.errorText ?? 'unknown';
      if (error.includes('ERR_ABORTED')) return;
      failures.push(`${request.method()} ${url} — ${error}`);
    });

    await use(page);

    expect(
      failures,
      'the page could not reach its own APIs; a blocked or failed request renders as an empty UI, not an error',
    ).toEqual([]);
  },
});

export { expect };

/** A window wide enough that the constellation has passes in it. */
export function window24h(): { start: string; end: string } {
  const start = new Date(Date.now() + 60 * 60_000);
  const end = new Date(start.getTime() + 24 * 3600_000);
  return { start: start.toISOString(), end: end.toISOString() };
}

export interface SubmitOptions {
  customerId: string;
  targetName: string;
  bidCredits: number;
  tier?: string;
  /** Narrow windows force competition; wide ones let everybody win. */
  window?: { start: string; end: string };
  coordinates?: [number, number];
}

/**
 * Submit through the real ingress.
 *
 * Through the API rather than the form, deliberately. The form is exercised by
 * the happy path; the contested path needs two requests placed within a
 * predictable window of each other, and driving that through a UI would be
 * testing typing speed.
 */
export async function submit(
  request: APIRequestContext,
  options: SubmitOptions,
): Promise<string> {
  const window = options.window ?? window24h();
  const response = await request.post(`${TASKING}/v1/tasking-requests`, {
    headers: {
      'Content-Type': 'application/json',
      // Unique per call: a reused key exercises the idempotency REPLAY path,
      // which returns the stored response without touching the write path.
      'Idempotency-Key': `e2e-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`,
    },
    data: {
      customer_id: options.customerId,
      target_name: options.targetName,
      target: { type: 'Point', coordinates: options.coordinates ?? [4.4, 51.9] },
      window,
      priority_tier: options.tier ?? 'BEST_EFFORT',
      bid_credits: options.bidCredits,
      requested_modes: ['SCAN'],
    },
  });
  expect(response.status(), await response.text()).toBe(202);
  const body = (await response.json()) as { request_id: string };
  return body.request_id;
}

/**
 * Wait for a request to reach one of the given states.
 *
 * POLLS A REAL CONDITION rather than sleeping. `expect.poll` retries until the
 * predicate holds or the budget runs out, so a fast machine is fast and a slow
 * one is still correct — which a fixed sleep cannot manage in both directions
 * at once.
 */
export async function waitForState(
  request: APIRequestContext,
  requestId: string,
  states: string[],
  timeout = 150_000,
): Promise<Record<string, unknown>> {
  let last: Record<string, unknown> = {};
  // `toBe(true)` on a predicate rather than a matcher over the state, because
  // Playwright's poll matchers do not include toSatisfy. The message carries
  // the detail a bare `false` would lose.
  await expect
    .poll(
      async () => {
        const response = await request.get(`${GATEWAY}/v1/requests/${requestId}`);
        if (!response.ok()) return false;
        last = (await response.json()) as Record<string, unknown>;
        return states.includes(String(last.state));
      },
      {
        timeout,
        message: `request ${requestId} never reached ${states.join(' or ')}`,
      },
    )
    .toBe(true);
  return last;
}

/**
 * Wait until the planner has DECIDED about a request.
 *
 * "Reached a state" is the wrong question for the contested path, and asking it
 * cost a false failure. A request that loses stays in AWAITING_PLANNING and
 * gains an `unfulfilment` — so a wait that accepts AWAITING_PLANNING returns
 * the instant the request is stored, long before anything has competed, and
 * reports "nothing was refused" about a round that had not yet run. A wait that
 * excludes it never returns for the losers at all.
 *
 * The real condition is the one below: a terminal state, or an explanation.
 * Either means the planner has looked at this request and made a decision about
 * it; neither is reachable before it has.
 */
export async function waitForDecision(
  request: APIRequestContext,
  requestId: string,
  timeout = 150_000,
): Promise<Record<string, unknown>> {
  let last: Record<string, unknown> = {};
  await expect
    .poll(
      async () => {
        const response = await request.get(`${GATEWAY}/v1/requests/${requestId}`);
        if (!response.ok()) return false;
        last = (await response.json()) as Record<string, unknown>;
        const decided = ['PLANNED', 'ACQUIRED', 'INFEASIBLE', 'REJECTED'];
        return (
          decided.includes(String(last.state)) ||
          (last.unfulfilment !== undefined && last.unfulfilment !== null)
        );
      },
      {
        timeout,
        message: `request ${requestId} was never planned, refused or explained`,
      },
    )
    .toBe(true);
  return last;
}

/** Reload and wait for the workspace to have painted its data. */
export async function openWorkspace(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Acquisitions' })).toBeVisible();
}
