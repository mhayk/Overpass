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
  /**
   * Attitude manoeuvre plus settling after the preceding acquisition.
   *
   * The number M4-02 exists to render. The timeline draws it as an OCCUPIED
   * block rather than as idle time, because an idle-looking gap invites "why
   * is the satellite doing nothing?" and the answer is that it is rotating,
   * and it is expensive, and that is the constraint that makes this problem
   * hard.
   *
   * Undefined for the first acquisition in a plan, which has nothing to slew
   * from — distinct from zero, which would mean an instantaneous manoeuvre.
   */
  slewFromPreviousS?: number | undefined;
  /** Idle time before this acquisition, after any slew. */
  gapFromPreviousS?: number | undefined;
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

/**
 * Why a request did not get an acquisition, as STRUCTURED DATA.
 *
 * The contract's own note is the reason this is not a message string: strings
 * cannot be aggregated, charted, or acted on, and a bid suggestion is only
 * possible if the shortfall is a number.
 *
 * Every field here is produced by the planner and passed through untouched.
 * Nothing is recomputed in the browser, which is what makes the explanation a
 * customer sees the same one the planner actually made.
 */
export interface Unfulfilment {
  reasonCode: string;
  /** How many rounds this request has lost. Input to the fairness ageing. */
  ageRounds?: number | undefined;
  /** Whether it stays in contention for later rounds. */
  eligibleForRetry?: boolean | undefined;
  /** How much more the bid needed to be. LOST_TO_HIGHER_VALUE. */
  shortfallCredits?: number | undefined;
  /** BLOCKED_BY_SLEW_CONSTRAINT: the manoeuvre against the room for it. */
  requiredSlewS?: number | undefined;
  availableGapS?: number | undefined;
  /** DUTY_CYCLE_EXHAUSTED: the budget against what this pass needed. */
  dutyCycleRemainingS?: number | undefined;
  dutyCycleRequiredS?: number | undefined;
  supersededByPlanId?: string | undefined;
  /** The strongest candidate this request had, for the ghost highlight. */
  bestRejectedOpportunityId?: string | undefined;
}

export interface RequestView {
  requestId: string;
  customerId: string;
  targetName: string;
  state: string;
  window: TimeRange;
  opportunityCount: number;
  /** Absent when the request was not refused — most of them. */
  unfulfilment?: Unfulfilment | undefined;
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
  slew_time_from_previous_s?: number;
  gap_from_previous_s?: number;
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
    slewFromPreviousS: raw.slew_time_from_previous_s,
    gapFromPreviousS: raw.gap_from_previous_s,
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
    // The EVENT's shape, nested explanation and all. The gateway serves the
    // projected event body verbatim; an earlier version of this client read a
    // flat shape the service has never produced, which #201 corrected in the
    // contract. Every field below was checked against a live response rather
    // than against the schema.
    unfulfilment?: {
      reason_code: string;
      age_rounds?: number;
      eligible_for_retry?: boolean;
      explanation?: {
        shortfall_credits?: number;
        required_slew_s?: number;
        available_gap_s?: number;
        duty_cycle_remaining_s?: number;
        duty_cycle_required_s?: number;
        superseded_by_plan_id?: string;
        best_rejected_opportunity_id?: string;
      };
    };
    staleness?: RawStaleness;
  }>(`/v1/requests/${encodeURIComponent(requestId)}`);

  return {
    requestId: body.request_id,
    customerId: body.customer_id,
    targetName: body.target_name,
    state: body.state,
    window: body.window,
    opportunityCount: body.opportunity_count,
    unfulfilment: body.unfulfilment
      ? {
          reasonCode: body.unfulfilment.reason_code,
          ageRounds: body.unfulfilment.age_rounds,
          eligibleForRetry: body.unfulfilment.eligible_for_retry,
          shortfallCredits: body.unfulfilment.explanation?.shortfall_credits,
          requiredSlewS: body.unfulfilment.explanation?.required_slew_s,
          availableGapS: body.unfulfilment.explanation?.available_gap_s,
          dutyCycleRemainingS: body.unfulfilment.explanation?.duty_cycle_remaining_s,
          dutyCycleRequiredS: body.unfulfilment.explanation?.duty_cycle_required_s,
          supersededByPlanId: body.unfulfilment.explanation?.superseded_by_plan_id,
          bestRejectedOpportunityId:
            body.unfulfilment.explanation?.best_rejected_opportunity_id,
        }
      : undefined,
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

/** A GeoJSON feature whose properties this client knows the shape of. */
export interface TypedFeature<P> {
  type: 'Feature';
  id?: string;
  geometry: unknown;
  properties: P;
}

/** One request's target — the DEMAND side of the map. */
export interface TargetProperties {
  request_id: string;
  customer_id: string;
  target_name?: string;
  state: string;
  window: { start: string; end: string };
  opportunity_count: number;
}

/** One candidate footprint — the CONTENTION side. */
export interface OpportunityProperties {
  opportunity_id: string;
  request_id: string;
  satellite_id: string;
  mode: string;
  window_start: string;
  window_end: string;
  quality_score?: number;
  awarded: boolean;
}

export interface GeoCollection<P> {
  features: TypedFeature<P>[];
  truncated: boolean;
  staleness: Staleness;
}

async function getCollection<P>(path: string): Promise<GeoCollection<P>> {
  const body = await getJson<{
    features?: TypedFeature<P>[];
    truncated?: boolean;
    staleness?: RawStaleness;
  }>(path);
  return {
    features: body.features ?? [],
    // TRUE when absent, the same safe direction fetchFootprints takes: a
    // viewport that wrongly believes it has everything draws emptiness over a
    // region that is merely unread.
    truncated: body.truncated ?? true,
    staleness: toStaleness(body.staleness),
  };
}

/**
 * Request targets over a window — what customers ASKED for.
 *
 * The half `fetchFootprints` structurally cannot show: footprints are
 * committed acquisitions, so a region thick with requests and empty of
 * acquisitions looks exactly like a region nobody asked about. Those are
 * opposite situations and the density layer exists to tell them apart.
 */
export async function fetchTargets(
  window: TimeRange,
  state?: string,
): Promise<GeoCollection<TargetProperties>> {
  const query = new URLSearchParams({
    window_start: window.start,
    window_end: window.end,
  });
  if (state) query.set('state', state);
  return getCollection<TargetProperties>(`/v1/geo/targets?${query.toString()}`);
}

/**
 * Candidate footprints over a window, WON AND LOST.
 *
 * `awarded: false` means the candidate lost, not that its request went
 * unserved — the same request may have won on another pass. So a density of
 * unawarded footprints measures contention for GROUND, which is what the
 * conflict layer claims to show, and not customer disappointment, which it
 * does not.
 */
export async function fetchOpportunityFootprints(
  window: TimeRange,
  awarded?: boolean,
): Promise<GeoCollection<OpportunityProperties>> {
  const query = new URLSearchParams({
    window_start: window.start,
    window_end: window.end,
  });
  if (awarded !== undefined) query.set('awarded', String(awarded));
  return getCollection<OpportunityProperties>(`/v1/geo/opportunities?${query.toString()}`);
}

/**
 * The constellation's orbit tracks, as a CZML document.
 *
 * Returned as an opaque array rather than parsed. CZML is Cesium's format and
 * Cesium's loader is the only thing that should interpret it — re-modelling a
 * packet stream in TypeScript would be a second implementation of a format the
 * server already renders from one read model, which is the exact duplication
 * ADR-0009 exists to prevent.
 *
 * Independent of any plan. The per-plan document draws the orbit of the
 * satellite that plan belongs to; the constellation exists before the first
 * plan is committed, and a globe that only shows satellites once something has
 * been scheduled tells the viewer something false.
 */
export async function fetchConstellationCZML(window: TimeRange): Promise<unknown[]> {
  const query = new URLSearchParams({
    window_start: window.start,
    window_end: window.end,
  });
  const body = await getJson<unknown[]>(`/v1/geo/satellites/czml?${query.toString()}`);
  return Array.isArray(body) ? body : [];
}
