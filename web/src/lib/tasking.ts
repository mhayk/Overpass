/**
 * The write side: tasking-api.
 *
 * Submission is idempotent and the key is the caller's responsibility. This
 * client derives one from the body, which is what makes a double-click harmless
 * — the same body under the same key replays instead of creating a second
 * request. A random key per attempt would turn an impatient user into two
 * tasking requests, and the API cannot tell those apart from two genuine ones.
 */

export type PriorityTier = 'GOVERNMENT' | 'CIVIL_PROTECTION' | 'COMMERCIAL' | 'BEST_EFFORT';
export type ImagingMode = 'SPOTLIGHT' | 'STRIPMAP' | 'SCAN';

export interface SubmitRequest {
  customerId: string;
  targetName: string;
  /** GeoJSON Point or Polygon, longitude first. */
  target: unknown;
  windowStart: string;
  windowEnd: string;
  priorityTier: PriorityTier;
  bidCredits: number;
  requestedModes: ImagingMode[];
}

export interface SubmitResult {
  requestId: string;
  state: string;
  /** True when the API recognised this exact submission and did not repeat it. */
  replayed: boolean;
}

export interface FieldError {
  /** RFC 6901 JSON Pointer into the submitted body. */
  pointer: string;
  message: string;
}

/** A submission the API refused, with per-field detail the form can render. */
export class SubmitRejected extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly fieldErrors: FieldError[] = [],
  ) {
    super(message);
    this.name = 'SubmitRejected';
  }
}

export const DEFAULT_TASKING_URL = 'http://localhost:8080';

function baseUrl(): string {
  return process.env.NEXT_PUBLIC_TASKING_API_URL ?? DEFAULT_TASKING_URL;
}

/**
 * A UUIDv5-shaped key derived from the body.
 *
 * Not crypto — a stable digest is all this needs, and SubtleCrypto is async and
 * unavailable over plain HTTP on some hosts, which is a poor reason for a form
 * to stop working. What matters is that identical bodies produce identical keys
 * and different bodies do not.
 */
export function idempotencyKey(body: unknown): string {
  const canonical = JSON.stringify(body);
  // FNV-1a over four offset streams, to fill 128 bits without a dependency.
  const hashes = [0x811c9dc5, 0x01000193, 0xdeadbeef, 0xcafebabe].map((seed) => {
    let hash = seed >>> 0;
    for (let i = 0; i < canonical.length; i += 1) {
      hash ^= canonical.charCodeAt(i);
      hash = Math.imul(hash, 0x01000193) >>> 0;
    }
    return hash.toString(16).padStart(8, '0');
  });
  const hex = hashes.join('');
  // Version 4 and variant bits, so the API's `format: uuid` check accepts it.
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    `4${hex.slice(13, 16)}`,
    `8${hex.slice(17, 20)}`,
    hex.slice(20, 32),
  ].join('-');
}

interface ProblemDocument {
  title?: string;
  detail?: string;
  errors?: { pointer?: string; message?: string }[];
}

export async function submitRequest(request: SubmitRequest): Promise<SubmitResult> {
  const body = {
    customer_id: request.customerId,
    target_name: request.targetName,
    target: request.target,
    window: { start: request.windowStart, end: request.windowEnd },
    priority_tier: request.priorityTier,
    bid_credits: request.bidCredits,
    requested_modes: request.requestedModes,
  };

  const response = await fetch(`${baseUrl()}/v1/tasking-requests`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Idempotency-Key': idempotencyKey(body),
    },
    body: JSON.stringify(body),
  });

  if (!response.ok) {
    const problem = (await response.json().catch(() => ({}))) as ProblemDocument;
    throw new SubmitRejected(
      response.status,
      problem.detail ?? problem.title ?? `Submission failed with ${response.status}`,
      (problem.errors ?? []).map((error) => ({
        pointer: error.pointer ?? '',
        message: error.message ?? '',
      })),
    );
  }

  const accepted = (await response.json()) as { request_id: string; state?: string };
  return {
    requestId: accepted.request_id,
    state: accepted.state ?? 'RECEIVED',
    // A HEADER, not a status code. A replay and a new acceptance are both 202,
    // because from the caller's point of view the outcome is identical: the
    // request is accepted and will be planned exactly once.
    replayed: response.headers.get('Idempotency-Replayed') === 'true',
  };
}
