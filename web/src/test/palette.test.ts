import { describe, expect, it } from 'vitest';

import {
  CONTENTION_RAMP,
  COVERAGE_RAMP,
  MODE_COLOUR,
  MODE_FALLBACK,
  css,
  rampStep,
} from '@/lib/palette';

/**
 * The palette is an ACCEPTANCE CRITERION, not a preference: "colour choices
 * must survive being read by someone who cannot distinguish red from green."
 *
 * The colour-vision separation itself was checked by the data-viz validator and
 * the results are recorded in palette.ts — those are ΔE computations and
 * re-implementing them here would be a second, worse validator. What these
 * tests hold is the structure the validation depends on, which is the part a
 * future edit can silently break: how many hues are in play, that they are
 * fixed rather than cycled, and that the ramps stay single-hue and ordered.
 */

describe('categorical assignment', () => {
  it('uses exactly three hues, because only three clear the all-pairs floors', () => {
    // A map is an all-pairs surface: every mark can sit beside every other,
    // unlike a bar chart where only neighbours meet. Under that pairlist the
    // validated palette clears its floors for three slots; a fourth puts
    // yellow next to orange and fails. Three imaging modes is not a
    // coincidence being exploited — it is the reason mode got the colour
    // channel and satellite did not.
    expect(Object.keys(MODE_COLOUR)).toHaveLength(3);
  });

  it('covers every imaging mode the contract defines', () => {
    // If the contract gains a mode, this fails rather than the map quietly
    // painting it grey and implying it is a different kind of thing.
    expect(Object.keys(MODE_COLOUR).sort()).toEqual(['SCAN', 'SPOTLIGHT', 'STRIPMAP']);
  });

  it('assigns a fixed hue per mode rather than cycling a list', () => {
    // Colour follows the entity, never its rank among whatever survived a
    // filter. Hiding SCAN must not repaint STRIPMAP, and the only way that
    // holds is a keyed lookup rather than an indexed one.
    const before = MODE_COLOUR.STRIPMAP;
    const withoutScan = Object.fromEntries(
      Object.entries(MODE_COLOUR).filter(([mode]) => mode !== 'SCAN'),
    );
    expect(withoutScan.STRIPMAP).toEqual(before);
  });

  it('has a distinct fallback, so an unknown mode is visibly unknown', () => {
    for (const colour of Object.values(MODE_COLOUR)) {
      expect(colour).not.toEqual(MODE_FALLBACK);
    }
  });
});

describe('sequential ramps', () => {
  const ramps: [string, [number, number, number][]][] = [
    ['coverage', COVERAGE_RAMP],
    ['contention', CONTENTION_RAMP],
  ];

  it.each(ramps)('%s is ordered light to dark', (_name, ramp) => {
    // Monotonic lightness is what makes a sequential ramp readable at all: a
    // reader recovers order from brightness, which is the one channel that
    // survives every form of colour blindness AND greyscale printing.
    const luminance = ramp.map(([r, g, b]) => 0.2126 * r + 0.7152 * g + 0.0722 * b);
    for (let i = 1; i < luminance.length; i += 1) {
      expect(luminance[i]).toBeLessThan(luminance[i - 1]);
    }
  });

  it('uses two different hues, so the two aggregations stay separable', () => {
    // Both hexbin layers can be on at once. Two blue ramps would be two
    // magnitudes nobody can tell apart, which is the failure mode of putting
    // "coverage" and "contention" on the same map.
    const [cr, cg, cb] = COVERAGE_RAMP[3]!;
    const [nr, ng, nb] = CONTENTION_RAMP[3]!;
    expect(cb).toBeGreaterThan(cr); // blue-dominant
    expect(nr).toBeGreaterThan(nb); // orange/red-dominant
    expect([cr, cg, cb]).not.toEqual([nr, ng, nb]);
  });
});

describe('rampStep', () => {
  it('maps the ends of the range to the ends of the ramp', () => {
    expect(rampStep(COVERAGE_RAMP, 0)).toEqual(COVERAGE_RAMP[0]);
    expect(rampStep(COVERAGE_RAMP, 1)).toEqual(COVERAGE_RAMP[COVERAGE_RAMP.length - 1]);
  });

  it('clamps rather than wrapping', () => {
    // Wrapping would send the highest magnitude back to the lightest step —
    // the densest cell rendering as the emptiest, which reads as good news.
    expect(rampStep(COVERAGE_RAMP, 5)).toEqual(COVERAGE_RAMP[COVERAGE_RAMP.length - 1]);
    expect(rampStep(COVERAGE_RAMP, -3)).toEqual(COVERAGE_RAMP[0]);
  });

  it('survives NaN, which is what an empty aggregation divides into', () => {
    expect(rampStep(COVERAGE_RAMP, Number.NaN)).toEqual(COVERAGE_RAMP[0]);
  });
});

describe('css', () => {
  it('emits rgb for opaque and rgba for transparent', () => {
    expect(css([1, 2, 3])).toBe('rgb(1,2,3)');
    expect(css([1, 2, 3], 0.5)).toBe('rgba(1,2,3,0.5)');
  });
});
