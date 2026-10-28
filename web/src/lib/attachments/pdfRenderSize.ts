/**
 * The canvas budget for one rendered PDF page (TASK-4075, Q4): the device pixel
 * ratio is capped at {@link PDF_MAX_DPR} and the backing store at
 * {@link PDF_MAX_PIXELS}, whatever the page and the zoom ask for. A phone
 * should not be handed a 100-megapixel canvas by a hostile page size or a
 * zoomed-in tap. Each SIDE is also capped at {@link PDF_MAX_SIDE}. The CSS size is NOT capped: a page rendered under the pixel
 * cap is drawn slightly soft rather than refused.
 */
export const PDF_MAX_DPR = 2;
/** 16 megapixels. */
export const PDF_MAX_PIXELS = 16_000_000;
/**
 * The longest backing-store side, in px. An area cap alone admits a 10 x 14400 pt
 * page at fit-width as a one-pixel-wide strip thousands of pixels tall. This is
 * a TEST PIN of our own budget, not a claim about any browser's canvas limit.
 * Scaling is uniform, so the aspect ratio holds.
 */
export const PDF_MAX_SIDE = 16384;

/**
 * pdf.js `maxImageSize`, in pixels (w*h): a decompression-bomb bound on every
 * image EXCEPT JPX (whose size pdf.js re-reads from the codestream), not a
 * memory budget. See the call site in AttachmentPdfPreview.svelte and TASK-4106.
 */
export const PDF_MAX_IMAGE_PIXELS = 64 * 1024 * 1024;

export interface PageRenderSize {
	/** CSS size of the canvas box: the layout the user sees. */
	cssWidth: number;
	cssHeight: number;
	/** Backing-store size: what the canvas allocates. */
	pixelWidth: number;
	pixelHeight: number;
}

/**
 * Returns `null` for input that has no honest size (non-finite or <= 0 page
 * size, CSS width or device pixel ratio): the caller shows "couldn't be drawn"
 * instead of a canvas sized by NaN or Infinity, which would skip both caps.
 */
export function pageRenderSize(
	pageWidth: number,
	pageHeight: number,
	cssWidth: number,
	devicePixelRatio: number
): PageRenderSize | null {
	for (const v of [pageWidth, pageHeight, cssWidth, devicePixelRatio]) {
		if (!Number.isFinite(v) || v <= 0) return null;
	}
	const cssScale = cssWidth / pageWidth;
	const dpr = Math.min(Math.max(devicePixelRatio, 1), PDF_MAX_DPR);
	let pixelScale = cssScale * dpr;
	const area = pageWidth * pixelScale * (pageHeight * pixelScale);
	if (area > PDF_MAX_PIXELS) pixelScale *= Math.sqrt(PDF_MAX_PIXELS / area);
	const longest = Math.max(pageWidth, pageHeight) * pixelScale;
	if (longest > PDF_MAX_SIDE) pixelScale *= PDF_MAX_SIDE / longest;
	// floor, so rounding can never push the product back over the cap.
	const pixelWidth = Math.max(1, Math.floor(pageWidth * pixelScale));
	const pixelHeight = Math.max(1, Math.floor(pageHeight * pixelScale));
	return {
		cssWidth,
		cssHeight: pageHeight * cssScale,
		pixelWidth,
		pixelHeight
	};
}

/** Zoom steps, as a multiple of fit-width. 1 is the default. */
export const PDF_ZOOM_STEPS: readonly number[] = [0.5, 0.75, 1, 1.25, 1.5, 2, 3];

/** The next zoom step in `direction`, clamped at the ends; an off-grid value snaps to the nearest step first. */
export function stepZoom(current: number, direction: 1 | -1): number {
	let at = 0;
	for (let i = 1; i < PDF_ZOOM_STEPS.length; i++) {
		if (Math.abs(PDF_ZOOM_STEPS[i] - current) < Math.abs(PDF_ZOOM_STEPS[at] - current)) at = i;
	}
	return PDF_ZOOM_STEPS[Math.min(PDF_ZOOM_STEPS.length - 1, Math.max(0, at + direction))];
}
