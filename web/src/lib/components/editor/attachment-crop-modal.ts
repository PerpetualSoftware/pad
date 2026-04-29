/**
 * Crop modal for AttachmentImage (TASK-880).
 *
 * Opens a centered <dialog> showing the full-resolution attachment
 * with an overlaid crop rectangle the user can drag (to move) or
 * resize via four corner handles. Aspect-preset toolbar (Free / 1:1 /
 * 4:3 / 16:9) snaps the rectangle to the chosen ratio while
 * preserving the rectangle's center.
 *
 * Returns a Promise that resolves to the crop rectangle in
 * ORIGINAL-IMAGE pixel coordinates (origin top-left, integer pixels)
 * when the user clicks Apply, or `null` when they Cancel / dismiss.
 *
 * The modal is pure DOM — no Svelte component, no extra deps. We
 * deliberately avoid `svelte-easy-crop` and friends to keep the
 * editor's bundle slim and the Svelte 5 migration story simple.
 *
 * Coordinate spaces:
 *   - "preview-pixel" coords are relative to the displayed <img>
 *     element's bounding box, which is fit-to-modal scaled.
 *   - "natural-pixel" coords are the image's source dimensions
 *     (image.naturalWidth × naturalHeight).
 *   The translation factor is naturalDim / displayedDim, applied
 *   per-axis so non-uniform scale (rare with object-fit: contain
 *   but possible if the modal is portrait and the image landscape)
 *   works correctly.
 */

export interface CropResult {
	/** x in original-image pixels, origin top-left. */
	x: number;
	y: number;
	w: number;
	h: number;
}

export interface OpenCropModalOptions {
	/** URL of the full-resolution image to crop. */
	imageUrl: string;
	/** Optional alt text for the modal's <img>. */
	alt?: string;
}

/** Aspect ratio choices the toolbar exposes. `null` = free. */
type AspectChoice = number | null;

const ASPECT_PRESETS: { label: string; value: AspectChoice }[] = [
	{ label: 'Free', value: null },
	{ label: '1:1', value: 1 },
	{ label: '4:3', value: 4 / 3 },
	{ label: '16:9', value: 16 / 9 },
];

/**
 * Display the crop modal and return a promise that resolves to the
 * cropped rectangle (or null on cancel). The Promise never rejects —
 * exceptional states (image fails to load, user closes browser tab,
 * etc.) all resolve to null so callers can treat cancel + error
 * the same way.
 */
export function openCropModal(opts: OpenCropModalOptions): Promise<CropResult | null> {
	if (typeof document === 'undefined') return Promise.resolve(null);

	return new Promise((resolve) => {
		const dialog = document.createElement('dialog');
		dialog.className = 'attachment-crop-modal';

		// Layout: header with aspect presets, image stage with crop
		// overlay, footer with Cancel / Apply buttons. All wrapped
		// in a single .attachment-crop-modal-shell container so the
		// dialog itself can stay minimal-styled.
		const shell = document.createElement('div');
		shell.className = 'attachment-crop-modal-shell';

		const header = document.createElement('div');
		header.className = 'attachment-crop-modal-header';
		const title = document.createElement('span');
		title.className = 'attachment-crop-modal-title';
		title.textContent = 'Crop image';
		header.appendChild(title);

		const aspectBar = document.createElement('div');
		aspectBar.className = 'attachment-crop-modal-aspects';
		header.appendChild(aspectBar);

		const stage = document.createElement('div');
		stage.className = 'attachment-crop-modal-stage';

		const img = document.createElement('img');
		img.className = 'attachment-crop-modal-img';
		img.alt = opts.alt ?? '';
		img.src = opts.imageUrl;
		// Prevent the contenteditable / drag interactions from racing
		// the crop rectangle's pointer events.
		img.draggable = false;

		const crop = document.createElement('div');
		crop.className = 'attachment-crop-modal-rect';
		// Build four corner handles (nw, ne, sw, se). Mid-edge handles
		// are deliberately omitted to keep the interaction surface
		// minimal — corners cover both width and height changes, and
		// the rectangle body itself is a "move" handle. Aspect locks
		// constrain corner drags so width-only / height-only resizes
		// fall out for free.
		const handles: Record<string, HTMLElement> = {};
		for (const corner of ['nw', 'ne', 'sw', 'se'] as const) {
			const h = document.createElement('div');
			h.className = `attachment-crop-modal-handle attachment-crop-modal-handle-${corner}`;
			h.dataset.corner = corner;
			crop.appendChild(h);
			handles[corner] = h;
		}

		stage.append(img, crop);

		const footer = document.createElement('div');
		footer.className = 'attachment-crop-modal-footer';
		const cancelBtn = button('Cancel', 'attachment-crop-modal-cancel');
		const applyBtn = button('Apply', 'attachment-crop-modal-apply attachment-crop-modal-primary');
		footer.append(cancelBtn, applyBtn);

		shell.append(header, stage, footer);
		dialog.appendChild(shell);
		document.body.appendChild(dialog);

		// State. `rect` is in preview-pixel coords relative to the
		// <img> element. We don't render the crop visual until the
		// image loads so dimensions are known.
		let rect: CropResult = { x: 0, y: 0, w: 0, h: 0 };
		let aspect: AspectChoice = null;

		// Build aspect-preset buttons. Active state mirrors `aspect`.
		const aspectButtons: HTMLButtonElement[] = [];
		for (const preset of ASPECT_PRESETS) {
			const btn = document.createElement('button');
			btn.type = 'button';
			btn.className = 'attachment-crop-modal-aspect-btn';
			btn.textContent = preset.label;
			btn.dataset.value = preset.value === null ? 'free' : String(preset.value);
			btn.addEventListener('click', (e) => {
				e.preventDefault();
				aspect = preset.value;
				rect = applyAspectToRect(rect, aspect, getNaturalBoundsPx());
				renderRect();
				updateAspectActive();
			});
			aspectBar.appendChild(btn);
			aspectButtons.push(btn);
		}
		const updateAspectActive = () => {
			for (const btn of aspectButtons) {
				const v = btn.dataset.value === 'free' ? null : Number(btn.dataset.value);
				btn.classList.toggle('attachment-crop-modal-aspect-active', v === aspect);
			}
		};

		// Initialize the rect once the image has loaded — we need
		// imgEl.offsetWidth / Height for sensible defaults.
		const onImageReady = () => {
			const bounds = getNaturalBoundsPx();
			// Default: 80% of the image, centered. Generous starting
			// point that the user can tighten without first having to
			// click into the image to scale up.
			const w = Math.round(bounds.w * 0.8);
			const h = Math.round(bounds.h * 0.8);
			rect = { x: Math.round((bounds.w - w) / 2), y: Math.round((bounds.h - h) / 2), w, h };
			renderRect();
			updateAspectActive();
		};
		if (img.complete && img.naturalWidth > 0) {
			// Cached load — onload won't fire; init synchronously.
			queueMicrotask(onImageReady);
		} else {
			img.addEventListener('load', onImageReady, { once: true });
			img.addEventListener(
				'error',
				() => {
					close(null);
				},
				{ once: true }
			);
		}

		// Re-clamp the crop rect on viewport resize so it doesn't slip
		// outside the (now smaller) preview bounds. Cheap to re-layout
		// since rect is preview-pixel-based.
		const onResize = () => {
			const bounds = getNaturalBoundsPx();
			rect = clampRect(rect, bounds);
			renderRect();
		};
		window.addEventListener('resize', onResize);

		// Drag interactions. We track three modes — move (rectangle
		// body), resize-corner, none — via `dragMode`. Pointer events
		// give us touch + mouse for free without wiring them
		// separately.
		type DragMode = { kind: 'none' } | { kind: 'move'; sx: number; sy: number; rx: number; ry: number } | { kind: 'resize'; corner: 'nw' | 'ne' | 'sw' | 'se'; sx: number; sy: number; sr: CropResult };
		let drag: DragMode = { kind: 'none' };

		const onPointerDown = (event: PointerEvent) => {
			const target = event.target as HTMLElement;
			if (handles[target.dataset.corner ?? '']) {
				drag = {
					kind: 'resize',
					corner: target.dataset.corner as 'nw' | 'ne' | 'sw' | 'se',
					sx: event.clientX,
					sy: event.clientY,
					sr: { ...rect },
				};
				event.preventDefault();
				event.stopPropagation();
				target.setPointerCapture?.(event.pointerId);
				return;
			}
			if (target === crop || crop.contains(target)) {
				drag = {
					kind: 'move',
					sx: event.clientX,
					sy: event.clientY,
					rx: rect.x,
					ry: rect.y,
				};
				event.preventDefault();
				event.stopPropagation();
				crop.setPointerCapture?.(event.pointerId);
			}
		};
		const onPointerMove = (event: PointerEvent) => {
			if (drag.kind === 'none') return;
			const bounds = getNaturalBoundsPx();
			if (drag.kind === 'move') {
				const dx = event.clientX - drag.sx;
				const dy = event.clientY - drag.sy;
				rect.x = drag.rx + dx;
				rect.y = drag.ry + dy;
				rect = clampRect(rect, bounds);
			} else {
				const dx = event.clientX - drag.sx;
				const dy = event.clientY - drag.sy;
				const c = drag.corner;
				let nx = drag.sr.x;
				let ny = drag.sr.y;
				let nw = drag.sr.w;
				let nh = drag.sr.h;
				// Per-corner direction map: which edges move with
				// the cursor. NW: left + top, NE: right + top,
				// SW: left + bottom, SE: right + bottom.
				if (c === 'nw' || c === 'sw') {
					nx = drag.sr.x + dx;
					nw = drag.sr.w - dx;
				} else {
					nw = drag.sr.w + dx;
				}
				if (c === 'nw' || c === 'ne') {
					ny = drag.sr.y + dy;
					nh = drag.sr.h - dy;
				} else {
					nh = drag.sr.h + dy;
				}
				// Aspect lock: clamp the secondary axis to match.
				// Drive from whichever delta is bigger so cursor
				// movement always wins on the dominant axis.
				if (aspect != null && aspect > 0) {
					const wDriven = Math.abs(dx) >= Math.abs(dy);
					if (wDriven) {
						const targetH = Math.round(nw / aspect);
						if (c === 'nw' || c === 'ne') ny = drag.sr.y + (drag.sr.h - targetH);
						nh = targetH;
					} else {
						const targetW = Math.round(nh * aspect);
						if (c === 'nw' || c === 'sw') nx = drag.sr.x + (drag.sr.w - targetW);
						nw = targetW;
					}
				}
				rect = clampRect({ x: Math.round(nx), y: Math.round(ny), w: Math.round(nw), h: Math.round(nh) }, bounds);
			}
			renderRect();
		};
		const onPointerUp = (event: PointerEvent) => {
			if (drag.kind !== 'none') {
				event.preventDefault();
				event.stopPropagation();
			}
			drag = { kind: 'none' };
		};

		stage.addEventListener('pointerdown', onPointerDown);
		document.addEventListener('pointermove', onPointerMove);
		document.addEventListener('pointerup', onPointerUp);

		// Pixel coordinates expressed against the displayed <img>
		// bounds. We use offset{Width,Height} (rounded ints) rather
		// than getBoundingClientRect (subpixel) so the rect stays in
		// integer space — simplifies clamping and avoids cumulative
		// drift across many drags.
		function getNaturalBoundsPx(): { w: number; h: number } {
			return { w: img.offsetWidth, h: img.offsetHeight };
		}

		function renderRect(): void {
			const offsetX = img.offsetLeft;
			const offsetY = img.offsetTop;
			crop.style.left = `${offsetX + rect.x}px`;
			crop.style.top = `${offsetY + rect.y}px`;
			crop.style.width = `${rect.w}px`;
			crop.style.height = `${rect.h}px`;
		}

		const close = (result: CropResult | null) => {
			window.removeEventListener('resize', onResize);
			document.removeEventListener('pointermove', onPointerMove);
			document.removeEventListener('pointerup', onPointerUp);
			if (dialog.open) dialog.close();
			dialog.remove();
			resolve(result);
		};

		cancelBtn.addEventListener('click', (e) => {
			e.preventDefault();
			close(null);
		});
		applyBtn.addEventListener('click', (e) => {
			e.preventDefault();
			if (img.naturalWidth <= 0 || img.naturalHeight <= 0) {
				close(null);
				return;
			}
			// Translate preview-pixel rect → natural-pixel rect.
			const sx = img.naturalWidth / img.offsetWidth;
			const sy = img.naturalHeight / img.offsetHeight;
			const result: CropResult = {
				x: Math.max(0, Math.round(rect.x * sx)),
				y: Math.max(0, Math.round(rect.y * sy)),
				w: Math.max(1, Math.round(rect.w * sx)),
				h: Math.max(1, Math.round(rect.h * sy)),
			};
			// Clamp to natural bounds so a fractional-rounding
			// overrun doesn't push the rect off-image.
			if (result.x + result.w > img.naturalWidth) result.w = img.naturalWidth - result.x;
			if (result.y + result.h > img.naturalHeight) result.h = img.naturalHeight - result.y;
			close(result);
		});

		// Esc / backdrop click → cancel.
		dialog.addEventListener('cancel', (e) => {
			e.preventDefault();
			close(null);
		});
		dialog.addEventListener('click', (event) => {
			if (event.target === dialog) close(null);
		});

		dialog.showModal();
	});
}

/** Build a button with consistent typing on the modal. */
function button(text: string, className: string): HTMLButtonElement {
	const btn = document.createElement('button');
	btn.type = 'button';
	btn.className = className;
	btn.textContent = text;
	return btn;
}

/**
 * Snap a rectangle to a target aspect ratio while preserving its
 * center. `aspect == null` is a no-op (free crop). The result is
 * clamped to `bounds` so the snapped rect never extends past the
 * image's preview-pixel bounds. Integer pixel coords throughout.
 */
function applyAspectToRect(
	r: CropResult,
	aspect: AspectChoice,
	bounds: { w: number; h: number }
): CropResult {
	if (aspect == null || aspect <= 0) return clampRect(r, bounds);
	const cx = r.x + r.w / 2;
	const cy = r.y + r.h / 2;
	// Pick the dimension that fits the existing rect best so
	// snapping doesn't grow past the image bounds.
	let w = r.w;
	let h = Math.round(w / aspect);
	if (h > r.h) {
		h = r.h;
		w = Math.round(h * aspect);
	}
	const nx = Math.round(cx - w / 2);
	const ny = Math.round(cy - h / 2);
	return clampRect({ x: nx, y: ny, w, h }, bounds);
}

/** Clamp a rectangle to fit within `bounds`, integer coords throughout. */
function clampRect(r: CropResult, bounds: { w: number; h: number }): CropResult {
	const w = Math.max(1, Math.min(r.w, bounds.w));
	const h = Math.max(1, Math.min(r.h, bounds.h));
	const x = Math.max(0, Math.min(r.x, bounds.w - w));
	const y = Math.max(0, Math.min(r.y, bounds.h - h));
	return { x, y, w, h };
}
