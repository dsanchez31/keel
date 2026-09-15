import { type Position } from '@keel/sdk';
import {
  Cartesian2,
  Cartesian3,
  Color,
  ColorMaterialProperty,
  LabelStyle,
  type MaterialProperty,
  PolylineDashMaterialProperty,
  VerticalOrigin,
} from 'cesium';

import { FONT_MONO } from '@/lib/fonts';

import { PALETTE, type Stroke } from './missionLayers';

/**
 * What every scene of the globe draws with: positions, strokes and labels in
 * one style, so the mission and the plan awaiting approval read alike.
 */

/** The font of every label; a component drawing labels waits for it (useFontReady). */
export const LABEL_FONT = `12px ${FONT_MONO}`;

/** Marker edge in pixels. */
export const MARKER_PX = 16;

export const cartesian = (p: Position): Cartesian3 => Cartesian3.fromDegrees(p.lon, p.lat, p.alt_m);
export const cartesians = (ps: readonly Position[]): Cartesian3[] => ps.map(cartesian);

export function material(stroke: Stroke): MaterialProperty {
  const color = Color.fromCssColorString(stroke.color).withAlpha(stroke.alpha);
  return stroke.dashed ? new PolylineDashMaterialProperty({ color, dashLength: 12 }) : new ColorMaterialProperty(color);
}

/** A label under its marker, pale on a black outline. */
export function label(text: string) {
  return {
    text,
    font: LABEL_FONT,
    fillColor: Color.fromCssColorString(PALETTE.pale),
    outlineColor: Color.BLACK,
    outlineWidth: 3,
    style: LabelStyle.FILL_AND_OUTLINE,
    verticalOrigin: VerticalOrigin.TOP,
    pixelOffset: new Cartesian2(0, MARKER_PX / 2 + 2),
  };
}
