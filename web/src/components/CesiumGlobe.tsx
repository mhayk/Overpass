'use client';

import { useEffect, useRef, useState } from 'react';

import type * as Cesium from 'cesium';

import type { Acquisition, Opportunity } from '@/lib/gateway';

// Type-only, so this costs nothing at runtime: `import type` is erased before
// the bundle is built, and the module still arrives through the dynamic import
// inside the boot effect. The alternative was `any` on the refs, which
// tsconfig forbids and which would have hidden the entity API entirely.
type CesiumModule = typeof Cesium;
type CesiumViewer = InstanceType<CesiumModule['Viewer']>;
type CesiumDataSource = Awaited<ReturnType<CesiumModule['CzmlDataSource']['load']>>;

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
 * TWO SOURCES, ON PURPOSE, and the split is the decision worth knowing.
 *
 * The ORBIT TRACKS come from the server's CZML, loaded straight into
 * CzmlDataSource. Cesium's own loader is the only thing that should interpret a
 * CZML packet stream, and the server already renders it from the one read model
 * — re-modelling it here would be the second implementation of the geometry
 * that ADR-0009 exists to prevent, and the two would disagree eventually.
 *
 * The FOOTPRINTS stay hand-built entities. They need per-request selection
 * highlighting, which means mutating material and outline alpha on entities
 * this component owns; CzmlDataSource owns everything it loads, so driving
 * selection through it would mean rewriting packets and reloading the document
 * on every click.
 *
 * What remains off the table: interpolating a path through footprint centroids.
 * It renders something that looks like an orbit and is not one, which is worse
 * than an absent layer because a viewer would believe it.
 */

export interface CesiumGlobeProps {
  acquisitions: Acquisition[];
  /** Highlighted request, if any. Its footprints render opaque; the rest recede. */
  selectedRequestId?: string | undefined;
  /**
   * The constellation's orbit tracks, as the server rendered them.
   *
   * Opaque: this component hands the document to Cesium and does not read it.
   * Undefined and empty are the same thing here — a window the ephemeris sweep
   * has not reached yet is a globe with no satellites on it, which is the
   * honest rendering rather than an error.
   */
  constellation?: unknown[] | undefined;
  /** Candidate footprints for the selected request, drawn as ghosts. */
  opportunities?: Opportunity[] | undefined;
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

/**
 * The outer ring of a footprint, or undefined if there is not one.
 *
 * One helper rather than the same four-line guard at each call site: an
 * acquisition and an opportunity carry the same geometry and used to check it
 * separately, which is two places for a hemisphere-swapping mistake to hide.
 */
function polygonRing(footprint: unknown): number[][] | undefined {
  if (!isPolygon(footprint)) {
    return undefined;
  }
  const ring = footprint.coordinates[0];
  return ring && ring.length > 0 ? ring : undefined;
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
  constellation,
  opportunities,
}: CesiumGlobeProps): React.JSX.Element {
  const container = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<LoadState>('loading');
  const [detail, setDetail] = useState<string>('');

  // THE VIEWER IS BUILT ONCE AND THEN MUTATED.
  //
  // It used to be built inside an effect keyed on
  // [acquisitions, selectedRequestId, constellation, opportunities], so every
  // SSE update and EVERY CLICK destroyed the WebGL context, constructed a new
  // Viewer, re-added every entity and re-ran zoomTo. Measured with a tagged
  // canvas: one click on an acquisition and the tagged node was gone. The
  // camera reset with it, which is the part a user notices — you cannot select
  // anything without losing your view.
  //
  // These refs are what let the data effects below reach the live viewer
  // without re-running boot. `cesium` is held too, because the module is
  // dynamically imported and the sync effects need its constructors.
  const viewerRef = useRef<CesiumViewer | null>(null);
  const cesiumRef = useRef<CesiumModule | null>(null);
  // Framing is a one-off, not a per-update behaviour. Re-running zoomTo on
  // every data change is how a globe yanks the camera away mid-inspection.
  const framedRef = useRef(false);
  const czmlRef = useRef<CesiumDataSource | null>(null);

  useEffect(() => {
    let cancelled = false;

    async function boot(): Promise<void> {
      try {
        const Cesium = await import('cesium');
        await import('cesium/Build/Cesium/Widgets/widgets.css');

        // The CSS import resolves when the stylesheet is inserted, not when
        // its rules apply. A Viewer constructed in that gap measures unstyled
        // sub-widgets: the fullscreen button spans the full width as a bare
        // block, Viewer.resize sets the timeline's right offset from it, and
        // the timeline collapses to zero width. The per-frame resize never
        // revisits any of it, because its guard only acts when the CONTAINER
        // changes size — so the broken layout is permanent until a remount.
        // Probe one widgets.css rule and wait until it measurably applies.
        await new Promise<void>((resolve) => {
          const probe = document.createElement('div');
          probe.className = 'cesium-viewer-fullscreenContainer';
          probe.style.visibility = 'hidden';
          document.body.appendChild(probe);
          const deadline = performance.now() + 2000;
          const check = (): void => {
            const applied = getComputedStyle(probe).position === 'absolute';
            if (applied || cancelled || performance.now() > deadline) {
              probe.remove();
              resolve();
            } else {
              requestAnimationFrame(check);
            }
          };
          check();
        });

        if (cancelled || !container.current) {
          return;
        }

        // Cesium resolves its workers and assets against this at runtime. It is
        // set before the first use rather than in a module scope, because the
        // module scope runs during SSR where `window` does not exist.
        (window as unknown as { CESIUM_BASE_URL: string }).CESIUM_BASE_URL =
          process.env.NEXT_PUBLIC_CESIUM_BASE_URL ?? '/cesium';

        const created = new Cesium.Viewer(container.current, {
          // No Ion token: the default Ion basemap needs an access token, and a
          // demo that asks a reviewer for one is a demo they do not run. The
          // basemap is instead Natural Earth II, the low-resolution tileset
          // Cesium ships in its own assets — copy-cesium-assets.mjs already
          // serves it under CESIUM_BASE_URL, so this stays fully offline.
          baseLayer: Cesium.ImageryLayer.fromProviderAsync(
            Cesium.TileMapServiceImageryProvider.fromUrl(
              Cesium.buildModuleUrl('Assets/Textures/NaturalEarthII'),
            ),
            {},
          ),
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

        if (cancelled) {
          created.destroy();
          return;
        }

        cesiumRef.current = Cesium;
        viewerRef.current = created;
        // Marks the viewer live, and re-runs the data effects below, which are
        // gated on it. They cannot usefully run before this point.
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
      const viewer = viewerRef.current;
      viewerRef.current = null;
      czmlRef.current = null;
      if (viewer && !viewer.isDestroyed()) {
        viewer.destroy();
      }
    };
  }, []);

  // The footprints, synced in place.
  //
  // Cesium's EntityCollection is keyed by id, and `entities.add` on an existing
  // id throws rather than replacing — so this removes what left, adds what
  // arrived, and updates the appearance of what stayed. suspendEvents batches
  // the whole thing into one scene update instead of one per entity, which is
  // the difference between a redraw and a stutter when a plan commits forty
  // acquisitions at once.
  useEffect(() => {
    const viewer = viewerRef.current;
    const Cesium = cesiumRef.current;
    if (!viewer || !Cesium || viewer.isDestroyed()) {
      return;
    }

    const desired = new Map<string, () => void>();

    for (const acquisition of acquisitions) {
      const ring = polygonRing(acquisition.footprint);
      if (!ring) continue;

      const dimmed = selectedRequestId !== undefined && acquisition.requestId !== selectedRequestId;
      const rgba = STATUS_COLOUR[acquisition.status] ?? STATUS_COLOUR.ACTIVE;
      const [r, g, b, a] = rgba ?? [64, 196, 255, 100];

      desired.set(acquisition.acquisitionId, () => {
        const existing = viewer.entities.getById(acquisition.acquisitionId);
        const material = Cesium.Color.fromBytes(r, g, b, dimmed ? Math.floor(a / 3) : a);
        const outline = Cesium.Color.WHITE.withAlpha(dimmed ? 0.2 : 0.6);

        if (existing?.polygon) {
          // SELECTION IS A MATERIAL CHANGE, NOT A REBUILD. This is the whole
          // point: clicking used to cost a new WebGL context.
          existing.polygon.material = new Cesium.ColorMaterialProperty(material);
          existing.polygon.outlineColor = new Cesium.ConstantProperty(outline);
          return;
        }

        viewer.entities.add({
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
            // WRAPPED EXPLICITLY, not handed a bare Color to infer from.
            //
            // Cesium infers a material from a Color only when the value
            // reaches the PolygonGraphics constructor by the path it expects;
            // a Color held in a variable and passed through this options
            // object threw "Unable to infer material type" and took the whole
            // React tree down with it — a blank page, not a broken globe.
            // Constructing the property says what is meant and cannot be
            // inferred wrongly. Matches the update path below.
            material: new Cesium.ColorMaterialProperty(material),
            outline: true,
            outlineColor: new Cesium.ConstantProperty(outline),
            // GEODESIC, not RHUMB. A swath edge spanning degrees of longitude
            // is a great-circle arc, and the wrong arc type draws a visibly
            // wrong shape at high latitude — where a sun-synchronous
            // constellation spends most of its passes.
            arcType: Cesium.ArcType.GEODESIC,
          },
        });
      });
    }

    // Candidate footprints, as ghosts. The losers are the point: a winner
    // shown alone explains nothing about why it won.
    for (const opportunity of opportunities ?? []) {
      const ring = polygonRing(opportunity.footprint);
      if (!ring) continue;
      const id = `opportunity/${opportunity.opportunityId}`;

      desired.set(id, () => {
        if (viewer.entities.getById(id)) {
          return;
        }
        viewer.entities.add({
          id,
          name: `${opportunity.satelliteId} ${opportunity.mode} (candidate)`,
          description: opportunity.won ? 'scheduled' : 'not scheduled',
          polygon: {
            hierarchy: new Cesium.PolygonHierarchy(
              ring.map(([lon, lat]) => Cesium.Cartesian3.fromDegrees(lon ?? 0, lat ?? 0)),
            ),
            // Unfilled and thin. A candidate is not a commitment, and forty
            // filled polygons over one target is an unreadable smear.
            material: new Cesium.ColorMaterialProperty(
              Cesium.Color.fromBytes(255, 190, 90, opportunity.won ? 60 : 12),
            ),
            outline: true,
            outlineColor: new Cesium.ConstantProperty(
              Cesium.Color.fromBytes(255, 190, 90, opportunity.won ? 200 : 90),
            ),
            arcType: Cesium.ArcType.GEODESIC,
          },
        });
      });
    }

    try {
      viewer.entities.suspendEvents();
      // Removals first, so an id that changed shape is gone before it is
      // re-added — `add` on a live id throws.
      for (const entity of viewer.entities.values.slice()) {
        if (!desired.has(entity.id)) {
          viewer.entities.remove(entity);
        }
      }
      for (const apply of desired.values()) {
        apply();
      }
    } catch (error) {
      // DEGRADE THE GLOBE, DO NOT TAKE THE PAGE WITH IT.
      //
      // An exception thrown from this effect propagates out of React's commit
      // phase and unmounts the whole tree — the acquisition list, the
      // timeline, the rejection panel, all of it. That is not hypothetical:
      // wrapping a Color as a material wrongly threw "Unable to infer material
      // type" here and the page rendered blank, with the cause visible only as
      // a minified class name in the console.
      //
      // The failure state below already says the list beside it is unaffected.
      // This is what makes that true.
      setDetail(error instanceof Error ? error.message : String(error));
      setState('failed');
    } finally {
      viewer.entities.resumeEvents();
    }

    if (!framedRef.current && viewer.entities.values.length > 0) {
      framedRef.current = true;
      // An explicit offset, because the default frames the bounding sphere of
      // the footprints — kilometre-scale polygons — and parks the camera a few
      // kilometres up, where the offline Natural Earth II basemap is a blur.
      // Straight down (-90°) over the footprints keeps their region facing the
      // camera; 15 000 km back shows the whole planet centred in the viewport,
      // which is the framing a tasking view opens on. Zooming in is one scroll.
      void viewer.zoomTo(
        viewer.entities,
        new Cesium.HeadingPitchRange(0, Cesium.Math.toRadians(-90), 15_000_000),
      );
    }
  }, [acquisitions, opportunities, selectedRequestId, state]);

  // The orbit tracks, straight into Cesium's own loader.
  //
  // Its own effect keyed on the document alone, so a selection change no longer
  // reloads several megabytes of CZML. The clock the document carries is what
  // makes scrubbing move satellites rather than merely move a cursor.
  useEffect(() => {
    const viewer = viewerRef.current;
    const Cesium = cesiumRef.current;
    if (!viewer || !Cesium || viewer.isDestroyed()) {
      return;
    }
    if (!constellation || constellation.length === 0) {
      return;
    }

    let cancelled = false;
    void (async () => {
      const source = await Cesium.CzmlDataSource.load(constellation);
      if (cancelled || viewer.isDestroyed()) {
        return;
      }
      // The previous document goes only once its replacement has loaded, so
      // the globe never blinks empty.
      const previous = czmlRef.current;
      await viewer.dataSources.add(source);
      if (previous) {
        viewer.dataSources.remove(previous, true);
      }
      czmlRef.current = source;
      viewer.clock.shouldAnimate = false;
    })();

    return () => {
      cancelled = true;
    };
  }, [constellation, state]);

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
