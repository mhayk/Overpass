import { expect, GATEWAY, openWorkspace, submit, test, waitForState } from './support';

/**
 * Submit → opportunities → plan → acquisition on screen.
 *
 * The path that crosses every boundary in the system: HTTP ingress, the
 * transactional outbox, JetStream, SGP4 in Python, the planner's round, the
 * projector, and the browser. Nothing below this layer can prove all of those
 * agree with each other.
 */
test('a submitted request becomes an acquisition on the page', async ({ page, request }) => {
  const requestId = await submit(request, {
    customerId: 'acme-imaging',
    targetName: `e2e happy ${Date.now()}`,
    bidCredits: 5000,
    tier: 'GOVERNMENT',
  });

  // Feasibility answers first. AWAITING_PLANNING means opportunities exist;
  // INFEASIBLE is also an ANSWER — the pipeline completed and said no — so it
  // is accepted here and distinguished below rather than timing out.
  const afterFeasibility = await waitForState(request, requestId, [
    'AWAITING_PLANNING',
    'PLANNED',
    'INFEASIBLE',
  ]);

  expect(
    afterFeasibility.state,
    'feasibility found no pass at all; the constellation is probably unseeded',
  ).not.toBe('INFEASIBLE');

  // Then the planner. This is the slow half — a round waits out its quiet
  // period — and it is why the budget here is minutes rather than seconds.
  await waitForState(request, requestId, ['PLANNED', 'ACQUIRED']);

  // And the acquisition is readable from the geo endpoint the map draws from,
  // which is a stronger claim than "the request says PLANNED": it means the
  // footprint projected too.
  await expect
    .poll(
      async () => {
        const response = await request.get(
          `${GATEWAY}/v1/acquisitions?window_start=${encodeURIComponent(
            new Date(Date.now() - 3600_000).toISOString(),
          )}&window_end=${encodeURIComponent(new Date(Date.now() + 48 * 3600_000).toISOString())}`,
        );
        if (!response.ok()) return 0;
        const body = (await response.json()) as { items: { request_id: string }[] };
        return body.items.filter((item) => item.request_id === requestId).length;
      },
      { message: 'the acquisition never reached the read model' },
    )
    .toBeGreaterThan(0);

  // Finally the browser. The page is opened AFTER the data exists, so this
  // asserts rendering rather than racing the pipeline through the UI.
  await openWorkspace(page);
  await expect(page.getByTestId('acquisition').first()).toBeVisible();
});
