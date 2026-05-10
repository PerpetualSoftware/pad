/**
 * Hidden-content detector for HTML blocks (TASK-1327 / PLAN-1322).
 *
 * Walks an HTML string looking for elements + comments that are visible
 * to LLMs / agents reading the document but invisible (or near-invisible)
 * to a human author reviewing the rendered preview. The detector is an
 * **authoring-honesty** feature — NOT a security control. Render-time
 * sanitization (sanitizeHtmlBlock) protects browsers; this protects
 * authors from accidentally pasting content that looks one way and
 * reads another.
 *
 * Heuristics flag content that is one of:
 *   - CSS-hidden via inline style (display:none, visibility:hidden,
 *     opacity:0, font-size:0 or very small, color === background-color)
 *   - Off-screen positioned (position:absolute with very negative
 *     left/right/top/bottom, transform translate to off-screen)
 *   - Zero-dimension (width:0 AND height:0, clip:rect(0,0,0,0))
 *   - HTML comments
 *   - Suspiciously long aria-label / alt / title (>200 chars or
 *     containing newlines — heuristic for hidden text in attribute
 *     values)
 *
 * Class-based hiding (e.g. `.sr-only`) is NOT flagged: resolving it
 * requires the page's stylesheet context, which we don't have here,
 * and the false-positive rate would be too high. The detector stays
 * conservative.
 */

/**
 * The Pad-internal sentinel used by `setHiddenContentAcknowledged` to
 * mark that a human has reviewed and accepted the hidden content.
 * `detectHiddenContent` skips this exact comment so dismissal sticks
 * after a doc reload.
 */
export const PAD_ACK_HIDDEN_MARKER = '<!-- pad:ack-hidden -->';

const PAD_ACK_HIDDEN_TEXT = ' pad:ack-hidden '; // textContent of the marker (with surrounding spaces)
const PAD_ACK_HIDDEN_TEXT_TRIMMED = 'pad:ack-hidden';

export interface HiddenSegment {
	/** Lowercase tag name of the offending element, or `#comment`. */
	tag: string;
	/** Short human-readable rule label, e.g. `display:none` or `font-size:2px (too small)`. */
	rule: string;
	/** Up to 80 chars of trimmed text content, for context in the inspector. */
	snippet: string;
}

/**
 * Returns true when the user has previously dismissed the hidden-content
 * warning for this block. The marker lives at the top of the html string
 * so it's the first thing read on parse and survives markdown round-trips
 * (it's just an HTML comment, valid in any context where raw HTML is
 * permitted).
 */
export function isHiddenContentAcknowledged(html: string): boolean {
	return html.trimStart().startsWith(PAD_ACK_HIDDEN_MARKER);
}

/**
 * Prepend the ack marker if not already present. Idempotent. Used by the
 * NodeView's "Dismiss for this block" affordance.
 */
export function setHiddenContentAcknowledged(html: string): string {
	if (isHiddenContentAcknowledged(html)) return html;
	const sep = html.startsWith('\n') || html === '' ? '' : '\n';
	return `${PAD_ACK_HIDDEN_MARKER}${sep}${html}`;
}

/**
 * Detect hidden content in an HTML block's raw source. Returns one
 * `HiddenSegment` per offending element / attribute / comment. Returns
 * an empty array when running outside a browser (no DOMParser) — the
 * detector is purely client-side.
 */
export function detectHiddenContent(html: string): HiddenSegment[] {
	if (typeof window === 'undefined' || typeof DOMParser === 'undefined') return [];
	if (!html) return [];

	const segments: HiddenSegment[] = [];
	const doc = new DOMParser().parseFromString(html, 'text/html');

	// Walk all elements regardless of whether the parser placed them
	// inside body (the typical case) or moved them to head (e.g. <style>
	// / <link> at the top of the snippet). We only check elements that
	// rendered content can hide behind, but element-level checks are
	// cheap so just walk the whole document.
	doc.querySelectorAll('*').forEach((el) => {
		// Skip <html>, <head>, <body> wrappers DOMParser inserts; they
		// never carry the inline-style hide patterns we look for.
		const tag = el.tagName.toLowerCase();
		if (tag === 'html' || tag === 'head' || tag === 'body') return;
		segments.push(...checkElement(el as HTMLElement));
	});

	// Walk comments anywhere in the document. DOMParser can place
	// comments at document level (siblings of <html>) when the input
	// has a leading `<!-- ... -->`; walking only doc.body would miss
	// those. createTreeWalker on `doc` is the inclusive root.
	const walker = doc.createTreeWalker(doc, NodeFilter.SHOW_COMMENT);
	let node = walker.nextNode();
	while (node) {
		const text = node.textContent ?? '';
		const trimmed = text.trim();
		if (text !== PAD_ACK_HIDDEN_TEXT && trimmed !== PAD_ACK_HIDDEN_TEXT_TRIMMED) {
			segments.push({
				tag: '#comment',
				rule: 'HTML comment',
				snippet: snippetFor(text),
			});
		}
		node = walker.nextNode();
	}

	return segments;
}

function checkElement(el: HTMLElement): HiddenSegment[] {
	const segments: HiddenSegment[] = [];
	const tag = el.tagName.toLowerCase();
	// `el.style` is the browser-parsed CSSStyleDeclaration for the
	// inline `style` attribute. The browser handles every CSS edge
	// case for us:
	//
	//   - `!important` priority (display:none!important;display:block
	//     leaves `el.style.display === 'none'`)
	//   - CSS comments (display:/**/none parses correctly)
	//   - case-insensitive keywords (DISPLAY: NONE → 'none')
	//   - whitespace and unit normalisation
	//
	// A hand-rolled `style` attribute parser misses these consistently;
	// using the CSSOM here is materially more robust.
	const style = el.style;
	const text = el.textContent ?? '';

	if (style.display === 'none') {
		segments.push({ tag, rule: 'display:none', snippet: snippetFor(text) });
	}
	if (style.visibility === 'hidden') {
		segments.push({ tag, rule: 'visibility:hidden', snippet: snippetFor(text) });
	}
	if (isZeroOpacity(style.opacity)) {
		segments.push({ tag, rule: `opacity:${style.opacity}`, snippet: snippetFor(text) });
	}
	const fontSize = parseLength(style.fontSize);
	if (fontSize !== null && fontSize < 6) {
		segments.push({
			tag,
			rule: `font-size:${style.fontSize} (too small)`,
			snippet: snippetFor(text),
		});
	}
	if (
		style.color &&
		style.backgroundColor &&
		normalizeColor(style.color) === normalizeColor(style.backgroundColor)
	) {
		segments.push({
			tag,
			rule: 'color matches background-color',
			snippet: snippetFor(text),
		});
	}
	if (style.position === 'absolute' || style.position === 'fixed') {
		for (const prop of ['left', 'right', 'top', 'bottom'] as const) {
			const value = style.getPropertyValue(prop);
			const offset = parseLength(value);
			if (offset !== null && offset <= -9000) {
				segments.push({
					tag,
					rule: `${prop}:${value} (off-screen)`,
					snippet: snippetFor(text),
				});
				break;
			}
		}
	}
	if (style.transform && /translate[xy]?\s*\(\s*-?\d+/i.test(style.transform)) {
		const off = /-\s*9\d{3,}|-\s*\d{5,}/.test(style.transform);
		if (off) {
			segments.push({
				tag,
				rule: 'transform off-screen',
				snippet: snippetFor(text),
			});
		}
	}
	const w = parseLength(style.width);
	const h = parseLength(style.height);
	if (w === 0 && h === 0) {
		segments.push({ tag, rule: 'width:0;height:0', snippet: snippetFor(text) });
	}
	if (style.clip && /rect\(\s*0(?:px)?\s*,\s*0(?:px)?\s*,\s*0(?:px)?\s*,\s*0(?:px)?\s*\)/i.test(style.clip)) {
		segments.push({ tag, rule: 'clip:rect(0,0,0,0)', snippet: snippetFor(text) });
	}

	for (const attr of ['aria-label', 'alt', 'title'] as const) {
		const val = el.getAttribute(attr);
		if (!val) continue;
		const tooLong = val.length > 200;
		const hasNewline = /\n/.test(val);
		if (tooLong || hasNewline) {
			const reasons: string[] = [];
			if (tooLong) reasons.push(`${val.length} chars`);
			if (hasNewline) reasons.push('contains newlines');
			segments.push({
				tag,
				rule: `${attr} suspicious (${reasons.join(', ')})`,
				snippet: snippetFor(val),
			});
		}
	}

	return segments;
}

/**
 * Parse a CSS length declaration like `12px`, `0`, `-9999px`, `1.5em`
 * into a number when the unit is bare (`0`) or px. Returns `null` for
 * percentage / em / rem / unrecognised forms — those don't have a
 * single numeric "is this hidden" answer.
 */
function parseLength(value: string | undefined): number | null {
	if (!value) return null;
	const trimmed = value.trim();
	if (trimmed === '0') return 0;
	const match = trimmed.match(/^(-?[\d.]+)px$/i);
	if (!match) return null;
	const n = parseFloat(match[1]);
	return Number.isFinite(n) ? n : null;
}

function isZeroOpacity(value: string | undefined): boolean {
	if (!value) return false;
	const v = value.trim();
	return v === '0' || v === '0.0' || v === '0%' || v === '.0';
}

function normalizeColor(c: string): string {
	return c.toLowerCase().replace(/\s+/g, '');
}

function snippetFor(text: string): string {
	const collapsed = text.trim().replace(/\s+/g, ' ');
	if (collapsed.length <= 80) return collapsed;
	return `${collapsed.slice(0, 77)}…`;
}
