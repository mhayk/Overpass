'use client';

import { useEffect, useRef, useState } from 'react';

import type { Acquisition } from '@/lib/gateway';

/**
 * The globe.
 *
 * A client component, and the only one that has to be. Cesium needs WebGL, the
 * DOM and a window; it cannot render on the server at all, and importing it
 * from a server component fails at build time rather than politely degrading.
 *
 * It is also the largest asset in the frontend by a wide margin, which is why
 * the import is DYNAMIC and inside an effect: the module is fetched after first
 * paint, so the page is interactive while the globe is still arriving. A static
 * import would block the whole route on several megabytes for a view the user
 * may never scroll to.
 *
 * What it draws today: acquisition footprints on a real WGS84 ellipsoid, with
 * the clock spanning the requested window. What it does NOT draw is the
 * satellites — not because the positions are missing any more (#128 landed the
 * ephemeris projection, and the CZML endpoint now serves a `position` and a
 * `path` per satellite) but because this component builds its entities from
 * `/v1/geo/footprints` rather than from that document. Consuming it is the rest
 * of #28; see web/README.md for the choice it involves.
 *
 * What remains off the table either way: interpolating a path through footprint
 * centroids. It renders something that looks like an orbit and is not one.
 */

export interface CesiumGlobeProps {
  acquisitions: Acquisition[];
  /** Highlighted request, if any. Its footprints render opaque; the rest recede. */
  selectedRequestId?: string | undefined;
}

type LoadState = 'loading' | 'ready' | 'failed';

// RGBA to match what plan-gateway emits in CZML. One table, so the globe and
// the server-rendered document cannot disagree about what "superseded" looks
// like.
const STATUS_COLOUR: Record<string, [number, number, number, number]> = {
  ACTIVE: [64, 196, 255, 100],
  EXECUTED: [80, 220, 140, 120],
  SUPERSEDED: [160, 160, 170, 45],
};

interface PolygonGeometry {
  type: string;
  coordinates: number[][][];
}

function isPolygon(value: unknown): value is PolygonGeometry {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { type?: unknown }).type === 'Polygon' &&
    Array.isArray((value as { coordinates?: unknown }).coordinates)
  );
}

export default function CesiumGlobe({
  acquisitions,
  selectedRequestId,
}: CesiumGlobeProps): React.JSX.Element {
  const container = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<LoadState>('loading');
  const [detail, setDetail] = useState<string>('');

  useEffect(() => {
    let viewer: { destroy: () => void; isDestroyed: () => boolean } | undefined;
    let cancelled = false;

    async function boot(): Promise<void> {
      try {
        const Cesium = await import('cesium');
        await import('cesium/Build/Cesium/Widgets/widgets.css');

        if (cancelled || !container.current) {
          return;
        }

        // Cesium resolves its workers and assets against this at runtime. It is
        // set before the first use rather than in a module scope, because the
        // module scope runs during SSR where `window` does not exist.
        (window as unknown as { CESIUM_BASE_URL: string }).CESIUM_BASE_URL =
          process.env.NEXT_PUBLIC_CESIUM_BASE_URL ?? '/cesium';

        const created = new Cesium.Viewer(container.current, {
          // No Ion token and no imagery provider: the default Ion basemap needs
          // an access token, and a demo that asks a reviewer for one is a demo
          // they do not run. An ellipsoid with a grid is enough to see where a
          // footprint falls, and it loads instantly.
          baseLayer: false,
          baseLayerPicker: false,
          geocoder: false,
          homeButton: false,
          sceneModePicker: false,
          navigationHelpButton: false,
          // The timeline and clock stay: the whole point of the globe is that
          // an acquisition happens at a time, not merely somewhere.
          timeline: true,
          animation: true,
        });
        created.scene.globe.showGroundAtmosphere = true;
        viewer = created;

        for (const acquisition of acquisitions) {
          if (!isPolygon(acquisition.footprint)) {
            continue;
          }
          const ring = acquisition.footprint.coordinates[0];
          if (!ring || ring.length === 0) {
            continue;
          }

          const dimmed = selectedRequestId !== undefined && acquisition.requestId !== selectedRequestId;
          const rgba = STATUS_COLOUR[acquisition.status] ?? STATUS_COLOUR.ACTIVE;
          const [r, g, b, a] = rgba ?? [64, 196, 255, 100];

          created.entities.add({
            id: acquisition.acquisitionId,
            name: `${acquisition.satelliteId} ${acquisition.mode}`,
            description: `request ${acquisition.requestId}, ${acquisition.status}`,
            // availability, so the timeline means something: the footprint
            // exists only while the acquisition is being taken. Without it the
            // globe draws everything at every instant and scrubbing does
            // nothing.
            availability: new Cesium.TimeIntervalCollection([
              new Cesium.TimeInterval({
                start: Cesium.JulianDate.fromIso8601(acquisition.window.start),
                stop: Cesium.JulianDate.fromIso8601(acquisition.window.end),
              }),
            ]),
            polygon: {
              hierarchy: new Cesium.PolygonHierarchy(
                // Longitude first, in GeoJSON and in Cesium alike. Stated
                // because a swap renders perfectly happily in the wrong
                // hemisphere.
                ring.map(([lon, lat]) => Cesium.Cartesian3.fromDegrees(lon ?? 0, lat ?? 0)),
              ),
              material: Cesium.Color.fromBytes(r, g, b, dimmed ? Math.floor(a / 3) : a),
              outline: true,
              outlineColor: Cesium.Color.WHITE.withAlpha(dimmed ? 0.2 : 0.6),
              // GEODESIC, not RHUMB. A swath edge spanning degrees of longitude
              // is a great-circle arc, and the wrong arc type draws a visibly
              // wrong shape at high latitude — where a sun-synchronous
              // constellation spends most of its passes.
              arcType: Cesium.ArcType.GEODESIC,
            },
          });
        }

        if (acquisitions.length > 0) {
          await created.zoomTo(created.entities);
        }
        setState('ready');
      } catch (error) {
        // Reported, never swallowed. A globe that silently fails to load is a
        // black rectangle, and a reviewer reads a black rectangle as "broken
        // product" rather than "missing asset".
        setDetail(error instanceof Error ? error.message : String(error));
        setState('failed');
      }
    }

    void boot();

    return () => {
      cancelled = true;
      if (viewer && !viewer.isDestroyed()) {
        viewer.destroy();
      }
    };
  }, [acquisitions, selectedRequestId]);

  return (
    <div className="relative h-full w-full">
      <div ref={container} className="h-full w-full" data-testid="cesium-container" />

      {state === 'loading' && (
        <div className="absolute inset-0 grid place-items-center bg-slate-950/80 text-slate-300">
          <div className="text-center">
            <div className="mb-2 text-sm font-medium">Loading the globe</div>
            <div className="text-xs text-slate-500">
              Cesium is several megabytes and is fetched after the page is interactive
            </div>
          </div>
        </div>
      )}

      {state === 'failed' && (
        <div className="absolute inset-0 grid place-items-center bg-slate-950/90 p-6 text-slate-300">
          <div className="max-w-md text-center">
            <div className="mb-2 text-sm font-medium text-red-400">The globe failed to load</div>
            <div className="text-xs text-slate-500">
              {detail || 'No detail was reported.'} The acquisition list beside it is unaffected.
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
