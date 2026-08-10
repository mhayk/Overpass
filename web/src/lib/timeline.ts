/**
 * Turning acquisitions into timeline blocks.
 *
 * Pure, and separate from the component, because THE INTERESTING PART IS
 * ARITHMETIC: which pixels a time maps to, where a slew block starts, and
 * which rows are worth mounting. A component test would have to render a DOM
 * to assert a number.
 */

import type { Acquisition } from '@/lib/gateway';

/**
 * What a block represents.
 *
 * `slew` is not idle, which is the point of the view. `ghost` is a candidate
 * that LOST — rendering only winners shows the result of de-confliction;
 * rendering the losers shows the decision.
 */
export type BlockKind = 'acquisition' | 'slew' | 'ghost';

export interface Block {
  kind: BlockKind;
  /** Present on ghosts: the candidate that lost. */
  opportunityId?: string;
  /** Present on acquisition blocks; absent on the slew that precedes one. */
  acquisitionId?: string;
  requestId?: string;
  mode?: string;
  startMs: number;
  endMs: number;
}

export interface SatelliteRow {
  satelliteId: string;
  blocks: Block[];
  /** Seconds of imaging in this window — the duty-cycle numerator. */
  imagingSeconds: number;
  /** Seconds spent rotating rather than imaging. */
  slewSeconds: number;
}

/** A losing candidate, as the timeline needs it. */
export interface Ghost {
  opportunityId: string;
  requestId: string;
  satelliteId: string;
  mode: string;
  startMs: number;
  endMs: number;
}

/**
 * Add ghost blocks to rows that already hold the winners.
 *
 * A SEPARATE PASS, not a parameter to buildRows, and the reason is the slew
 * arithmetic: slew is derived from consecutive COMMITTED acquisitions, and
 * feeding losing candidates into that sequence would invent manoeuvres between
 * passes the satellite never made. The winners define the schedule; the ghosts
 * are drawn beside it.
 *
 * A ghost on a satellite with no acquisitions still gets a row. "This
 * satellite was considered and won nothing" is a real answer, and dropping the
 * row would make the constellation look smaller than it is.
 */
export function withGhosts(rows: SatelliteRow[], ghosts: Ghost[]): SatelliteRow[] {
  if (ghosts.length === 0) return rows;

  const byId = new Map(rows.map((row) => [row.satelliteId, { ...row, blocks: [...row.blocks] }]));

  for (const ghost of ghosts) {
    const row = byId.get(ghost.satelliteId) ?? {
      satelliteId: ghost.satelliteId,
      blocks: [],
      imagingSeconds: 0,
      slewSeconds: 0,
    };
    row.blocks.push({
      kind: 'ghost',
      opportunityId: ghost.opportunityId,
      requestId: ghost.requestId,
      mode: ghost.mode,
      startMs: ghost.startMs,
      endMs: ghost.endMs,
    });
    byId.set(ghost.satelliteId, row);
  }

  // Ghosts do NOT contribute to imagingSeconds. Duty cycle is what the
  // satellite actually spent, and counting candidates it declined would report
  // a satellite as busier than it was — which is the number an operator would
  // use to decide it had no capacity left.
  return [...byId.values()].sort((a, b) => a.satelliteId.localeCompare(b.satelliteId));
}

/**
 * Group acquisitions into rows and derive the slew blocks between them.
 *
 * SLEW IS DRAWN BACKWARDS FROM THE ACQUISITION IT PRECEDES, not forwards from
 * the one before. `slew_time_from_previous_s` is a property of the arrival —
 * how long it took to point AT this target — so the block ends where the
 * acquisition begins. Drawing it forwards from the previous acquisition's end
 * would place it correctly only when the gap happens to equal the slew, and
 * would silently misplace it whenever the satellite waited.
 *
 * A slew longer than the gap available is CLAMPED to the gap rather than drawn
 * overlapping its predecessor. That case should not occur — the planner refuses
 * it, and BLOCKED_BY_SLEW_CONSTRAINT exists precisely to reject it — so drawing
 * an overlap would render an impossible schedule as though it were real. The
 * clamp keeps the picture honest about what was committed; the planner's
 * refusals are the place that case is visible.
 */
export function buildRows(acquisitions: Acquisition[]): SatelliteRow[] {
  const bySatellite = new Map<string, Acquisition[]>();
  for (const acquisition of acquisitions) {
    const list = bySatellite.get(acquisition.satelliteId) ?? [];
    list.push(acquisition);
    bySatellite.set(acquisition.satelliteId, list);
  }

  const rows: SatelliteRow[] = [];
  for (const [satelliteId, list] of bySatellite) {
    const ordered = [...list].sort(
      (a, b) => Date.parse(a.window.start) - Date.parse(b.window.start),
    );

    const blocks: Block[] = [];
    let imagingSeconds = 0;
    let slewSeconds = 0;
    let previousEnd: number | null = null;

    for (const acquisition of ordered) {
      const startMs = Date.parse(acquisition.window.start);
      const endMs = Date.parse(acquisition.window.end);
      if (!Number.isFinite(startMs) || !Number.isFinite(endMs)) continue;

      const slew = acquisition.slewFromPreviousS ?? 0;
      if (slew > 0) {
        const wanted = slew * 1000;
        const available = previousEnd === null ? wanted : Math.max(0, startMs - previousEnd);
        const drawn = Math.min(wanted, available);
        if (drawn > 0) {
          blocks.push({ kind: 'slew', startMs: startMs - drawn, endMs: startMs });
          slewSeconds += drawn / 1000;
        }
      }

      blocks.push({
        kind: 'acquisition',
        acquisitionId: acquisition.acquisitionId,
        requestId: acquisition.requestId,
        mode: acquisition.mode,
        startMs,
        endMs,
      });
      imagingSeconds += Math.max(0, (endMs - startMs) / 1000);
      previousEnd = endMs;
    }

    rows.push({ satelliteId, blocks, imagingSeconds, slewSeconds });
  }

  // Alphabetical, so a satellite does not jump rows between refreshes because
  // its acquisition count changed. A timeline whose rows reorder under the
  // cursor is one nobody can point at.
  return rows.sort((a, b) => a.satelliteId.localeCompare(b.satelliteId));
}

export interface Viewport {
  startMs: number;
  endMs: number;
  widthPx: number;
}

/** Time to x, in pixels. */
export function toX(timeMs: number, view: Viewport): number {
  const span = view.endMs - view.startMs;
  if (span <= 0) return 0;
  return ((timeMs - view.startMs) / span) * view.widthPx;
}

/**
 * A block's on-screen rectangle, or null when it is off-screen.
 *
 * VIRTUALISATION LIVES HERE, and it is the reason this is a function rather
 * than a style prop. M4-02 asks for it from the start rather than as a later
 * optimisation, because retrofitting it into a timeline that assumes
 * everything is mounted is a rewrite.
 *
 * A block narrower than one pixel is still drawn, at one pixel. A 4-second
 * acquisition on a two-day axis is sub-pixel, and dropping it would show an
 * empty satellite that is in fact working — the opposite of what this view is
 * for.
 */
export function blockRect(block: Block, view: Viewport): { x: number; width: number } | null {
  if (block.endMs <= view.startMs || block.startMs >= view.endMs) return null;
  const x = toX(block.startMs, view);
  const width = Math.max(1, toX(block.endMs, view) - x);
  return { x, width };
}

/** Only the blocks that intersect the viewport, so the DOM stays small. */
export function visibleBlocks(row: SatelliteRow, view: Viewport): Block[] {
  return row.blocks.filter((block) => block.endMs > view.startMs && block.startMs < view.endMs);
}

/**
 * Zoom about a fixed point on the axis.
 *
 * Anchored on the cursor rather than the centre: zooming toward the middle
 * moves whatever you were looking at, which makes a timeline feel like it is
 * fighting you.
 */
export function zoom(view: Viewport, factor: number, anchorPx: number): Viewport {
  const span = view.endMs - view.startMs;
  const anchorFraction = view.widthPx > 0 ? anchorPx / view.widthPx : 0.5;
  const anchorMs = view.startMs + span * anchorFraction;
  const nextSpan = Math.max(MIN_SPAN_MS, Math.min(MAX_SPAN_MS, span * factor));
  return {
    ...view,
    startMs: anchorMs - nextSpan * anchorFraction,
    endMs: anchorMs + nextSpan * (1 - anchorFraction),
  };
}

export function pan(view: Viewport, deltaPx: number): Viewport {
  const span = view.endMs - view.startMs;
  const deltaMs = view.widthPx > 0 ? (deltaPx / view.widthPx) * span : 0;
  return { ...view, startMs: view.startMs - deltaMs, endMs: view.endMs - deltaMs };
}

/** Ten minutes to a week: "hours to days", with room either side. */
export const MIN_SPAN_MS = 10 * 60 * 1000;
export const MAX_SPAN_MS = 7 * 24 * 60 * 60 * 1000;

/**
 * Axis ticks at a human interval.
 *
 * Chosen from a fixed ladder rather than computed by dividing the span: a
 * "nice" number derived by division lands on 37-minute intervals, and an axis
 * nobody can read the labels of is decoration.
 */
export function ticks(view: Viewport, target = 6): number[] {
  const ladder = [
    5 * 60_000,
    15 * 60_000,
    30 * 60_000,
    60 * 60_000,
    3 * 60 * 60_000,
    6 * 60 * 60_000,
    12 * 60 * 60_000,
    24 * 60 * 60_000,
  ];
  const span = view.endMs - view.startMs;
  const wanted = span / target;
  const step = ladder.find((candidate) => candidate >= wanted) ?? ladder[ladder.length - 1]!;

  const out: number[] = [];
  for (let t = Math.ceil(view.startMs / step) * step; t <= view.endMs; t += step) {
    out.push(t);
  }
  return out;
}
