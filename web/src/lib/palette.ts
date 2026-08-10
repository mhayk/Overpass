/**
 * The planning view's colours, and the rules that produced them.
 *
 * M4-01 makes this an acceptance criterion rather than a preference: "colour
 * choices must survive being read by someone who cannot distinguish red from
 * green. A planning tool that fails for a colour-blind operator is broken, not
 * imperfect."
 *
 * So these were VALIDATED, not chosen by eye. Running the data-viz validator
 * against the dark surface these render on, with the all-pairs pairlist a map
 * needs — every mark can sit beside every other, unlike a bar chart where only
 * adjacent pairs meet:
 *
 *   [PASS] Lightness band       all 3 inside L 0.48–0.67
 *   [PASS] Chroma floor         all 3 >= 0.1
 *   [PASS] CVD separation       worst #199e70↔#d95926 ΔE 9.4 (deutan) · tritan 4.0
 *   [PASS] Normal-vision floor  worst #199e70↔#3987e5 ΔE 20.9
 *   [PASS] Contrast vs surface  all 3 >= 3:1
 *
 * THE THREE-SLOT CAP IS WHY MODE IS THE CATEGORICAL CHANNEL AND SATELLITE IS
 * NOT. Under the all-pairs rule only the first three categorical slots clear
 * the separation floors; a fourth puts yellow beside orange and fails. There
 * are exactly three imaging modes and nine satellites, so mode gets the colour
 * and satellite gets filtering and the tooltip. Colouring nine satellites would
 * have meant inventing hues nobody can tell apart — which is the failure this
 * criterion names.
 */

/** The dark chart surface these were validated against. */
export const SURFACE = '#1a1a19';

/**
 * Imaging mode → colour. Categorical slots 1–3, in the validated order.
 *
 * Fixed assignment, never cycled: a filter that hides SCAN must not repaint
 * STRIPMAP. Colour follows the entity, not its rank among whatever survived.
 */
export const MODE_COLOUR: Record<string, [number, number, number]> = {
  SPOTLIGHT: [57, 135, 229], // #3987e5 blue
  STRIPMAP: [217, 89, 38], // #d95926 orange
  SCAN: [25, 158, 112], // #199e70 aqua
};

/** Anything the contract adds later, until it is given a slot deliberately. */
export const MODE_FALLBACK: [number, number, number] = [140, 140, 134];

/**
 * Coverage density → the blue sequential ramp, light to dark.
 *
 * One hue, monotonic in lightness. Never a rainbow: a rainbow ramp has no
 * order a reader can recover, and under CVD it folds into bands that read as
 * boundaries in the data rather than in the palette.
 */
export const COVERAGE_RAMP: [number, number, number][] = [
  [205, 226, 251], // 100
  [158, 197, 244], // 200
  [109, 167, 236], // 300
  [57, 135, 229], // 400
  [37, 106, 191], // 500
  [24, 79, 149], // 600
  [13, 54, 107], // 700
];

/**
 * Contention density → the ORANGE sequential ramp.
 *
 * A second sequential context on the same map takes the next categorical
 * slot's hue as its own one-hue ramp, so the two aggregations stay
 * distinguishable when both are on. Two blue ramps would be two magnitudes
 * nobody can separate.
 *
 * Orange also happens to read as heat, which is right here: this layer is
 * "where is the system fighting itself".
 */
export const CONTENTION_RAMP: [number, number, number][] = [
  [251, 222, 209],
  [246, 190, 165],
  [241, 154, 116],
  [217, 89, 38],
  [180, 71, 28],
  [140, 54, 20],
  [99, 37, 13],
];

/**
 * Demand targets. Deliberately NOT a categorical slot.
 *
 * Targets are a different KIND of thing from footprints — a request rather
 * than an acquisition — so they are encoded by shape and a neutral ink instead
 * of competing for a hue. Giving them slot 4 would have put them in the pair
 * that fails the all-pairs floor, and would have implied they belong to the
 * same series family as the modes.
 */
export const TARGET_COLOUR: [number, number, number] = [195, 194, 183];

/** Text and chrome, from the same dark-surface tokens. */
export const INK = {
  primary: '#ffffff',
  secondary: '#c3c2b7',
  muted: '#8c8c86',
  grid: 'rgba(195,194,183,0.14)',
} as const;

/** Pick a ramp step for a 0..1 magnitude. */
export function rampStep(
  ramp: [number, number, number][],
  fraction: number,
): [number, number, number] {
  // The fallback is the FIRST step rather than a neutral grey: a ramp is an
  // ordered scale, and a value outside it belongs at an end, not off it.
  const first = ramp[0] ?? MODE_FALLBACK;
  if (!Number.isFinite(fraction) || ramp.length === 0) return first;
  const clamped = Math.min(1, Math.max(0, fraction));
  const index = Math.min(ramp.length - 1, Math.floor(clamped * ramp.length));
  return ramp[index] ?? first;
}

/** `rgb()` for CSS, from the same arrays deck.gl consumes. */
export function css(colour: [number, number, number], alpha = 1): string {
  const [r, g, b] = colour;
  return alpha === 1 ? `rgb(${r},${g},${b})` : `rgba(${r},${g},${b},${alpha})`;
}
