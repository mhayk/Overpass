import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import RejectionPanel from '@/components/RejectionPanel';
import * as gateway from '@/lib/gateway';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function withRequest(unfulfilment?: gateway.Unfulfilment): void {
  vi.spyOn(gateway, 'fetchRequest').mockResolvedValue({
    requestId: 'r1',
    customerId: 'acme-imaging',
    targetName: 'Port of Rotterdam',
    state: unfulfilment ? 'AWAITING_PLANNING' : 'PLANNED',
    window: { start: '2026-08-10T00:00:00Z', end: '2026-08-11T00:00:00Z' },
    opportunityCount: 3,
    unfulfilment,
    staleness: { asOf: '2026-08-10T00:00:00Z', lagSeconds: 1 },
  });
}

describe('RejectionPanel', () => {
  it('shows the shortfall as a number the customer can act on', async () => {
    // "The single most useful number a losing customer can receive, and only
    // expressible because the reason is structured data rather than prose."
    withRequest({ reasonCode: 'LOST_TO_HIGHER_VALUE', shortfallCredits: 2500 });
    render(<RejectionPanel requestId="r1" />);

    // getAllBy, not getBy: the number appears TWICE and that is correct —
    // once stating the gap, once as the amount to bid. A single-match
    // assertion would fail on the panel doing the right thing.
    await waitFor(() => expect(screen.getAllByText(/2,500/).length).toBeGreaterThan(0));
  });

  it('shows required slew against available gap, which is the explanation', async () => {
    withRequest({
      reasonCode: 'BLOCKED_BY_SLEW_CONSTRAINT',
      requiredSlewS: 42.8,
      availableGapS: 19.5,
    });
    render(<RejectionPanel requestId="r1" />);

    await waitFor(() => {
      const text = document.body.textContent ?? '';
      expect(text).toContain('42.8');
      expect(text).toContain('19.5');
    });
  });

  it('never tells a customer to bid more when the constraint is physics', async () => {
    // The issue names this case: telling a customer to raise their bid when
    // the real problem is that no satellite can turn in time is worse than
    // saying nothing — it sends them to spend on a constraint money does not
    // touch.
    withRequest({
      reasonCode: 'BLOCKED_BY_SLEW_CONSTRAINT',
      requiredSlewS: 42.8,
      availableGapS: 19.5,
    });
    render(<RejectionPanel requestId="r1" />);

    await waitFor(() => expect(document.body.textContent).toContain('42.8'));
    expect(document.body.textContent?.toLowerCase()).not.toMatch(/bid/);
  });

  it('says plainly when there is nothing to change', async () => {
    // Silence would read as a missing feature. Saying "nothing to change"
    // is the honest answer for a request still competing.
    withRequest({ reasonCode: 'NO_OPPORTUNITY_IN_BUCKET' });
    render(<RejectionPanel requestId="r1" />);

    await waitFor(() => expect(screen.getByText(/Nothing to change/i)).toBeTruthy());
  });

  it('does not invent an explanation for a request that was not refused', async () => {
    withRequest(undefined);
    render(<RejectionPanel requestId="r1" />);

    await waitFor(() => expect(screen.getByText(/still competing|Scheduled/i)).toBeTruthy());
    expect(document.body.textContent?.toLowerCase()).not.toMatch(/outbid|slew/);
  });

  it('renders nothing without a selection', () => {
    const { container } = render(<RejectionPanel />);
    expect(container.firstChild).toBeNull();
  });

  it('reports a failed read rather than showing an empty panel', async () => {
    // "There is no explanation" and "I could not fetch the explanation" are
    // different answers, and the read model is allowed to be unreachable.
    vi.spyOn(gateway, 'fetchRequest').mockRejectedValue(new gateway.GatewayError(503, 'gateway down'));
    render(<RejectionPanel requestId="r1" />);

    await waitFor(() => expect(screen.getByRole('alert')).toBeTruthy());
  });
});
