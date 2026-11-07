// Test support for lib/chat/chatLinkCensus.test.ts: finds anchors and imperative
// navigations that point at the Chat routes, per site rather than per file.

/** Pull each `<a ...>` opening tag out of a Svelte source, honouring `{...}` and quotes. */
export function anchorTags(text: string): string[] {
	const tags: string[] = [];
	const open = /<a(?=[\s>])/g;
	let m: RegExpExecArray | null;
	while ((m = open.exec(text))) {
		let i = m.index + 2;
		let depth = 0;
		let quote: string | null = null;
		let inJsString: string | null = null;
		for (; i < text.length; i++) {
			const c = text[i];
			if (inJsString) {
				if (c === '\\') i++;
				else if (c === inJsString) inJsString = null;
			} else if (depth > 0) {
				if (c === '"' || c === "'" || c === '`') inJsString = c;
				else if (c === '{') depth++;
				else if (c === '}') depth--;
			} else if (quote) {
				if (c === quote) quote = null;
			} else if (c === '"' || c === "'") quote = c;
			else if (c === '{') depth++;
			else if (c === '>') break;
		}
		tags.push(text.slice(m.index, i + 1));
		open.lastIndex = i;
	}
	return tags;
}

const TO_CHAT = '/mess' + 'ages';
const HREF = new RegExp(`\\bhref\\s*=\\s*(?:["']|\\{\\s*[\`"'])${TO_CHAT}`);
const GOTO = new RegExp(`\\bgoto\\(\\s*[\`'"]${TO_CHAT}`, 'g');

/** Anchors whose href is a Chat route. */
export function chatAnchors(text: string): string[] {
	return anchorTags(text).filter((t) => HREF.test(t));
}

/** How many imperative navigations name a Chat route literally. */
export function chatGotoCount(text: string): number {
	return (text.match(GOTO) ?? []).length;
}

/** The Chat anchors in `text` that do not route their click through the surface. */
export function unwiredChatAnchors(text: string): string[] {
	return chatAnchors(text).filter((t) => !t.includes('chatSurface.handleLinkClick('));
}
