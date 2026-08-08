import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  GatewayError,
  fetchAcquisitions,
  fetchFootprints,
  fetchOpportunities,
} from '@/lib/gateway';

const WINDOW = { start: '2026-03-01T00:00:00Z', end: '2026-03-02T00:00:00Z' };

function respondWith(body: unknown, status = 200): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        }),
    ),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe('staleness', () => {
  it('is carried through rather than discarded', async () => {
    respondWith({ items: [], staleness: { as_of: '2026-03-01T12:00:00Z', lag_seconds: 30 } });

    const result = await fetchAcquisitions(WINDOW);
    expect(result.staleness).toEqual({ asOf: '2026-03-01T12:00:00Z', lagSeconds: 30 });
  });

  it('reports an absent lag as unknown, not as zero', async () => {
    // Zero means "perfectly current", which is a far stronger claim than "the
    // server did not say" — and it is the claim a reader would act on.
    respondWith({ items: [] });

    const result = await fetchAcquisitions(WINDOW);
    expect(Number.isNaN(result.staleness.lagSeconds)).toBe(true);
  });
});

describe('an unreachable read model', () => {
  it('is distinguished from an empty result', async () => {
    respondWith({ title: 'Read model unavailable', detail: 'not reachable' }, 503);

    await expect(fetchAcquisitions(WINDOW)).rejects.toBeInstanceOf(GatewayError);
    await expect(fetchAcquisitions(WINDOW)).rejects.toMatchObject({
      status: 503,
      transient: true,
    });
  });

  it('reports a 404 as permanent, so the UI shows an empty state and not a retry', async () => {
    respondWith({ title: 'Not found' }, 404);

    await expect(fetchOpportunities('req-1')).rejects.toMatchObject({ transient: false });
  });

  it('turns a network failure into the same shape as a 5xx', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('ECONNREFUSED');
      }),
    );

    await expect(fetchAcquisitions(WINDOW)).rejects.toMatchObject({ transient: true });
  });
});

describe('collections', () => {
  it('survive a response with no items array', async () => {
    // The gateway emits [] rather than null and has a test for it. A client
    // that throws on one missing key takes the whole page down with it.
    respondWith({ staleness: { as_of: 'x', lag_seconds: 1 } });

    await expect(fetchAcquisitions(WINDOW)).resolves.toMatchObject({ items: [] });
  });

  it('map snake_case onto the shape the UI uses', async () => {
    respondWith({
      items: [
        {
          acquisition_id: 'acq-1',
          request_id: 'req-1',
          customer_id: 'acme',
          satellite_id: 'SAT-1',
          mode: 'STRIPMAP',
          window: WINDOW,
          status: 'ACTIVE',
          footprint: { type: 'Polygon', coordinates: [] },
          awarded_value_credits: 500,
        },
      ],
    });

    const result = await fetchAcquisitions(WINDOW);
    expect(result.items[0]).toMatchObject({
      acquisitionId: 'acq-1',
      awardedValueCredits: 500,
      status: 'ACTIVE',
    });
  });

  it('keep losing candidates, because they are the explanation', async () => {
    respondWith({
      items: [
        {
          opportunity_id: 'o1',
          satellite_id: 'S',
          mode: 'SCAN',
          access_window: WINDOW,
          quality_score: 0.9,
          footprint: {},
          won: true,
        },
        {
          opportunity_id: 'o2',
          satellite_id: 'S',
          mode: 'SCAN',
          access_window: WINDOW,
          quality_score: 0.4,
          footprint: {},
          won: false,
        },
      ],
    });

    const result = await fetchOpportunities('req-1');
    expect(result.items).toHaveLength(2);
    expect(result.items.filter((candidate) => !candidate.won)).toHaveLength(1);
  });
});

describe('footprint truncation', () => {
  it('defaults to truncated when the server does not say', async () => {
    // The safe direction. A viewport that wrongly believes it has everything
    // draws a coverage gap where there is really a limit, and a reader cannot
    // tell the difference.
    respondWith({ features: [] });

    await expect(fetchFootprints(WINDOW)).resolves.toMatchObject({ truncated: true });
  });

  it('reports it faithfully when the server does', async () => {
    respondWith({ features: [], truncated: false });

    await expect(fetchFootprints(WINDOW)).resolves.toMatchObject({ truncated: false });
  });
});
