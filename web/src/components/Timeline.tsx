'use client';

/**
 * Per-satellite timeline (M4-02).
 *
 * The view where the sequence-dependent setup cost stops being an abstraction.
 * A satellite cannot image two targets in a row without rotating between them,
 * and that rotation is expensive — it is the reason this scheduling problem is
 * not simple interval scheduling.
 *
 * SLEW IS DRAWN AS OCCUPIED, NOT IDLE. That is the whole point of the view. An
 * idle-looking gap invites "why is the satellite doing nothing?", and the
 * answer is that it is rotating. A hatched block says so; whitespace does not.
 *
 * SVG rather than canvas or a chart library. The marks are rectangles on a
 * linear scale — the thing SVG is for — and it keeps every block a real DOM
 * node, which is what makes hover, focus and cross-highlighting work without
 * hit-testing by hand. Virtualisation keeps the node count bounded, which is
 * the reason canvas is usually reached for.
 *
 * BLOCKS ARE VIRTUALISED; ROWS ARE NOT, AND THAT IS DELIBERATE (M4-07).
 *
 * Blocks are unbounded — a busy bucket puts hundreds on one satellite — so
 * visibleBlocks mounts only what intersects the viewport. Rows are one per
 * SATELLITE, so their count is the constellation's size: nine here, and a
 * number that grows by procurement rather than by usage. At nine rows the whole
 * chart is about 330px tall and never scrolls, so windowing them would add a
 * scroll container, a measurement pass and an index calculation to avoid
 * mounting eight <g> elements.
 *
 * Stated rather than left implicit, because "virtualise the rows too" is a
 * reasonable-sounding review comment and the answer should not have to be
 * rediscovered. If this ever renders a row per ACQUISITION or per customer, the
 * bound disappears and the answer changes.
 */

import { useCallback, useMemo, useRef, useState } from 'react';

import type { Acquisition } from '@/lib/gateway';
import {
  buildRows,
  blockRect,
  pan,
  ticks,
  visibleBlocks,
  withGhosts,
  zoom,
  type Block,
  type Ghost,
  type Viewport,
} from '@/lib/timeline';
import { INK, MODE_COLOUR, MODE_FALLBACK, css } from '@/lib/palette';

const ROW_HEIGHT = 34;
const BLOCK_HEIGHT = 18;
const LABEL_WIDTH = 132;
const AXIS_HEIGHT = 22;

export interface TimelineProps {
  acquisitions: Acquisition[];
  /**
   * Candidates that lost. Rendering only winners shows the RESULT of
   * de-confliction; rendering the losers shows the DECISION.
   */
  ghosts?: Ghost[] | undefined;
  /**
   * Why a request lost, keyed by request id, for the ghost tooltip.
   *
   * Supplied by the parent rather than fetched here: the same explanations
   * feed the M4-04 panel, and two components fetching them independently would
   * be two caches disagreeing about one answer.
   */
  reasons?: Record<string, string> | undefined;
  // `| undefined` explicitly, not just `?`. tsconfig sets
  // exactOptionalPropertyTypes, which distinguishes "absent" from "present and
  // undefined" — and a parent holding this in useState always passes the
  // second.
  selectedRequestId?: string | undefined;
  /** Selecting a block cross-highlights it on the globe and the 2D view. */
  onSelectRequest?: ((requestId: string) => void) | undefined;
}

export default function Timeline({
  acquisitions,
  ghosts,
  reasons,
  selectedRequestId,
  onSelectRequest,
}: TimelineProps): React.JSX.Element {
  // OFF BY DEFAULT. Ghosts are dense — there are several candidates per winner
  // — and shown unasked they would bury the schedule they are supposed to
  // explain. The toggle is what makes them a tool rather than noise.
  const [showGhosts, setShowGhosts] = useState(false);

  const rows = useMemo(() => {
    const winners = buildRows(acquisitions);
    return showGhosts && ghosts ? withGhosts(winners, ghosts) : winners;
  }, [acquisitions, ghosts, showGhosts]);

  const bounds = useMemo(() => {
    const times = acquisitions.flatMap((a) => [
      Date.parse(a.window.start),
      Date.parse(a.window.end),
    ]);
    const finite = times.filter((t) => Number.isFinite(t));
    if (finite.length === 0) {
      const now = Date.now();
      return { startMs: now, endMs: now + 6 * 3600_000 };
    }
    const min = Math.min(...finite);
    const max = Math.max(...finite);
    // A little padding, so the first and last blocks are not flush against the
    // frame and readable as "there might be more just off-screen".
    const pad = Math.max(60_000, (max - min) * 0.05);
    return { startMs: min - pad, endMs: max + pad };
  }, [acquisitions]);

  const [width, setWidth] = useState(900);
  const [view, setView] = useState<Viewport | null>(null);
  const dragRef = useRef<{ x: number } | null>(null);

  // The viewport follows the data until the user touches it, then stops. A
  // view that keeps resetting to fit is one you cannot hold still to read.
  const effective: Viewport = view ?? { ...bounds, widthPx: width - LABEL_WIDTH };

  const containerRef = useCallback((node: HTMLDivElement | null) => {
    if (node) setWidth(node.clientWidth);
  }, []);

  const onWheel = useCallback(
    (event: React.WheelEvent<SVGSVGElement>) => {
      event.preventDefault();
      const rect = event.currentTarget.getBoundingClientRect();
      const anchor = event.clientX - rect.left - LABEL_WIDTH;
      setView((current) =>
        zoom(current ?? { ...bounds, widthPx: width - LABEL_WIDTH }, event.deltaY > 0 ? 1.2 : 1 / 1.2, anchor),
      );
    },
    [bounds, width],
  );

  const onPointerDown = useCallback((event: React.PointerEvent<SVGSVGElement>) => {
    dragRef.current = { x: event.clientX };
    event.currentTarget.setPointerCapture(event.pointerId);
  }, []);

  const onPointerMove = useCallback(
    (event: React.PointerEvent<SVGSVGElement>) => {
      const drag = dragRef.current;
      if (!drag) return;
      const delta = event.clientX - drag.x;
      dragRef.current = { x: event.clientX };
      setView((current) => pan(current ?? { ...bounds, widthPx: width - LABEL_WIDTH }, delta));
    },
    [bounds, width],
  );

  const onPointerUp = useCallback(() => {
    dragRef.current = null;
  }, []);

  const height = AXIS_HEIGHT + rows.length * ROW_HEIGHT + 8;

  return (
    <div ref={containerRef} className="h-full w-full overflow-auto" style={{ background: '#0f172a' }}>
      <label className="flex items-center gap-2 px-3 py-2 text-xs text-slate-300">
        <input
          type="checkbox"
          checked={showGhosts}
          onChange={() => setShowGhosts((on) => !on)}
        />
        Show losing candidates
        <span className="text-slate-500">
          — what the planner considered and turned down
        </span>
      </label>

      {rows.length === 0 ? (
        <p className="p-4 text-sm text-slate-400">
          No acquisitions in this window. Submit a request, or widen the window.
        </p>
      ) : (
        <svg
          role="img"
          aria-label="Per-satellite acquisition timeline"
          width="100%"
          height={height}
          onWheel={onWheel}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp}
          style={{ touchAction: 'none', cursor: dragRef.current ? 'grabbing' : 'grab' }}
        >
          <defs>
            {/*
              Hatching, not a flat fill. Slew has to read as a DIFFERENT KIND of
              occupancy from imaging — the satellite is busy but producing
              nothing — and texture says that where a fourth hue would just look
              like a fourth mode. It also survives greyscale and colour
              blindness, which a hue alone does not.
            */}
            <pattern id="slew-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
              <rect width="6" height="6" fill="rgba(195,194,183,0.10)" />
              <line x1="0" y1="0" x2="0" y2="6" stroke="rgba(195,194,183,0.55)" strokeWidth="2" />
            </pattern>
          </defs>

          <Axis view={effective} height={height} />

          {rows.map((row, index) => {
            const y = AXIS_HEIGHT + index * ROW_HEIGHT;
            const dutyPercent =
              row.imagingSeconds + row.slewSeconds > 0
                ? (row.imagingSeconds / (row.imagingSeconds + row.slewSeconds)) * 100
                : 0;

            return (
              <g key={row.satelliteId}>
                <text x={8} y={y + BLOCK_HEIGHT} fill={INK.secondary} fontSize={11}>
                  {row.satelliteId}
                </text>
                <title>
                  {`${row.satelliteId}: ${Math.round(row.imagingSeconds)}s imaging, ` +
                    `${Math.round(row.slewSeconds)}s slewing (${dutyPercent.toFixed(0)}% productive)`}
                </title>

                <line
                  x1={LABEL_WIDTH}
                  y1={y + ROW_HEIGHT - 1}
                  x2="100%"
                  y2={y + ROW_HEIGHT - 1}
                  stroke={INK.grid}
                />

                {visibleBlocks(row, effective).map((block, blockIndex) => (
                  <BlockRect
                    key={`${block.kind}-${block.acquisitionId ?? block.opportunityId ?? blockIndex}-${block.startMs}`}
                    block={block}
                    view={effective}
                    y={y + 6}
                    selected={block.requestId !== undefined && block.requestId === selectedRequestId}
                    reason={block.requestId ? reasons?.[block.requestId] : undefined}
                    onSelect={onSelectRequest}
                  />
                ))}
              </g>
            );
          })}
        </svg>
      )}
    </div>
  );
}

function BlockRect({
  block,
  view,
  y,
  selected,
  reason,
  onSelect,
}: {
  block: Block;
  view: Viewport;
  y: number;
  selected: boolean;
  reason?: string | undefined;
  onSelect?: ((requestId: string) => void) | undefined;
}): React.JSX.Element | null {
  const rect = blockRect(block, view);
  if (!rect) return null;

  const isSlew = block.kind === 'slew';
  const isGhost = block.kind === 'ghost';
  const modeColour = MODE_COLOUR[block.mode ?? ''] ?? MODE_FALLBACK;

  // GHOSTS MUST NOT COMPETE WITH THE COMMITTED SCHEDULE. They keep the mode's
  // hue so a reader can see WHICH kind of pass was declined, but they are
  // outline-only and half-height: unfilled reads as "considered", filled reads
  // as "happened", and the difference has to survive a glance.
  const fill = isSlew ? 'url(#slew-hatch)' : isGhost ? 'none' : css(modeColour, 0.85);

  const seconds = Math.round((block.endMs - block.startMs) / 1000);
  const label = isSlew
    ? `Slewing, ${seconds}s — the satellite is rotating, not idle`
    : isGhost
      ? `Candidate not scheduled — ${block.mode}, ${seconds}s` +
        (reason ? `\nWhy it lost: ${reason}` : '\nSelect it to see why it lost')
      : `${block.mode} acquisition, ${seconds}s`;

  return (
    <g>
      <rect
        x={LABEL_WIDTH + rect.x}
        y={isGhost ? y + BLOCK_HEIGHT / 4 : y}
        width={rect.width}
        height={isGhost ? BLOCK_HEIGHT / 2 : BLOCK_HEIGHT}
        rx={3}
        fill={fill}
        stroke={selected ? INK.primary : isGhost ? css(modeColour, 0.75) : 'transparent'}
        strokeWidth={selected ? 2 : isGhost ? 1 : 0}
        strokeDasharray={isGhost && !selected ? '3 2' : undefined}
        style={{ cursor: block.requestId ? 'pointer' : 'default' }}
        onClick={() => {
          if (block.requestId && onSelect) onSelect(block.requestId);
        }}
      />
      <title>{label}</title>
    </g>
  );
}

function Axis({ view, height }: { view: Viewport; height: number }): React.JSX.Element {
  const marks = ticks(view);
  const span = view.endMs - view.startMs;
  // Below a day, the date is noise repeated on every tick; above it, the time
  // alone is ambiguous.
  const showDate = span > 24 * 3600_000;

  return (
    <g>
      {marks.map((mark) => {
        const x = LABEL_WIDTH + ((mark - view.startMs) / span) * view.widthPx;
        const at = new Date(mark);
        return (
          <g key={mark}>
            <line x1={x} y1={AXIS_HEIGHT - 6} x2={x} y2={height} stroke={INK.grid} />
            <text x={x + 3} y={12} fill={INK.muted} fontSize={10}>
              {showDate
                ? at.toISOString().slice(5, 16).replace('T', ' ')
                : at.toISOString().slice(11, 16)}
            </text>
          </g>
        );
      })}
    </g>
  );
}
