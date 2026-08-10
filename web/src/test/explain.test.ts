import { describe, expect, it } from 'vitest';

import { explain } from '@/lib/explain';

/**
 * The rule these enforce is the issue's own: "suggestions only where genuinely
 * actionable. Telling a customer to raise their bid when the real problem is
 * that no satellite can see their target is worse than saying nothing."
 */

describe('explain', () => {
  it('turns a shortfall into a number the customer can act on', () => {
    const result = explain({ reasonCode: 'LOST_TO_HIGHER_VALUE', shortfallCredits: 2500 });
    expect(result?.summary).toContain('2,500');
    expect(result?.suggestion).toContain('2,500');
  });

  it('never suggests bidding more when the constraint is physics', () => {
    // Money does not buy angular acceleration. This is the case the issue
    // calls out by name, and the one most tempting to get wrong because
    // "bid more" is a suggestion that fits every slot.
    const slew = explain({
      reasonCode: 'BLOCKED_BY_SLEW_CONSTRAINT',
      requiredSlewS: 42.8,
      availableGapS: 19.5,
    });
    expect(slew?.suggestion ?? '').not.toMatch(/bid/i);

    const duty = explain({ reasonCode: 'DUTY_CYCLE_EXHAUSTED' });
    expect(duty?.suggestion ?? '').not.toMatch(/bid/i);
  });

  it('shows both slew numbers, because the comparison is the explanation', () => {
    const result = explain({
      reasonCode: 'BLOCKED_BY_SLEW_CONSTRAINT',
      requiredSlewS: 42.8,
      availableGapS: 19.5,
    });
    expect(result?.summary).toContain('42.8');
    expect(result?.summary).toContain('19.5');
  });

  it('offers no suggestion where there is nothing to do', () => {
    // A request still competing for later buckets needs no advice, and
    // inventing some would imply the customer had done something wrong.
    expect(explain({ reasonCode: 'NO_OPPORTUNITY_IN_BUCKET' })?.suggestion).toBeUndefined();
    expect(explain({ reasonCode: 'SUPERSEDED' })?.suggestion).toBeUndefined();
    expect(explain({ reasonCode: 'CANCELLED_BY_CUSTOMER' })?.suggestion).toBeUndefined();
  });

  it('covers every reason code the contract defines', () => {
    // A reason the planner can emit but the UI cannot explain is a customer
    // question nobody can answer.
    const codes = [
      'LOST_TO_HIGHER_VALUE',
      'BLOCKED_BY_SLEW_CONSTRAINT',
      'DUTY_CYCLE_EXHAUSTED',
      'DEADLINE_PASSED',
      'NO_OPPORTUNITY_IN_BUCKET',
      'SUPERSEDED',
      'CANCELLED_BY_CUSTOMER',
    ];
    for (const reasonCode of codes) {
      const result = explain({ reasonCode });
      expect(result?.summary, reasonCode).toBeTruthy();
      expect(result?.summary, reasonCode).not.toContain(reasonCode);
    }
  });

  it('names an unknown code rather than inventing prose for it', () => {
    expect(explain({ reasonCode: 'SOMETHING_NEW' })?.summary).toContain('SOMETHING_NEW');
  });

  it('returns nothing for a request that was not refused', () => {
    expect(explain(undefined)).toBeUndefined();
  });
});
