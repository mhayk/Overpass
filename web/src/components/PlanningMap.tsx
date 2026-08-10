'use client';

/**
 * The 2D planning view (M4-01).
 *
 * It answers a different question from the globe. The globe answers "where is
 * this satellite, what can it see, and when" — a question about a rotating
 * ellipsoid. This answers "where is the demand, and where did requests
 * collide", which is a question about hundreds of overlapping shapes at once
 * and is legible precisely because it is flattened. ADR-0009 is the argument;
 * this is the thing it argued for.
 *
 * NO BASEMAP, deliberately. deck.gl has no built-in one, and the alternatives
 * are a tile service (network, an API key, a demo that breaks on a plane) or
 * bundling coastline geometry (another dependency, another thing to keep in
 * step with the projection). A graticule plus the data's own geometry gives
 * enough spatial anchoring for a contention view, and `make up` still works
 * offline — which the rest of this stack is careful about.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import DeckGL from '@deck.gl/react';
import { GeoJsonLayer, LineLayer, ScatterplotLayer } from '@deck.gl/layers';
import { HexagonLayer } from '@deck.gl/aggregation-layers';
import type { MapViewState, PickingInfo } from '@deck.gl/core';

import {
  fetchOpportunityFootprints,
  fetchTargets,
  type GeoCollection,
  type OpportunityProperties,
  type TargetProperties,
  type TimeRange,
  type TypedFeature,
} from '@/lib/gateway';
import {
  CONTENTION_RAMP,
  COVERAGE_RAMP,
  INK,
  MODE_COLOUR,
  MODE_FALLBACK,
  SURFACE,
  TARGET_COLOUR,
  css,
} from '@/lib/palette';

/** Rotterdam, the demo's centre of gravity. */
const INITIAL_VIEW: MapViewState = {
  longitude: 4.4,
  latitude: 51.9,
  zoom: 3,
  pitch: 0,
  bearing: 0,
};

export interface LayerToggles {
  footprints: boolean;
  coverage: boolean;
  targets: boolean;
  conflicts: boolean;
}

const DEFAULT_TOGGLES: LayerToggles = {
  footprints: true,
  coverage: false,
  targets: true,
  conflicts: true,
};

function modeColour(mode: string): [number, number, number] {
  return MODE_COLOUR[mode] ?? MODE_FALLBACK;
}

/**
 * A polygon's representative point, for the aggregation layers.
 *
 * HexagonLayer bins POINTS, and a footprint is a polygon. Using the first
 * vertex would bias every bin toward one corner of every swath, so this takes
 * the mean of the outer ring — close enough to a centroid for binning, and it
 * does not need a geometry library to compute.
 *
 * This is the one place the client derives geometry, and it is derived for
 * BINNING rather than for display: the polygons themselves are drawn from the
 * server's coordinates untouched, which is the rule ADR-0009 sets.
 */
function ringCentre(geometry: unknown): [number, number] | null {
  const geo = geometry as { type?: string; coordinates?: unknown };
  if (geo?.type !== 'Polygon' || !Array.isArray(geo.coordinates)) return null;
  const ring = geo.coordinates[0];
  if (!Array.isArray(ring) || ring.length === 0) return null;

  let sumLon = 0;
  let sumLat = 0;
  let count = 0;
  for (const position of ring) {
    if (!Array.isArray(position) || position.length < 2) continue;
    sumLon += Number(position[0]);
    sumLat += Number(position[1]);
    count += 1;
  }
  return count === 0 ? null : [sumLon / count, sumLat / count];
}

function pointOf(geometry: unknown): [number, number] | null {
  const geo = geometry as { type?: string; coordinates?: unknown };
  if (geo?.type === 'Point' && Array.isArray(geo.coordinates)) {
    return [Number(geo.coordinates[0]), Number(geo.coordinates[1])];
  }
  return ringCentre(geometry);
}

/** A 30° graticule, so the flattened view has some spatial anchoring. */
function graticule(): { from: [number, number]; to: [number, number] }[] {
  const lines: { from: [number, number]; to: [number, number] }[] = [];
  for (let lon = -180; lon <= 180; lon += 30) {
    lines.push({ from: [lon, -85], to: [lon, 85] });
  }
  for (let lat = -60; lat <= 60; lat += 30) {
    lines.push({ from: [-180, lat], to: [180, lat] });
  }
  return lines;
}

export interface PlanningMapProps {
  /**
   * The window, shared with the globe rather than owned here. M4-01 asks for
   * the two views to move together, and the only way that holds is for one
   * piece of state to feed both.
   */
  window: TimeRange;
}

export default function PlanningMap({ window: timeWindow }: PlanningMapProps): React.JSX.Element {
  const [toggles, setToggles] = useState<LayerToggles>(DEFAULT_TOGGLES);
  const [targets, setTargets] = useState<GeoCollection<TargetProperties> | null>(null);
  const [candidates, setCandidates] = useState<GeoCollection<OpportunityProperties> | null>(null);
  const [error, setError] = useState<string | undefined>();
  const [hover, setHover] = useState<PickingInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load(): Promise<void> {
      try {
        const [nextTargets, nextCandidates] = await Promise.all([
          fetchTargets(timeWindow),
          fetchOpportunityFootprints(timeWindow),
        ]);
        if (cancelled) return;
        setTargets(nextTargets);
        setCandidates(nextCandidates);
        setError(undefined);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : String(cause));
      }
    }
    void load();
    return () => {
      cancelled = true;
    };
  }, [timeWindow]);

  const lost = useMemo(
    () => (candidates?.features ?? []).filter((feature) => !feature.properties.awarded),
    [candidates],
  );

  const conflictPoints = useMemo(
    () =>
      lost
        .map((feature) => ({ position: pointOf(feature.geometry) }))
        .filter((row): row is { position: [number, number] } => row.position !== null),
    [lost],
  );

  const coveragePoints = useMemo(
    () =>
      (candidates?.features ?? [])
        .filter((feature) => feature.properties.awarded)
        .map((feature) => ({ position: pointOf(feature.geometry) }))
        .filter((row): row is { position: [number, number] } => row.position !== null),
    [candidates],
  );

  const targetPoints = useMemo(
    () =>
      (targets?.features ?? [])
        .map((feature) => ({
          position: pointOf(feature.geometry),
          properties: feature.properties,
        }))
        .filter(
          (row): row is { position: [number, number]; properties: TargetProperties } =>
            row.position !== null,
        ),
    [targets],
  );

  const layers = useMemo(() => {
    const built = [];

    built.push(
      new LineLayer({
        id: 'graticule',
        data: graticule(),
        getSourcePosition: (d: { from: [number, number] }) => d.from,
        getTargetPosition: (d: { to: [number, number] }) => d.to,
        getColor: [195, 194, 183, 30],
        getWidth: 1,
        pickable: false,
      }),
    );

    // Coverage FIRST, so it sits under the shapes it aggregates. A heatmap
    // painted over its own polygons hides the thing it summarises.
    if (toggles.coverage) {
      built.push(
        new HexagonLayer({
          id: 'coverage',
          data: coveragePoints,
          getPosition: (d: { position: [number, number] }) => d.position,
          radius: 40000,
          coverage: 0.92,
          extruded: false,
          colorRange: COVERAGE_RAMP,
          opacity: 0.55,
          pickable: false,
        }),
      );
    }

    if (toggles.conflicts) {
      built.push(
        new HexagonLayer({
          id: 'conflicts',
          data: conflictPoints,
          getPosition: (d: { position: [number, number] }) => d.position,
          radius: 40000,
          coverage: 0.92,
          extruded: false,
          colorRange: CONTENTION_RAMP,
          opacity: 0.6,
          pickable: true,
        }),
      );
    }

    if (toggles.footprints) {
      built.push(
        new GeoJsonLayer({
          id: 'footprints',
          data: {
            type: 'FeatureCollection',
            features: (candidates?.features ?? []).filter((f) => f.properties.awarded),
          } as never,
          filled: true,
          stroked: true,
          // Cast at the boundary: deck.gl types its accessors against its own
          // Feature and the properties are ours. One assertion here beats four
          // inside the callbacks, and this is the only place the two type
          // worlds meet.
          getFillColor: ((feature: TypedFeature<OpportunityProperties>) => {
            const [r, g, b] = modeColour(feature.properties.mode);
            return [r, g, b, 90];
          }) as never,
          // A 2px surface ring on overlapping marks, so two swaths that
          // overlap read as two rather than as one darker shape.
          getLineColor: ((feature: TypedFeature<OpportunityProperties>) =>
            modeColour(feature.properties.mode)) as never,
          getLineWidth: 2,
          lineWidthUnits: 'pixels',
          pickable: true,
        }),
      );
    }

    if (toggles.targets) {
      built.push(
        new ScatterplotLayer({
          id: 'targets',
          data: targetPoints,
          getPosition: (d: { position: [number, number] }) => d.position,
          // Size carries opportunity_count, so a target nobody could image
          // reads as small rather than as absent. Radius in pixels, so zoom
          // does not change what the symbol means.
          getRadius: (d: { properties: TargetProperties }) =>
            4 + Math.min(6, d.properties.opportunity_count),
          radiusUnits: 'pixels',
          getFillColor: [TARGET_COLOUR[0], TARGET_COLOUR[1], TARGET_COLOUR[2], 210],
          stroked: true,
          getLineColor: [26, 26, 25, 255],
          lineWidthUnits: 'pixels',
          getLineWidth: 1,
          pickable: true,
        }),
      );
    }

    return built;
  }, [toggles, candidates, conflictPoints, coveragePoints, targetPoints]);

  const onToggle = useCallback((key: keyof LayerToggles) => {
    setToggles((current) => ({ ...current, [key]: !current[key] }));
  }, []);

  const truncated = (targets?.truncated ?? false) || (candidates?.truncated ?? false);

  return (
    <section
      aria-label="Planning map"
      style={{ position: 'relative', width: '100%', height: '100%', background: SURFACE }}
    >
      <DeckGL
        initialViewState={INITIAL_VIEW}
        controller
        layers={layers}
        onHover={setHover}
        getTooltip={null}
        style={{ position: 'absolute', inset: '0' }}
      />

      <Legend toggles={toggles} onToggle={onToggle} />

      {truncated ? (
        <p role="status" style={bannerStyle}>
          Showing a bounded slice — narrow the window to see everything in it.
        </p>
      ) : null}

      {error ? (
        <p role="alert" style={{ ...bannerStyle, color: '#e66767' }}>
          {error}
        </p>
      ) : null}

      {hover?.object ? <Tooltip info={hover} /> : null}
    </section>
  );
}

const bannerStyle: React.CSSProperties = {
  position: 'absolute',
  bottom: 12,
  left: 12,
  margin: 0,
  padding: '6px 10px',
  borderRadius: 6,
  background: 'rgba(26,26,25,0.86)',
  color: INK.secondary,
  fontSize: 12,
};

/**
 * The legend, which is also the layer switch.
 *
 * One control rather than two: a legend that explains a layer you cannot turn
 * off, beside a switch that does not say what it turns on, is two half
 * components. Identity is never colour alone — every swatch carries its label,
 * which is what makes the map readable in the CVD and print cases the palette
 * validator cannot cover.
 */
function Legend({
  toggles,
  onToggle,
}: {
  toggles: LayerToggles;
  onToggle: (key: keyof LayerToggles) => void;
}): React.JSX.Element {
  return (
    <div
      style={{
        position: 'absolute',
        top: 12,
        right: 12,
        padding: '10px 12px',
        borderRadius: 8,
        background: 'rgba(26,26,25,0.88)',
        color: INK.primary,
        fontSize: 12,
        minWidth: 190,
      }}
    >
      <LayerSwitch
        label="Acquisitions"
        hint="won candidates, by mode"
        checked={toggles.footprints}
        onChange={() => onToggle('footprints')}
      >
        <div style={{ display: 'flex', gap: 10, marginTop: 6 }}>
          {Object.entries(MODE_COLOUR).map(([mode, colour]) => (
            <span key={mode} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
              <span
                aria-hidden
                style={{
                  width: 10,
                  height: 10,
                  borderRadius: 2,
                  background: css(colour),
                  display: 'inline-block',
                }}
              />
              <span style={{ color: INK.secondary }}>{mode}</span>
            </span>
          ))}
        </div>
      </LayerSwitch>

      <LayerSwitch
        label="Contention"
        hint="losing candidates per cell"
        checked={toggles.conflicts}
        onChange={() => onToggle('conflicts')}
      >
        <Ramp ramp={CONTENTION_RAMP} low="few" high="many" />
      </LayerSwitch>

      <LayerSwitch
        label="Coverage"
        hint="awarded footprints per cell"
        checked={toggles.coverage}
        onChange={() => onToggle('coverage')}
      >
        <Ramp ramp={COVERAGE_RAMP} low="thin" high="dense" />
      </LayerSwitch>

      <LayerSwitch
        label="Targets"
        hint="what was asked for"
        checked={toggles.targets}
        onChange={() => onToggle('targets')}
      >
        <span style={{ color: INK.muted }}>size = opportunities found</span>
      </LayerSwitch>
    </div>
  );
}

function LayerSwitch({
  label,
  hint,
  checked,
  onChange,
  children,
}: {
  label: string;
  hint: string;
  checked: boolean;
  onChange: () => void;
  children?: React.ReactNode;
}): React.JSX.Element {
  return (
    <div style={{ marginBottom: 10 }}>
      <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
        <input type="checkbox" checked={checked} onChange={onChange} aria-label={label} />
        <span>{label}</span>
      </label>
      <div style={{ color: INK.muted, marginLeft: 22, fontSize: 11 }}>{hint}</div>
      {checked ? <div style={{ marginLeft: 22 }}>{children}</div> : null}
    </div>
  );
}

function Ramp({
  ramp,
  low,
  high,
}: {
  ramp: [number, number, number][];
  low: string;
  high: string;
}): React.JSX.Element {
  return (
    <div style={{ marginTop: 6 }}>
      <div style={{ display: 'flex', height: 8, borderRadius: 2, overflow: 'hidden' }}>
        {ramp.map((step, index) => (
          <span key={index} aria-hidden style={{ flex: 1, background: css(step) }} />
        ))}
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', color: INK.muted }}>
        <span>{low}</span>
        <span>{high}</span>
      </div>
    </div>
  );
}

function Tooltip({ info }: { info: PickingInfo }): React.JSX.Element | null {
  const object = info.object as
    | { properties?: OpportunityProperties | TargetProperties; count?: number }
    | undefined;
  if (!object) return null;

  const rows: [string, string][] = [];
  const properties = object.properties;

  if (properties && 'satellite_id' in properties) {
    rows.push(['Satellite', properties.satellite_id]);
    rows.push(['Mode', properties.mode]);
    rows.push(['Window', `${properties.window_start} → ${properties.window_end}`]);
  } else if (properties && 'request_id' in properties) {
    rows.push(['Customer', properties.customer_id]);
    rows.push(['State', properties.state]);
    rows.push(['Opportunities', String(properties.opportunity_count)]);
  } else if (typeof object.count === 'number') {
    // A hexbin. The count IS the finding, so it is the whole tooltip.
    rows.push(['Losing candidates', String(object.count)]);
  }

  if (rows.length === 0) return null;

  return (
    <div
      role="tooltip"
      style={{
        position: 'absolute',
        left: `${(info.x ?? 0) + 12}px`,
        top: `${(info.y ?? 0) + 12}px`,
        padding: '6px 8px',
        borderRadius: 6,
        background: 'rgba(26,26,25,0.94)',
        color: INK.primary,
        fontSize: 12,
        pointerEvents: 'none',
      }}
    >
      {rows.map(([label, value]) => (
        <div key={label}>
          <span style={{ color: INK.muted }}>{label}: </span>
          {value}
        </div>
      ))}
    </div>
  );
}
