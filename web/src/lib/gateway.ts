/**
 * The read side: plan-gateway.
 *
 * Every response carries a `staleness`, and this client keeps it rather than
 * discarding it. A read model that cannot say how far behind it is forces the
 * UI to guess, and the UI guesses optimistically — showing a plan as current
 * when it has already been superseded. Anything rendered from here is rendered
 * with an age attached.
 */

export interface Staleness {
  /** When the newest folded event happened. */
  asOf: string;
  /** How far behind the read model is, in seconds. */
  lagSeconds: number;
}

export interface TimeRange {
  start: string;
  end: string;
}

export type AcquisitionStatus = 'ACTIVE' | 'EXECUTED' | 'SUPERSEDED';

export interface Acquisition {
  acquisitionId: string;
  requestId: string;
  customerId: string;
  satelliteId: string;
  mode: string;
  window: TimeRange;
  status: AcquisitionStatus;
  /** RFC 7946 geometry, longitude first. Passed to the map untouched. */
  footprint: unknown;
  awardedValueCredits: number;
}

export interface Opportunity {
  opportunityId: string;
  satelliteId: string;
  mode: string;
  accessWindow: TimeRange;
  qualityScore: number;
  footprint: unknown;
  /**
   * Whether this candidate was scheduled.
   *
   * The losers are the point. A winner shown without them explains nothing
   * about why it won, and the ghost rendering in M4-05 is built on this field.
   */
  won: boolean;
}

export interface RequestView {
  requestId: string;
  customerId: string;
  targetName: string;
  state: string;
  window: TimeRange;
  opportunityCount: number;
  staleness: Staleness;
}

export interface Collection<T> {
  items: T[];
  staleness: Staleness;
}

/**
 * A read that failed, distinguished from a read that found nothing.
 *
 * "There is no plan" and "I cannot tell you whether there is a plan" are
 * different answers and the UI must not conflate them: one is an empty state,
 * the other is an error banner. The gateway is careful to keep them apart
 * (404 versus 503) and it would be a waste to flatten them here.
 */
export class GatewayError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly detail?: string,
  ) {
    super(message);
    this.name = 'GatewayError';
  }

  /**
   * Whether retrying could plausibly succeed.
   *
   * Status 0 counts, and getting that wrong is what this comment is for: a
   * network failure never reaches the server, so it has no status, and
   * `status >= 500` classified the ONE case that most deserves a retry as
   * permanent. The UI would have shown "this does not exist" for a gateway
   * that was merely not running yet.
   */
  get transient(): boolean {
    return this.status === 0 || this.status >= 500;
  }
}

export const DEFAULT_GATEWAY_URL = 'http://localhost:8083';

function baseUrl(): string {
  return process.env.NEXT_PUBLIC_GATEWAY_URL ?? DEFAULT_GATEWAY_URL;
}

interface RawStaleness {
  as_of?: string;
  lag_seconds?: number;
}

function toStaleness(raw: RawStaleness | undefined): Staleness {
  return {
    asOf: raw?.as_of ?? '',
    // Absent staleness is reported as unknown rather than zero. Zero means
    // "perfectly current", which is a much stronger claim than "the server did
    // not say", and it is the claim a reader would act on.
    lagSeconds: raw?.lag_seconds ?? Number.NaN,
  };
}

async function getJson<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${baseUrl()}${path}`, {
      ...init,
      headers: { Accept: 'application/json', ...(init?.headers ?? {}) },
    });
  } catch (cause) {
    // A network failure is not a 5xx, but it is transient in the same way, and
    // the UI treats both as "try again" rather than "this does not exist".
    throw new GatewayError(0, 'The read model could not be reached', String(cause));
  }

  if (!response.ok) {
    const problem = (await response.json().catch(() => ({}))) as {
      title?: string;
      detail?: string;
    };
    throw new GatewayError(
      response.status,
      problem.title ?? `Request failed with ${response.status}`,
      problem.detail,
    );
  }
  return (await response.json()) as T;
}

interface RawAcquisition {
  acquisition_id: string;
  request_id: string;
  customer_id: string;
  satellite_id: string;
  mode: string;
  window: TimeRange;
  status: AcquisitionStatus;
  footprint: unknown;
  awarded_value_credits: number;
}

function toAcquisition(raw: RawAcquisition): Acquisition {
  return {
    acquisitionId: raw.acquisition_id,
    requestId: raw.request_id,
    customerId: raw.customer_id,
    satelliteId: raw.satellite_id,
    mode: raw.mode,
    window: raw.window,
    status: raw.status,
    footprint: raw.footprint,
    awardedValueCredits: raw.awarded_value_credits,
  };
}

interface RawOpportunity {
  opportunity_id: string;
  satellite_id: string;
  mode: string;
  access_window: TimeRange;
  quality_score: number;
  footprint: unknown;
  won: boolean;
}

function toOpportunity(raw: RawOpportunity): Opportunity {
  return {
    opportunityId: raw.opportunity_id,
    satelliteId: raw.satellite_id,
    mode: raw.mode,
    accessWindow: raw.access_window,
    qualityScore: raw.quality_score,
    footprint: raw.footprint,
    won: raw.won,
  };
}

/** Acquisitions overlapping a window. */
export async function fetchAcquisitions(window: TimeRange): Promise<Collection<Acquisition>> {
  const query = new URLSearchParams({
    window_start: window.start,
    window_end: window.end,
  });
  const body = await getJson<{ items?: RawAcquisition[]; staleness?: RawStaleness }>(
    `/v1/acquisitions?${query.toString()}`,
  );
  return {
    // `?? []` rather than trusting the field. The gateway is careful to emit an
    // empty array rather than null — it has a test for exactly that — but a
    // client that throws on one missing key takes the whole page down.
    items: (body.items ?? []).map(toAcquisition),
    staleness: toStaleness(body.staleness),
  };
}

/** Every candidate for a request, won and lost. */
export async function fetchOpportunities(requestId: string): Promise<Collection<Opportunity>> {
  const body = await getJson<{ items?: RawOpportunity[]; staleness?: RawStaleness }>(
    `/v1/requests/${encodeURIComponent(requestId)}/opportunities`,
  );
  return {
    items: (body.items ?? []).map(toOpportunity),
    staleness: toStaleness(body.staleness),
  };
}

/** One request's projected view. */
export async function fetchRequest(requestId: string): Promise<RequestView> {
  const body = await getJson<{
    request_id: string;
    customer_id: string;
    target_name: string;
    state: string;
    window: TimeRange;
    opportunity_count: number;
    staleness?: RawStaleness;
  }>(`/v1/requests/${encodeURIComponent(requestId)}`);

  return {
    requestId: body.request_id,
    customerId: body.customer_id,
    targetName: body.target_name,
    state: body.state,
    window: body.window,
    opportunityCount: body.opportunity_count,
    staleness: toStaleness(body.staleness),
  };
}

/** The GeoJSON footprint collection deck.gl and the globe both read. */
export async function fetchFootprints(window: TimeRange): Promise<{
  features: unknown[];
  truncated: boolean;
  staleness: Staleness;
}> {
  const query = new URLSearchParams({
    window_start: window.start,
    window_end: window.end,
  });
  const body = await getJson<{
    features?: unknown[];
    truncated?: boolean;
    staleness?: RawStaleness;
  }>(`/v1/geo/footprints?${query.toString()}`);

  return {
    features: body.features ?? [],
    // Defaults to TRUE when absent, which is the safe direction: a viewport
    // that wrongly believes it has everything draws a coverage gap where there
    // is really a limit, and a reader cannot tell the difference.
    truncated: body.truncated ?? true,
    staleness: toStaleness(body.staleness),
  };
}
