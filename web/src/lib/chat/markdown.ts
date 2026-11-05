// Chat message markdown (TASK-4315 PR 2a): an isolated Marked instance, then ONE
// pass through the Chat profile of sanitizeMarkdownHtml. Agent and human bodies
// take the same path and are equally untrusted; ref titles are untrusted too.
//
// The instance is `new Marked()`, so nothing here touches the global `marked`
// the item renderer configures, and the item renderer's hooks and state are not
// consulted. Raw HTML tokens are escaped (shown literally), links keep to http(s)
// without credentials, images keep to Pad's exact attachment route, and
// everything else an image or pad-attachment: reference could do is a
// noninteractive placeholder. The sanitiser re-enforces all of it on the output.

import { Marked, type Tokens } from 'marked';
import type { ChatRef } from '$lib/types';
import { sanitizeMarkdownHtml } from '$lib/utils/markdown';
import { attachmentImageSrc, safeLinkHref } from './urlPolicy';
import { replaceRefs } from './refs';

const escapeHtml = (s: string): string =>
	s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');

const PAD_ATTACHMENT = /^pad-attachment:/i;

const origin = (): string => window.location.origin;

const chatMarked = new Marked({
	gfm: true,
	// A chat line break is a line break (bodies were pre-wrap plain text before).
	breaks: true,
	renderer: {
		// Raw HTML, block or inline, is text to display, never markup to run.
		html({ text }: Tokens.HTML | Tokens.Tag): string {
			return escapeHtml(text);
		},
		link(this: { parser: { parseInline(t: Tokens.Generic[]): string } }, { href, title, tokens }: Tokens.Link): string {
			const text = this.parser.parseInline(tokens);
			if (PAD_ATTACHMENT.test(href.trim())) return `<span class="md-note">${text}</span>`;
			const safe = safeLinkHref(href, origin());
			if (safe === null) return text;
			const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
			return `<a href="${escapeHtml(safe)}"${titleAttr} target="_blank" rel="noopener noreferrer">${text}</a>`;
		},
		image({ href, title, text }: Tokens.Image): string {
			const alt = escapeHtml(text);
			if (PAD_ATTACHMENT.test(href.trim())) return `<span class="md-note">Attachment: ${alt}</span>`;
			const src = attachmentImageSrc(href, origin());
			if (src === null) return `${alt} <span class="md-note">External image omitted</span>`;
			const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
			return `<img src="${escapeHtml(src)}" alt="${alt}"${titleAttr} loading="lazy" decoding="async">`;
		},
		// Task-list boxes are never interactive in Chat: show the state as text.
		checkbox({ checked }: Tokens.Checkbox): string {
			return checked ? '[x] ' : '[ ] ';
		}
	}
});

/** Swap tokens for their HTML in text positions; inside a tag they revert to the literal ref. */
function swapTokens(html: string, swaps: ReturnType<typeof replaceRefs>['swaps']): string {
	if (swaps.length === 0) return html;
	const byToken = new Map(swaps.map((s) => [s.token, s]));
	const re = new RegExp(swaps.map((s) => s.token).join('|'), 'g');
	// marked escapes every `<` in text, so each `<...>` run is a tag.
	// Inside a <pre> (fenced or indented code, which marked and extractRefs may
	// disagree on) or an <a> (a link's own text or an autolinked URL), a ref stays
	// its literal text: code is inert, and an anchor cannot nest in an anchor.
	let literalDepth = 0;
	return html
		.split(/(<[^>]*>)/)
		.map((part) => {
			if (part.startsWith('<') && part.endsWith('>')) {
				const m = /^<(\/?)(pre|a)[\s>/]/i.exec(part);
				if (m) literalDepth = Math.max(0, literalDepth + (m[1] ? -1 : 1));
				return part.replace(re, (t) => escapeHtml(byToken.get(t)!.literal));
			}
			return part.replace(re, (t) =>
				literalDepth > 0 ? escapeHtml(byToken.get(t)!.literal) : byToken.get(t)!.html
			);
		})
		.join('');
}

/**
 * Render a Chat message body to sanitised HTML safe for {@html}. `refs` are the
 * thread's server-authorised verdicts; `ctxWorkspace` the message's context
 * workspace slug (a bare ref with none stays literal). With no DOM the result
 * is the body as escaped text, never markup.
 */
export function renderChatMarkdown(
	body: string,
	refs: readonly ChatRef[],
	ctxWorkspace: string | undefined
): string {
	if (!body) return '';
	if (typeof window === 'undefined') return escapeHtml(body);
	const { text, swaps } = replaceRefs(body, refs, ctxWorkspace);
	const html = swapTokens((chatMarked.parse(text, { async: false }) as string).trim(), swaps);
	return sanitizeMarkdownHtml(html, 'chat');
}
