import { expect, openWorkspace, submit, test, waitForDecision } from './support';

/**
 * Two requests compete, one wins, and the loser is told why.
 *
 * THE TEST THAT MATTERS. It is the only one that exercises the actual point of
 * the system: de-confliction, and an explanation a customer can act on. A
 * happy path proves the pipes connect; this proves the thing they were built
 * to carry.
 */
test('a losing request is told which constraint bound', async ({ page, request }) => {
  // ONE narrow window over ONE point, so the two cannot both be satisfied on
  // different passes. A wide window is what makes the demo's four requests all
  // win, and a contested test that quietly stops contending would pass forever
  // while proving nothing.
  const start = new Date(Date.now() + 2 * 3600_000);
  const contested = {
    start: start.toISOString(),
    end: new Date(start.getTime() + 3 * 3600_000).toISOString(),
  };

  const stamp = Date.now();
  const ids: string[] = [];

  // FOUR, measured rather than guessed. This was twelve on the reasoning that
  // a nine-satellite constellation needs a lot of demand before anything
  // loses. That reasoning ignored the window: feasibility finds ONE
  // opportunity per request in a three-hour window over a single point, so
  // every request here wants the same slot and all but one must lose. Four
  // submissions were measured producing four refusals.
  //
  // The count is not free. Feasibility is a single worker at roughly 0.25 rps
  // (#189), so each extra request is real wall-clock in a budget that has to
  // hold on a shared CI runner — twelve of them timed out at 150s having
  // proved nothing. Four contends just as hard for a third of the time.
  for (let i = 0; i < 4; i++) {
    ids.push(
      await submit(request, {
        customerId: i % 2 === 0 ? 'acme-imaging' : 'port-authority-nl',
        targetName: `e2e contested ${stamp}-${i}`,
        bidCredits: 100 + i * 400,
        tier: i % 2 === 0 ? 'BEST_EFFORT' : 'COMMERCIAL',
        window: contested,
      }),
    );
  }

  // Every one of them must reach an answer. Conservation is a contract
  // property — a request that competed gets an outcome — and a request stuck
  // with no decision would be the pipeline losing it.
  //
  // waitForDecision, NOT a state list. A loser stays in AWAITING_PLANNING and
  // gains an explanation, so accepting that state as an answer returns before
  // the planner has run and then reports that nothing was refused. That is
  // exactly how this test first failed.
  const outcomes = await Promise.all(ids.map((id) => waitForDecision(request, id)));

  // Someone has to lose, or there was no contention and this test asserted
  // nothing. A request still AWAITING_PLANNING after the planner has run is a
  // request that competed and did not win.
  const refused = outcomes.filter((view) => view.unfulfilment !== undefined && view.unfulfilment !== null);
  expect(
    refused.length,
    'nothing was refused; the window was not narrow enough to force contention',
  ).toBeGreaterThan(0);

  const explanation = refused[0]!.unfulfilment as { reason_code: string; explanation?: Record<string, number> };

  // The reason must be one the contract defines and the UI can explain. A code
  // the panel does not handle is a customer question nobody can answer.
  expect([
    'LOST_TO_HIGHER_VALUE',
    'BLOCKED_BY_SLEW_CONSTRAINT',
    'DUTY_CYCLE_EXHAUSTED',
    'NO_OPPORTUNITY_IN_BUCKET',
    'SUPERSEDED',
  ]).toContain(explanation.reason_code);

  // And it must carry NUMBERS, not just a code. The structured explanation is
  // the whole product claim — "you were outbid by this much", "the slew needed
  // this long and this was free" — and a reason with no figures is the
  // announcement this system exists not to make.
  if (explanation.reason_code === 'BLOCKED_BY_SLEW_CONSTRAINT') {
    expect(explanation.explanation?.required_slew_s).toBeGreaterThan(0);
    expect(explanation.explanation?.available_gap_s).toBeGreaterThanOrEqual(0);
  }
  if (explanation.reason_code === 'LOST_TO_HIGHER_VALUE') {
    expect(explanation.explanation?.shortfall_credits).toBeGreaterThan(0);
  }

  // Now the panel. Selecting the losing request must show the explanation the
  // planner produced — not a recomputation, which is the property that makes
  // it trustworthy.
  const losingId = String(refused[0]!.request_id);
  await openWorkspace(page);

  const entry = page.getByTestId('acquisition').filter({ hasText: losingId.slice(0, 8) });
  if ((await entry.count()) > 0) {
    await entry.first().click();
    await expect(page.getByTestId('rejection-panel')).toBeVisible();
    await expect(page.getByTestId('rejection-summary')).not.toBeEmpty();
  } else {
    // The list shows requests that WON acquisitions, so a total loser may not
    // appear there. Stated rather than skipped silently: the API assertions
    // above already prove the explanation exists and carries its numbers.
    test.info().annotations.push({
      type: 'note',
      description: 'the losing request has no acquisition row to click; API assertions stand',
    });
  }
});
