import { describe, expect, it } from 'vitest';

import type { Acquisition } from '@/lib/gateway';
import {
  MAX_SPAN_MS,
  MIN_SPAN_MS,
  blockRect,
  buildRows,
  pan,
  ticks,
  visibleBlocks,
  zoom,
  type Viewport,
} from '@/lib/timeline';

const T0 = Date.parse('2026-08-10T12:00:00Z');

function acquisition(overrides: Partial<Acquisition> & { start: string; end: string }): Acquisition {
  const { start, end, ...rest } = overrides;
  return {
    acquisitionId: 'a1',
    requestId: 'r1',
    customerId: 'acme',
    satelliteId: 'SENTINEL-1A',
    mode: 'SCAN',
    window: { start, end },
    status: 'ACTIVE',
    footprint: null,
    awardedValueCredits: 100,
    ...rest,
  };
}

describe('buildRows', () => {
  it('renders slew as its own block, ending where the acquisition begins', () => {
    // THE POINT OF THE WHOLE VIEW. Slew is occupancy, not idle time, and it is
    // a property of the ARRIVAL — how long it took to point at this target —
    // so the block ends where the acquisition starts. Drawing it forwards from
    // the previous acquisition's end would only land correctly when the gap
    // happens to equal the slew.
    const rows = buildRows([
      acquisition({ start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
      acquisition({
        acquisitionId: 'a2',
        requestId: 'r2',
        start: '2026-08-10T12:02:00Z',
        end: '2026-08-10T12:02:10Z',
        slewFromPreviousS: 30,
      }),
    ]);

    expect(rows).toHaveLength(1);
    const slew = rows[0]!.blocks.filter((b) => b.kind === 'slew');
    expect(slew).toHaveLength(1);
    expect(slew[0]!.endMs).toBe(Date.parse('2026-08-10T12:02:00Z'));
    expect(slew[0]!.startMs).toBe(Date.parse('2026-08-10T12:01:30Z'));
  });

  it('clamps a slew that will not fit rather than overlapping its predecessor', () => {
    // The planner refuses this case — BLOCKED_BY_SLEW_CONSTRAINT exists for it
    // — so drawing an overlap would render an impossible schedule as though it
    // had been committed.
    const rows = buildRows([
      acquisition({ start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
      acquisition({
        acquisitionId: 'a2',
        start: '2026-08-10T12:00:20Z',
        end: '2026-08-10T12:00:30Z',
        slewFromPreviousS: 600,
      }),
    ]);

    const slew = rows[0]!.blocks.find((b) => b.kind === 'slew')!;
    expect(slew.startMs).toBeGreaterThanOrEqual(Date.parse('2026-08-10T12:00:10Z'));
  });

  it('emits no slew block for the first acquisition, which has nothing to turn from', () => {
    const rows = buildRows([
      acquisition({ start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
    ]);
    expect(rows[0]!.blocks.every((b) => b.kind === 'acquisition')).toBe(true);
  });

  it('counts slew separately from imaging, so duty cycle is not flattered', () => {
    const rows = buildRows([
      acquisition({ start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
      acquisition({
        acquisitionId: 'a2',
        start: '2026-08-10T12:01:00Z',
        end: '2026-08-10T12:01:20Z',
        slewFromPreviousS: 15,
      }),
    ]);
    expect(rows[0]!.imagingSeconds).toBe(30);
    expect(rows[0]!.slewSeconds).toBe(15);
  });

  it('keeps rows in a stable order across refreshes', () => {
    // A timeline whose rows reorder because a satellite gained an acquisition
    // is one nobody can point at.
    const first = buildRows([
      acquisition({ satelliteId: 'ICEYE-X2', start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
      acquisition({ satelliteId: 'CAPELLA-11', start: '2026-08-10T12:05:00Z', end: '2026-08-10T12:05:10Z' }),
    ]);
    const second = buildRows([
      acquisition({ satelliteId: 'CAPELLA-11', start: '2026-08-10T12:05:00Z', end: '2026-08-10T12:05:10Z' }),
      acquisition({ satelliteId: 'ICEYE-X2', start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
      acquisition({ satelliteId: 'ICEYE-X2', start: '2026-08-10T13:00:00Z', end: '2026-08-10T13:00:10Z' }),
    ]);
    expect(first.map((r) => r.satelliteId)).toEqual(second.map((r) => r.satelliteId));
  });

  it('orders blocks by time even when the input is not', () => {
    const rows = buildRows([
      acquisition({ acquisitionId: 'late', start: '2026-08-10T13:00:00Z', end: '2026-08-10T13:00:10Z' }),
      acquisition({ acquisitionId: 'early', start: '2026-08-10T12:00:00Z', end: '2026-08-10T12:00:10Z' }),
    ]);
    const ids = rows[0]!.blocks.map((b) => b.acquisitionId);
    expect(ids).toEqual(['early', 'late']);
  });
});

describe('virtualisation', () => {
  const view: Viewport = { startMs: T0, endMs: T0 + 3600_000, widthPx: 600 };

  it('drops blocks outside the viewport', () => {
    const row = buildRows([
      acquisition({ start: '2026-08-10T09:00:00Z', end: '2026-08-10T09:00:10Z' }),
      acquisition({ acquisitionId: 'a2', start: '2026-08-10T12:30:00Z', end: '2026-08-10T12:30:10Z' }),
    ])[0]!;

    expect(visibleBlocks(row, view).map((b) => b.acquisitionId)).toEqual(['a2']);
  });

  it('draws a sub-pixel block at one pixel rather than dropping it', () => {
    // A four-second acquisition on a two-day axis is sub-pixel. Dropping it
    // would show an idle satellite that is in fact working, which is the
    // opposite of what this view is for.
    const wide: Viewport = { startMs: T0, endMs: T0 + 2 * 86400_000, widthPx: 800 };
    const rect = blockRect(
      { kind: 'acquisition', startMs: T0 + 1000, endMs: T0 + 5000 },
      wide,
    );
    expect(rect).not.toBeNull();
    expect(rect!.width).toBeGreaterThanOrEqual(1);
  });
});

describe('zoom and pan', () => {
  const view: Viewport = { startMs: T0, endMs: T0 + 3600_000, widthPx: 600 };

  it('holds the anchor point still', () => {
    // Zooming toward the centre moves whatever you were looking at, which
    // makes a timeline feel like it is fighting you.
    const anchorPx = 150;
    const before = view.startMs + ((view.endMs - view.startMs) * anchorPx) / view.widthPx;
    const after = zoom(view, 0.5, anchorPx);
    const anchorAfter = after.startMs + ((after.endMs - after.startMs) * anchorPx) / after.widthPx;
    expect(anchorAfter).toBeCloseTo(before, -1);
  });

  it('clamps the span at both ends', () => {
    let zoomedIn = view;
    for (let i = 0; i < 40; i += 1) zoomedIn = zoom(zoomedIn, 0.5, 300);
    expect(zoomedIn.endMs - zoomedIn.startMs).toBeGreaterThanOrEqual(MIN_SPAN_MS);

    let zoomedOut = view;
    for (let i = 0; i < 40; i += 1) zoomedOut = zoom(zoomedOut, 2, 300);
    expect(zoomedOut.endMs - zoomedOut.startMs).toBeLessThanOrEqual(MAX_SPAN_MS);
  });

  it('pans without changing the span', () => {
    const panned = pan(view, 100);
    expect(panned.endMs - panned.startMs).toBe(view.endMs - view.startMs);
    expect(panned.startMs).toBeLessThan(view.startMs);
  });
});

describe('ticks', () => {
  it('lands on human intervals rather than dividing the span', () => {
    // A "nice" number derived by division lands on 37-minute intervals, and an
    // axis whose labels nobody can read is decoration.
    const marks = ticks({ startMs: T0, endMs: T0 + 6 * 3600_000, widthPx: 900 });
    expect(marks.length).toBeGreaterThan(1);
    const step = marks[1]! - marks[0]!;
    expect([5, 15, 30, 60, 180, 360, 720, 1440].map((m) => m * 60_000)).toContain(step);
  });
});
