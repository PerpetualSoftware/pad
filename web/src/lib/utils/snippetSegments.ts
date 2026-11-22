// A search snippet as text segments, so the palette can show which words
// matched (TASK-4520).
//
// The server sends the snippet as RAW stored text (not HTML-escaped) with
// `<mark>` / `</mark>` around matched terms (internal/store/search.go). Only
// those two markers mean anything here; every other tag-looking run is
// removed exactly as the palette's old stripHtml did, and the result is only
// ever rendered as text, never as HTML.

export interface SnippetSegment {
	text: string;
	hit: boolean;
}

const MARKER = /<\/?mark>/g;
const TAG = /<[^>]*>/g;

export function snippetSegments(snippet: string): SnippetSegment[] {
	const out: SnippetSegment[] = [];
	let inHit = false;
	let last = 0;
	const push = (raw: string) => {
		const text = raw.replace(TAG, '');
		if (!text) return;
		const prev = out[out.length - 1];
		if (prev && prev.hit === inHit) prev.text += text;
		else out.push({ text, hit: inHit });
	};
	for (const m of snippet.matchAll(MARKER)) {
		push(snippet.slice(last, m.index));
		last = m.index + m[0].length;
		// An opening marker inside a hit and a closing one outside it are
		// dropped; an unclosed hit runs to the end.
		inHit = m[0] === '<mark>';
	}
	push(snippet.slice(last));
	return out;
}

/**
 * Trim the text before the first hit to at most `maxLead` characters, so a
 * row that truncates at its end still shows the match. The cut moves forward
 * to the next word start when one exists inside the kept window, and a
 * leading "…" says text was dropped.
 */
export function windowSnippet(segments: SnippetSegment[], maxLead: number): SnippetSegment[] {
	const first = segments.findIndex((s) => s.hit);
	if (first <= 0) return segments;
	const lead = segments
		.slice(0, first)
		.map((s) => s.text)
		.join('');
	if (lead.length <= maxLead) return segments;
	const cut = lead.length - maxLead;
	let kept = lead.slice(cut);
	if (!/\s/.test(lead[cut - 1]) && !/^\s/.test(kept)) {
		const space = kept.search(/\s/);
		if (space >= 0) kept = kept.slice(space + 1);
	}
	kept = kept.trimStart();
	return [{ text: `…${kept}`, hit: false }, ...segments.slice(first)];
}
