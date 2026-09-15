import { type Grid } from '@keel/sdk';
import {
  GeometryInstance,
  GroundPrimitive,
  Material,
  MaterialAppearance,
  Rectangle,
  RectangleGeometry,
  type Viewer,
} from 'cesium';

import { createFogImage, type FogImage, gridExtent, sameRaster, updateFogImage } from './fogRaster';

/**
 * The fog of war on the globe: one ground-clamped rectangle over the AO
 * raster, textured with the fog image (fogRaster.ts), a primitive of its own
 * apart from the mission's entities (design section 8.2).
 *
 * The texture is one canvas, and Cesium uploads a canvas uniform again only
 * when it is given a different one (Material's texture update compares
 * references), so two canvases take turns: each change of coverage paints
 * the image into the one not showing and hands it over. A new raster (another
 * mission, another AO) builds a new rectangle. Coverage changes at most once
 * a tick, so the fog is repainted at most ten times a second, and only the
 * cells explored since are repainted into the image.
 */
export class FogLayer {
  private readonly viewer: Viewer;
  private readonly canvases = [document.createElement('canvas'), document.createElement('canvas')] as const;
  private front: 0 | 1 = 0;
  private grid: Grid | undefined;
  private image: FogImage | undefined;
  private primitive: GroundPrimitive | undefined;
  private material: Material | undefined;

  constructor(viewer: Viewer) {
    this.viewer = viewer;
  }

  update(grid: Grid): void {
    if (grid === this.grid) return;
    const before = this.grid;
    this.grid = grid;
    if (before && this.image && this.material && sameRaster(before, grid)) {
      if (updateFogImage(this.image, grid, before.explored)) this.show(this.image);
      return;
    }
    this.rebuild(grid);
  }

  destroy(): void {
    this.remove();
  }

  private rebuild(grid: Grid): void {
    this.remove();
    if (grid.cols === 0 || grid.rows === 0) return;
    const { scene } = this.viewer;
    if (!GroundPrimitive.supportsMaterials(scene)) {
      console.warn('this browser cannot drape a texture over the ground: no fog of war');
      return;
    }
    const image = createFogImage(grid);
    const canvas = this.paint(image);
    const material = Material.fromType(Material.ImageType, { image: canvas });
    const { west, south, east, north } = gridExtent(grid);
    this.primitive = scene.groundPrimitives.add(
      new GroundPrimitive({
        geometryInstances: new GeometryInstance({
          geometry: new RectangleGeometry({
            rectangle: Rectangle.fromDegrees(west, south, east, north),
            vertexFormat: MaterialAppearance.MaterialSupport.TEXTURED.vertexFormat,
          }),
        }),
        appearance: new MaterialAppearance({ material, translucent: true }),
        allowPicking: false,
      }),
    );
    this.image = image;
    this.material = material;
    scene.requestRender();
  }

  private show(image: FogImage): void {
    if (!this.material) return;
    this.material.uniforms.image = this.paint(image);
    this.viewer.scene.requestRender();
  }

  // The canvas not showing, painted with the image.
  private paint(image: FogImage): HTMLCanvasElement {
    this.front = this.front === 0 ? 1 : 0;
    const canvas = this.canvases[this.front];
    canvas.width = image.width;
    canvas.height = image.height;
    canvas.getContext('2d')?.putImageData(new ImageData(image.data, image.width, image.height), 0, 0);
    return canvas;
  }

  private remove(): void {
    if (this.primitive) this.viewer.scene.groundPrimitives.remove(this.primitive);
    this.primitive = undefined;
    this.material = undefined;
    this.image = undefined;
  }
}
