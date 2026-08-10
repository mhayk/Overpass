/**
 * The live stream, as a subscription a component can hold.
 *
 * COALESCED BEFORE IT REACHES REACT. A planning round commits many changes at
 * once; handing each one to setState turns one logical update into dozens of
 * renders. The server already batches over 100ms, and this batches again on the
 * way in, because a burst that survives the network is still a burst.
 *
 * The payloads are thin by contract: an event names what changed and the
 * subscriber refetches. That is the whole design — a stream carrying entities
 * would be a second API to keep in step with the first.
 */

export interface LiveChange {
  kind: 'request' | 'plan' | 'reset';
  requestId?: string;
  planId?: string;
  satelliteId?: string;
}

export interface LiveOptions {
  gatewayUrl: string;
  requestId?: string | undefined;
  satelliteId?: string | undefined;
  /** How long to gather changes before delivering them. */
  coalesceMs?: number;
  onChanges: (changes: LiveChange[]) => void;
}

/**
 * Open the stream. Returns a function that closes it.
 *
 * Reconnection is EventSource's own: the browser retries and replays
 * Last-Event-ID without being asked, which is the reason M4-03 chose SSE over
 * WebSockets in the first place. Writing a reconnect loop here would be
 * re-implementing the thing the protocol was picked for.
 */
export function subscribe(options: LiveOptions): () => void {
  const { gatewayUrl, requestId, satelliteId, coalesceMs = 150, onChanges } = options;

  const query = new URLSearchParams();
  if (requestId) query.set('request_id', requestId);
  if (satelliteId) query.set('satellite_id', satelliteId);
  const url = `${gatewayUrl}/v1/events${query.size > 0 ? `?${query.toString()}` : ''}`;

  const source = new EventSource(url);
  let pending: LiveChange[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;

  const flush = (): void => {
    timer = undefined;
    if (pending.length === 0) return;
    const batch = pending;
    pending = [];
    onChanges(batch);
  };

  const push = (change: LiveChange): void => {
    pending.push(change);
    timer ??= setTimeout(flush, coalesceMs);
  };

  source.addEventListener('request', (event) => {
    const data = parse(event);
    if (data) push({ kind: 'request', requestId: String(data.request_id ?? '') });
  });

  source.addEventListener('plan', (event) => {
    const data = parse(event);
    if (data) {
      push({
        kind: 'plan',
        planId: String(data.plan_id ?? ''),
        satelliteId: String(data.satellite_id ?? ''),
      });
    }
  });

  // A reset is delivered IMMEDIATELY rather than coalesced. It means "you may
  // have missed something", and holding it for 150ms alongside changes it
  // cannot vouch for would let a subscriber act on a partial picture first.
  source.addEventListener('reset', () => {
    pending = [];
    if (timer) {
      clearTimeout(timer);
      timer = undefined;
    }
    onChanges([{ kind: 'reset' }]);
  });

  return () => {
    if (timer) clearTimeout(timer);
    source.close();
  };
}

function parse(event: MessageEvent): Record<string, unknown> | undefined {
  try {
    return JSON.parse(event.data as string) as Record<string, unknown>;
  } catch {
    // A frame this client cannot read is not a reason to tear down the
    // stream: the next one is probably fine, and a dropped connection costs
    // more than a dropped event.
    return undefined;
  }
}
