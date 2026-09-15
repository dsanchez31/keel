/**
 * KEEL's type families, as CSS font-family lists.
 *
 * These mirror `--font-sans` and `--font-mono` in src/index.css by hand, and
 * the duplication is deliberate: Cesium takes LabelGraphics.font as a CSS font
 * shorthand string and hands it to the browser's rasterizer, which resolves it
 * against nothing in the document, so it cannot read a custom property. Change
 * index.css, change this.
 *
 * Both faces are self-hosted (@fontsource-variable, imported by index.css) and
 * served from the app's own origin, as the rest of the globe is: no font CDN.
 */

/** Geist Variable: long prose over the monospace base, a plan's or a decision's rationale. */
export const FONT_SANS = "'Geist Variable', ui-sans-serif, system-ui, sans-serif";

/** Geist Mono Variable: the tactical surface, the page's base font, globe labels. */
export const FONT_MONO = "'Geist Mono Variable', ui-monospace, monospace";
