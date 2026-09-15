import { useEffect, useState } from 'react';

/**
 * Whether the browser has the face named by a CSS font shorthand.
 *
 * It exists for one consumer and one failure mode. Cesium bakes a label's
 * glyphs into a WebGL texture atlas when the entity is created, so a webfont
 * landing a moment later never reaches the scene: the fallback is rasterized
 * once and kept, the vector ids stay in the platform's generic font beside
 * Geist Mono everywhere else, and nothing says why. No later re-render fixes
 * it. Gating the label entities on this removes the race.
 *
 * True at once where the API is missing. Under jsdom `document.fonts` is a
 * stub, and a suite waiting on it would hang rather than fail; a browser
 * without FontFaceSet gets its fallback, the answer it would have had with no
 * gate at all.
 *
 * `font` is the shorthand FontFaceSet.load expects, a size and a family, e.g.
 * `12px ${FONT_MONO}`. A bare family name throws a SyntaxError.
 */
export const useFontReady = (font: string): boolean => {
  // `document.fonts` is typed as always present; jsdom and old browsers say
  // otherwise.
  const [isReady, setIsReady] = useState(() => !(document.fonts as FontFaceSet | undefined)?.load);

  useEffect(() => {
    const fonts = document.fonts as FontFaceSet | undefined;
    if (!fonts?.load) return;

    let cancelled = false;
    // Rejects only on a malformed shorthand; a face the browser cannot fetch
    // resolves with an empty list. Either way the answer is to draw with what
    // there is.
    void fonts
      .load(font)
      .catch(() => [])
      .then(() => {
        if (!cancelled) setIsReady(true);
      });

    return () => {
      cancelled = true;
    };
  }, [font]);

  return isReady;
};
