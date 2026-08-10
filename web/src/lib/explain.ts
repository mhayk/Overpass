/**
 * Turning an unfulfilment into a sentence a customer can act on.
 *
 * EVERY NUMBER COMES FROM THE PLANNER. Nothing here recomputes anything — the
 * explanation shown is exactly the explanation the planner produced, which is
 * what makes it trustworthy rather than a plausible story assembled in a
 * browser.
 *
 * Suggestions only where genuinely actionable. Telling a customer to raise
 * their bid when the real problem is that no satellite can see their target is
 * worse than saying nothing: it sends them to spend money on a constraint
 * money does not touch.
 */

import type { Unfulfilment } from '@/lib/gateway';

export interface Explanation {
  /** One sentence stating what bound. */
  summary: string;
  /** What to change, when there is something. Absent is a real answer. */
  suggestion?: string;
}

export function explain(unfulfilment: Unfulfilment | undefined): Explanation | undefined {
  if (!unfulfilment) return undefined;

  switch (unfulfilment.reasonCode) {
    case 'LOST_TO_HIGHER_VALUE': {
      const shortfall = unfulfilment.shortfallCredits;
      return {
        summary:
          shortfall === undefined
            ? 'The window was free, but another request was worth more.'
            : `Outbid by ${shortfall.toLocaleString()} credits of effective value.`,
        // The one case where money is genuinely the lever.
        suggestion:
          shortfall === undefined
            ? 'Raise the bid, or widen the window so it competes in more rounds.'
            : `Bid ${shortfall.toLocaleString()} more, or widen the window so it competes elsewhere.`,
      };
    }

    case 'BLOCKED_BY_SLEW_CONSTRAINT': {
      const required = unfulfilment.requiredSlewS;
      const available = unfulfilment.availableGapS;
      const numbers =
        required !== undefined && available !== undefined
          ? ` Reaching it needed ${required.toFixed(1)}s of slew and only ${available.toFixed(1)}s were free.`
          : '';
      return {
        summary: `The pass was free, but the satellite could not turn to it in time.${numbers}`,
        // NOT "bid more". Money does not buy angular acceleration, and saying
        // so would send the customer to spend on the wrong constraint.
        suggestion: 'Widen the window, or accept more modes so a gentler look angle qualifies.',
      };
    }

    case 'DUTY_CYCLE_EXHAUSTED': {
      const remaining = unfulfilment.dutyCycleRemainingS;
      const required = unfulfilment.dutyCycleRequiredS;
      const numbers =
        remaining !== undefined && required !== undefined
          ? ` It needed ${required.toFixed(0)}s and ${remaining.toFixed(0)}s were left.`
          : '';
      return {
        summary: `The satellite's imaging budget for that orbit was already spent.${numbers}`,
        // Not "bid more" either: the knapsack dimension bound, not the
        // interval one, and no amount of money adds seconds to an orbit.
        suggestion: 'Widen the window so the request can fall in a less busy orbit.',
      };
    }

    case 'NO_OPPORTUNITY_IN_BUCKET':
      return {
        summary: 'No pass over this target in that planning bucket. The request is still competing for later ones.',
      };

    case 'DEADLINE_PASSED':
      return {
        summary: 'The window closed before a planning round could place it.',
        suggestion: 'Resubmit with a window that starts further ahead.',
      };

    case 'SUPERSEDED':
      return {
        summary: 'It held a slot in a plan that was replaced, and the new plan did not include it.',
      };

    case 'CANCELLED_BY_CUSTOMER':
      return { summary: 'Withdrawn before allocation.' };

    default:
      // A reason the contract has gained and this has not. Naming the code
      // beats inventing prose for it.
      return { summary: `Not scheduled: ${unfulfilment.reasonCode}.` };
  }
}

/** The summary alone, for a tooltip that has no room for advice. */
export function explainUnfulfilment(unfulfilment: Unfulfilment | undefined): string | undefined {
  return explain(unfulfilment)?.summary;
}
