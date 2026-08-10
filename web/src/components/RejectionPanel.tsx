'use client';

/**
 * Why was my request rejected? (M4-04)
 *
 * The UI equivalent of an ADR: the system EXPLAINS its decisions rather than
 * announcing them. Most scheduling systems tell a customer "no"; this one says
 * which constraint bound and by how much.
 *
 * EVERY NUMBER COMES FROM THE PLANNER. The panel reads
 * planning.request.unfulfilled.v1 by way of the read model and recomputes
 * nothing — that is what makes the explanation the customer sees the same one
 * the planner actually made, rather than a plausible story assembled in a
 * browser from partial data.
 */

import { useEffect, useState } from 'react';

import { explain } from '@/lib/explain';
import { fetchRequest, type RequestView } from '@/lib/gateway';

export interface RejectionPanelProps {
  requestId?: string | undefined;
  /** Highlights the request's best rejected candidate on the other views. */
  onHighlight?: ((requestId: string) => void) | undefined;
}

export default function RejectionPanel({
  requestId,
  onHighlight,
}: RejectionPanelProps): React.JSX.Element | null {
  const [request, setRequest] = useState<RequestView | null>(null);
  const [error, setError] = useState<string | undefined>();

  useEffect(() => {
    if (!requestId) {
      setRequest(null);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const view = await fetchRequest(requestId);
        if (!cancelled) {
          setRequest(view);
          setError(undefined);
        }
      } catch (cause) {
        if (!cancelled) {
          setRequest(null);
          setError(cause instanceof Error ? cause.message : String(cause));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [requestId]);

  if (!requestId) return null;

  if (error) {
    return (
      <section className="rounded border border-slate-700 p-3 text-sm">
        <p role="alert" className="text-rose-300">
          Could not load this request: {error}
        </p>
      </section>
    );
  }

  if (!request) {
    return <p className="p-3 text-sm text-slate-400">Loading the explanation…</p>;
  }

  const explanation = explain(request.unfulfilment);

  return (
    <section
      aria-label="Why this request was not scheduled"
      className="rounded border border-slate-700 p-3 text-sm"
    >
      <header className="mb-2">
        <h3 className="font-medium text-slate-100">{request.targetName || 'Request'}</h3>
        <p className="text-xs text-slate-400">
          {request.customerId} · <span className="uppercase">{request.state}</span>
        </p>
      </header>

      {explanation ? (
        <>
          <p className="text-slate-200">{explanation.summary}</p>

          {explanation.suggestion ? (
            <p className="mt-2 rounded bg-slate-800 p-2 text-slate-300">
              <span className="text-slate-500">What would change it: </span>
              {explanation.suggestion}
            </p>
          ) : (
            // Absence is deliberate and worth saying out loud. Inventing advice
            // here would imply the customer did something wrong when the
            // honest answer is that nothing they control was the problem.
            <p className="mt-2 text-xs text-slate-500">
              Nothing to change — this one is not about the request.
            </p>
          )}

          <button
            type="button"
            className="mt-3 text-xs text-sky-300 underline"
            onClick={() => onHighlight?.(request.requestId)}
          >
            Show its candidates on the timeline and map
          </button>
        </>
      ) : (
        <p className="text-slate-300">
          {request.opportunityCount > 0
            ? `Scheduled or still competing — ${request.opportunityCount} opportunities found.`
            : 'No opportunities were found for this request.'}
        </p>
      )}

      <p className="mt-3 text-[11px] text-slate-500">
        Every figure here is the planner&rsquo;s own. Nothing is recalculated in the browser.
      </p>
    </section>
  );
}
