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
 *
 * The 150s budget is not arbitrary and not a sleep — it is a ceiling derived
 * from the pipeline's measured throughput. Feasibility is a single worker at
 * roughly 0.25 rps (#189), so a dozen requests need about a minute of sweeping
 * before the planner's quiet period even starts. This much headroom means a
 * slow machine still passes; what it will not survive is a queue that was
 * already full when the test began, which is why CI seeds and then runs this
 * BEFORE driving the demo.
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

/**
 * The access window of a request's first opportunity.
 *
 * THE CONTESTED PATH MUST NOT INVENT A WINDOW. It used to compete inside an
 * arbitrary "two hours from now, for three hours", which assumes a satellite
 * passes over Rotterdam in that span. On a cold stack it did not: feasibility
 * answered all four with NO_ACCESS_IN_HORIZON, correctly, and the test read
 * that as a pipeline stall.
 *
 * Asking the system when it can actually see the target turns the window from
 * an assumption into an observation, and pins the competitors to one pass —
 * which is what makes them compete at all.
 */
export async function firstOpportunityWindow(
  request: APIRequestContext,
  requestId: string,
  timeout = 120_000,
): Promise<{ start: string; end: string } | undefined> {
  let window: { start: string; end: string } | undefined;
  let lastStatus = 0;

  // POLLED, BECAUSE A STATE IS NOT A PROMISE THAT THE ROWS ARE THERE.
  //
  // This used to read once, after waitForState reported AWAITING_PLANNING or
  // PLANNED. Both of those are derived from rows the gateway has folded — but
  // FEASIBILITY and PLANNING are separate streams and the broker gives no
  // ordering between them, which the projector says out loud. So a plan can be
  // folded before the opportunities that produced it: the request reads
  // PLANNED while opportunity_views is still empty, and a single read comes
  // back with nothing. It passed locally for exactly as long as the gateway
  // stayed ahead of the test, and failed the first time CI was slow enough for
  // feasibility to take seven seconds.
  await expect
    .poll(
      async () => {
        const response = await request.get(`${GATEWAY}/v1/requests/${requestId}/opportunities`);
        lastStatus = response.status();
        if (!response.ok()) {
          // Reported, not swallowed. Returning undefined here made a 503 look
          // identical to an unseeded constellation, and the failure message
          // then blamed the wrong thing.
          return false;
        }
        const body = (await response.json()) as {
          items?: { access_window?: { start: string; end: string } }[];
        };
        window = body.items?.[0]?.access_window;
        return window !== undefined;
      },
      {
        timeout,
        message:
          `no opportunity for ${requestId} became readable ` +
          `(last status ${lastStatus}); the constellation is unseeded, ` +
          'the horizon is empty, or the gateway never folded them',
      },
    )
    .toBe(true);

  return window;
}

/** Reload and wait for the workspace to have painted its data. */
export async function openWorkspace(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Acquisitions' })).toBeVisible();
}
