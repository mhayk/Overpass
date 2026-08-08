'use client';

import dynamic from 'next/dynamic';
import { useCallback, useEffect, useState } from 'react';

import { GatewayError, fetchAcquisitions, type Acquisition, type Staleness } from '@/lib/gateway';
import { SubmitRejected, submitRequest, type FieldError } from '@/lib/tasking';

/**
 * The interactive shell around the globe.
 *
 * next/dynamic with ssr:false, because Cesium touches `window` at import time.
 * The loading state is deliberate and visible rather than a blank div: a
 * multi-megabyte asset arriving silently reads as a broken page, and telling
 * the user what is happening costs one line.
 */
const CesiumGlobe = dynamic(() => import('@/components/CesiumGlobe'), {
  ssr: false,
  loading: () => (
    <div className="grid h-full place-items-center text-sm text-slate-500">
      Preparing the globe…
    </div>
  ),
});

function defaultWindow(): { start: string; end: string } {
  const now = new Date();
  const start = new Date(now.getTime() - 6 * 3600_000);
  const end = new Date(now.getTime() + 42 * 3600_000);
  return { start: start.toISOString(), end: end.toISOString() };
}

/**
 * How current the answer is, on screen rather than in a console.
 *
 * Every gateway response carries this and the whole point is that a reader can
 * see it. A globe left open on a wall display is the surface where "this is
 * current" is assumed hardest.
 */
function StalenessBadge({ staleness }: { staleness: Staleness }): React.JSX.Element {
  if (Number.isNaN(staleness.lagSeconds)) {
    return <span className="text-xs text-amber-400">age unknown</span>;
  }
  const stale = staleness.lagSeconds > 60;
  return (
    <span className={`text-xs ${stale ? 'text-amber-400' : 'text-slate-500'}`}>
      {Math.round(staleness.lagSeconds)}s behind
    </span>
  );
}

export default function Workspace(): React.JSX.Element {
  const [acquisitions, setAcquisitions] = useState<Acquisition[]>([]);
  const [staleness, setStaleness] = useState<Staleness>({ asOf: '', lagSeconds: Number.NaN });
  const [selectedRequestId, setSelectedRequestId] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const [submitting, setSubmitting] = useState(false);
  const [submitNote, setSubmitNote] = useState<string | undefined>(undefined);
  const [fieldErrors, setFieldErrors] = useState<FieldError[]>([]);

  const load = useCallback(async () => {
    try {
      const result = await fetchAcquisitions(defaultWindow());
      setAcquisitions(result.items);
      setStaleness(result.staleness);
      setError(undefined);
    } catch (cause) {
      // A failed read and an empty result are different answers and are shown
      // differently: this is a banner, an empty list is just an empty list.
      setError(
        cause instanceof GatewayError
          ? `${cause.message}${cause.transient ? ' — retrying may help' : ''}`
          : String(cause),
      );
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function onSubmit(event: React.FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    setSubmitting(true);
    setFieldErrors([]);
    setSubmitNote(undefined);

    const form = new FormData(event.currentTarget);
    const window = defaultWindow();

    try {
      const result = await submitRequest({
        customerId: String(form.get('customer_id') ?? ''),
        targetName: String(form.get('target_name') ?? ''),
        target: {
          type: 'Point',
          // Longitude first. The field order on screen matches the array order
          // so a reader can check it without reading this file.
          coordinates: [Number(form.get('lon')), Number(form.get('lat'))],
        },
        windowStart: window.start,
        windowEnd: window.end,
        priorityTier: 'BEST_EFFORT',
        bidCredits: Number(form.get('bid_credits') ?? 0),
        requestedModes: ['SCAN'],
      });

      setSubmitNote(
        result.replayed
          ? `Already submitted — replayed ${result.requestId}, nothing was duplicated`
          : `Accepted ${result.requestId}`,
      );
      await load();
    } catch (cause) {
      if (cause instanceof SubmitRejected) {
        setFieldErrors(cause.fieldErrors);
        setSubmitNote(cause.message);
      } else {
        setSubmitNote(String(cause));
      }
    } finally {
      setSubmitting(false);
    }
  }

  const requestIds = [...new Set(acquisitions.map((a) => a.requestId))];

  return (
    <main className="grid h-full grid-cols-[380px_1fr]">
      <aside className="flex flex-col gap-6 overflow-y-auto border-r border-slate-800 p-5">
        <header>
          <h1 className="text-lg font-semibold">Overpass</h1>
          <p className="text-xs text-slate-500">Satellite tasking and collection planning</p>
        </header>

        <section>
          <div className="mb-2 flex items-baseline justify-between">
            <h2 className="text-sm font-medium">Acquisitions</h2>
            <StalenessBadge staleness={staleness} />
          </div>

          {error !== undefined && (
            <p className="rounded border border-red-900 bg-red-950/40 p-2 text-xs text-red-300">
              {error}
            </p>
          )}

          {error === undefined && acquisitions.length === 0 && (
            <p className="text-xs text-slate-500">
              Nothing scheduled in this window. Run <code>make demo</code> to submit some.
            </p>
          )}

          <ul className="flex flex-col gap-1">
            {requestIds.map((requestId) => {
              const selected = requestId === selectedRequestId;
              const forRequest = acquisitions.filter((a) => a.requestId === requestId);
              return (
                <li key={requestId}>
                  <button
                    type="button"
                    onClick={() => setSelectedRequestId(selected ? undefined : requestId)}
                    className={`w-full rounded px-2 py-1.5 text-left text-xs ${
                      selected ? 'bg-slate-800 text-slate-100' : 'text-slate-400 hover:bg-slate-900'
                    }`}
                  >
                    <span className="block font-mono">{requestId.slice(0, 8)}</span>
                    <span className="text-slate-500">
                      {forRequest.length} acquisition{forRequest.length === 1 ? '' : 's'} ·{' '}
                      {forRequest[0]?.satelliteId}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        </section>

        <section>
          <h2 className="mb-2 text-sm font-medium">Submit a request</h2>
          <form onSubmit={onSubmit} className="flex flex-col gap-2 text-xs">
            <input
              name="customer_id"
              defaultValue="acme-imaging"
              aria-label="Customer"
              className="rounded bg-slate-900 px-2 py-1.5"
            />
            <input
              name="target_name"
              defaultValue="Rotterdam"
              aria-label="Target name"
              className="rounded bg-slate-900 px-2 py-1.5"
            />
            <div className="grid grid-cols-2 gap-2">
              <input
                name="lon"
                type="number"
                step="any"
                defaultValue="4.4"
                aria-label="Longitude"
                className="rounded bg-slate-900 px-2 py-1.5"
              />
              <input
                name="lat"
                type="number"
                step="any"
                defaultValue="51.9"
                aria-label="Latitude"
                className="rounded bg-slate-900 px-2 py-1.5"
              />
            </div>
            <input
              name="bid_credits"
              type="number"
              defaultValue="0"
              aria-label="Bid credits"
              className="rounded bg-slate-900 px-2 py-1.5"
            />
            <button
              type="submit"
              disabled={submitting}
              className="rounded bg-sky-700 px-2 py-1.5 font-medium disabled:opacity-50"
            >
              {submitting ? 'Submitting…' : 'Submit'}
            </button>
          </form>

          {submitNote !== undefined && <p className="mt-2 text-xs text-slate-400">{submitNote}</p>}

          {/* Every field error, not just the first. The API renders them all on
              purpose so a user fixes one form rather than four. */}
          {fieldErrors.length > 0 && (
            <ul className="mt-2 flex flex-col gap-1">
              {fieldErrors.map((fieldError) => (
                <li key={fieldError.pointer} className="text-xs text-red-300">
                  <code>{fieldError.pointer}</code> {fieldError.message}
                </li>
              ))}
            </ul>
          )}
        </section>
      </aside>

      <div className="h-full">
        <CesiumGlobe acquisitions={acquisitions} selectedRequestId={selectedRequestId} />
      </div>
    </main>
  );
}
