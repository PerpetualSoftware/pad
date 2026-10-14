/**
 * The public share page's post-pass over SANITIZED markdown HTML (TASK-2248,
 * audit clusters C83 / C84). A share viewer is anonymous, so nothing on the
 * page may send them into the authenticated app:
 *
 * - WIKI-LINKS (`[[Title]]`, `[[REF]]`, `[[ws::REF]]`) used to reach the
 *   viewer as literal brackets, which read as a rendering glitch. They become
 *   emphasized plain text: the display override, else the title the share's
 *   own payload holds for a ref, else the body. Never a link.
 * - ROOT-RELATIVE and same-origin links (`/owner/ws/tasks/TASK-5`) looked like
 *   working links and bounced the viewer to /login, while also exposing the
 *   owner's username / workspace / slug path. They become plain text with a
 *   "requires access" title. The two families that ARE for share viewers stay
 *   links: `/s/…` (share pages) and `/api/v1/s/…` (token-scoped attachment
 *   bytes, which this page's own renderer emits).
 * - EXTERNAL links open in a new tab, `rel="noopener noreferrer"`.
 *
 * Runs on a DOM, so it is a no-op without one (SSR): the page's sanitizer pass
 * is client-only for the same reason. Code spans and code blocks are left
 * alone, since `[[x]]` in code is code. A wiki-link whose brackets markdown
 * split across elements (`[[TA*SK*-5]]`) is matched per text node and stays
 * as written: it can only ever read as brackets, never become a link.
 */
import { WIKI_LINK_PATTERN_SOURCE, wikiDisplayText } from '$lib/utils/markdown';

const VIEWER_PATH_PREFIXES = ['/s/', '/api/v1/s/'];

/**
 * True when `href`, followed from the page at `pageUrl`, lands on this app's
 * origin somewhere a share viewer cannot follow.
 *
 * Decided on the URL the BROWSER would navigate to, never on the href's
 * spelling: `new URL` applies the same WHATWG parsing a click does, so dot
 * segments (`/s/../dave/ws`, `%2e%2e`), letter case and backslashes are
 * normalised before the path is compared. A prefix test on the raw string let
 * `/s/../dave/ws` through (codex round 1 on TASK-2248 U1). A leading
 * backslash (`\/x`) is `//x` to a browser: another HOST, so not internal.
 * Page-local hrefs (`#x`, `?q`) resolve to the share page itself, under `/s/`.
 */
export function isInternalHref(href: string, pageUrl: string): boolean {
	let url: URL;
	let page: URL;
	try {
		page = new URL(pageUrl);
		url = new URL(href, page);
	} catch {
		return false;
	}
	if (url.origin !== page.origin) return false;
	return !VIEWER_PATH_PREFIXES.some((p) => url.pathname.startsWith(p));
}

function inCode(node: Node): boolean {
	for (let p = node.parentElement; p; p = p.parentElement) {
		if (p.tagName === 'CODE' || p.tagName === 'PRE') return true;
	}
	return false;
}

/**
 * Rewrite wiki-links and internal links in sanitized `html` for an anonymous
 * viewer. `titleByRef` maps an UPPER-CASE ref (`TASK-5`) to the title the
 * share payload carries for it.
 */
/**
 * The classes the markdown renderer itself emits, by tag (BUG-3799), from a
 * census of renderMarkedWithAttachments + sanitizeMarkdownHtml on the share
 * path: external links, attachment file chips and their unavailable / missing
 * placeholders, and marked's `language-*` on fenced code (which the mermaid
 * pass reads). The in-app sanitizer keeps any class, so a share body could
 * reuse the app's CSS (a fixed or full-screen utility) to lay a fake login or
 * banner over the page. Classes the share page's own passes add
 * (share-wiki-ref, share-internal-ref, share-mermaid) are added AFTER this
 * runs, so they are not listed.
 */
const SHARE_CLASSES_BY_TAG: Readonly<Record<string, ReadonlySet<string>>> = {
	a: new Set(['external-link', 'file-chip']),
	span: new Set([
		'external-icon',
		'file-chip-icon',
		'file-chip-name',
		'file-chip-size',
		'attachment-missing',
		'attachment-unavailable',
		'attachment-unavailable-note',
	]),
};
const CODE_LANGUAGE_CLASS = /^language-[a-z0-9_+#.-]+$/i;

/** The allowlist as `tag.class` pairs, for the census test that pins it. */
export function shareClassAllowlist(): string[] {
	return Object.entries(SHARE_CLASSES_BY_TAG).flatMap(([tag, set]) => [...set].map((c) => `${tag}.${c}`)).sort();
}

function shareClassAllowed(tag: string, cls: string): boolean {
	if (tag === 'code') return CODE_LANGUAGE_CLASS.test(cls);
	return SHARE_CLASSES_BY_TAG[tag]?.has(cls) ?? false;
}

/**
 * Restrict sanitized share HTML to what the markdown renderer itself emits
 * (BUG-3799), so a body cannot select the app's global CSS:
 * - every class the renderer would not have emitted, on that tag, is stripped;
 * - an <input> survives only as the renderer's task-list checkbox, and is
 *   forced disabled. Any other input (a text field) matched the global
 *   `input` rule, which is a styled fake sign-in field without any class
 *   (Codex r1 on this unit).
 * Runs on the sanitizer's output, before the share page's own passes. Fails
 * CLOSED without a DOM: it returns nothing rather than HTML it could not check.
 */
export function restrictShareMarkup(html: string): string {
	if (!html) return html;
	if (typeof document === 'undefined') return '';
	const tpl = document.createElement('template');
	tpl.innerHTML = html;
	for (const input of tpl.content.querySelectorAll('input')) {
		if ((input.getAttribute('type') ?? '').toLowerCase() === 'checkbox') input.setAttribute('disabled', '');
		else input.remove();
	}
	for (const el of tpl.content.querySelectorAll('[class]')) {
		const tag = el.tagName.toLowerCase();
		const kept = [...el.classList].filter((c) => shareClassAllowed(tag, c));
		if (kept.length) el.setAttribute('class', kept.join(' '));
		else el.removeAttribute('class');
	}
	return tpl.innerHTML;
}

export function inertInternalReferences(
	html: string,
	titleByRef: ReadonlyMap<string, string>,
	pageUrl: string = typeof location !== 'undefined' ? location.href : 'http://localhost/s/'
): string {
	if (typeof document === 'undefined' || !html) return html;
	const tpl = document.createElement('template');
	tpl.innerHTML = html;
	const root = tpl.content;

	// An image-map <area> is a link too, and has no text to keep: it loses its href.
	for (const area of Array.from(root.querySelectorAll('area[href]'))) {
		if (isInternalHref(area.getAttribute('href') ?? '', pageUrl)) area.removeAttribute('href');
	}
	for (const a of Array.from(root.querySelectorAll('a[href]'))) {
		const href = a.getAttribute('href') ?? '';
		if (isInternalHref(href, pageUrl)) {
			const span = document.createElement('span');
			span.className = 'share-internal-ref';
			span.title = 'Requires access to this workspace';
			while (a.firstChild) span.appendChild(a.firstChild);
			a.replaceWith(span);
		} else if (/^https?:\/\//i.test(href.trim())) {
			a.setAttribute('target', '_blank');
			a.setAttribute('rel', 'noopener noreferrer');
		}
	}

	const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
	const texts: Text[] = [];
	for (let n = walker.nextNode(); n; n = walker.nextNode()) {
		const t = n as Text;
		if (t.data.includes('[[') && !inCode(t)) texts.push(t);
	}
	for (const t of texts) {
		const pattern = new RegExp(WIKI_LINK_PATTERN_SOURCE, 'g');
		const frag = document.createDocumentFragment();
		let last = 0;
		for (let m = pattern.exec(t.data); m; m = pattern.exec(t.data)) {
			if (m.index > last) frag.appendChild(document.createTextNode(t.data.slice(last, m.index)));
			const em = document.createElement('em');
			em.className = 'share-wiki-ref';
			em.textContent = wikiDisplayText(m[1], titleByRef);
			frag.appendChild(em);
			last = m.index + m[0].length;
		}
		if (last === 0) continue;
		if (last < t.data.length) frag.appendChild(document.createTextNode(t.data.slice(last)));
		t.replaceWith(frag);
	}
	return tpl.innerHTML;
}
