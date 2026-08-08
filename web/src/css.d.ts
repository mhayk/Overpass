/**
 * Ambient declarations for side-effect CSS imports.
 *
 * Cesium's widget stylesheet is imported for its effect, not its value.
 * TypeScript has no notion of a CSS module, so without this the globe's dynamic
 * import fails to type-check — while working perfectly at runtime, which is the
 * confusing direction.
 */
declare module '*.css';
