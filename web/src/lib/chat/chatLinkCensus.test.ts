// TASK-4315 PR 3b: every in-app link to /messages goes through the chat surface,
// so a plain click opens Chat inside the shell and only a modified click is a
// direct entry. Checked per link, not per file: each anchor to /messages carries
// its own chatSurface.handleLinkClick, and an imperative goto to /messages is
// allow-listed with a reason.
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import { anchorTags, chatAnchors, chatGotoCount, unwiredChatAnchors } from '../../test/linkCensusScan';

const SRC = fileURLToPath(new URL('../..', import.meta.url));

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) walk(p, out);
		else if (/\.(svelte|ts)$/.test(name) && !/\.test\.ts$/.test(name)) out.push(p);
	}
	return out;
}

// file (relative to src/) -> why a goto to a Chat route there is not a link to intercept
const ALLOWED_GOTO: Record<string, string> = {
	'lib/stores/chatSurface.svelte.ts': 'it is the surface: its shallow goto IS the interception'
};

const files = walk(SRC).map((f) => ({ file: relative(SRC, f), text: readFileSync(f, 'utf8') }));

describe('the scanner', () => {
	const wired = `<a href="/m" onclick={(e) => chatSurface.handleLinkClick(e, '/m')}>x</a>`.replaceAll('/m', '/mess' + 'ages');
	const bare = `<a class="two" href="/m">y</a>`.replaceAll('/m', '/mess' + 'ages');

	it('accepts a wired link and ignores arrows inside the handler', () => {
		expect(chatAnchors(wired)).toHaveLength(1);
		expect(unwiredChatAnchors(wired)).toEqual([]);
	});

	it('fails a deliberately unwired second link in a file whose first link is wired', () => {
		expect(unwiredChatAnchors(`${wired}\n${bare}`)).toHaveLength(1);
		expect(unwiredChatAnchors(`${wired}\n${bare}`)[0]).toContain('class="two"');
	});

	it('sees a templated href', () => {
		const t = '<a href="/mess' + 'ages/{c.number}" class="x">z</a>';
		expect(unwiredChatAnchors(t)).toHaveLength(1);
	});

	it('does not confuse other anchors, and reads past > inside braces', () => {
		expect(anchorTags(`<a href="/x" onclick={() => a > b}>q</a><a href="/y">r</a>`)).toHaveLength(2);
		expect(chatAnchors(`<a href="/x">q</a>`)).toEqual([]);
	});

	it('counts imperative navigations to a Chat route', () => {
		expect(chatGotoCount("goto('/mess" + "ages/4')")).toBe(1);
		expect(chatGotoCount('goto(`/mess' + 'ages`)')).toBe(1);
		expect(chatGotoCount('goto(href)')).toBe(0);
	});
});

describe('chat link census', () => {
	const sites = files.flatMap((f) => chatAnchors(f.text).map((tag) => ({ file: f.file, tag })));

	it('finds the known entry points', () => {
		const have = new Set(sites.map((s) => s.file));
		for (const want of [
			'lib/components/chat/ChatLink.svelte',
			'lib/components/chat/ChatButton.svelte',
			'lib/components/chat/ConversationList.svelte',
			'lib/components/chat/RosterRow.svelte',
			'lib/components/chat/ChatThreadView.svelte',
			'lib/components/layout/Sidebar.svelte'
		]) {
			expect(have.has(want), want).toBe(true);
		}
	});

	it.each(sites.map((s, i) => [`${s.file} #${i}`, s.file, s.tag] as const))(
		'%s intercepts its own click',
		(_name, file, tag) => {
			expect(tag, `${file}: this link to a Chat route has no chatSurface.handleLinkClick`).toContain(
				'chatSurface.handleLinkClick('
			);
		}
	);

	it('every goto to a Chat route is allow-listed with a reason', () => {
		const loose = files
			.filter((f) => chatGotoCount(f.text) > 0 && !ALLOWED_GOTO[f.file])
			.map((f) => f.file);
		expect(loose, 'goto to a Chat route outside the surface').toEqual([]);
	});

	it('every allow-listed file still exists, still navigates there, and has a reason', () => {
		for (const [file, why] of Object.entries(ALLOWED_GOTO)) {
			expect(why.length).toBeGreaterThan(10);
			const f = files.find((x) => x.file === file);
			expect(f, file).toBeDefined();
			expect(chatGotoCount(f!.text), file).toBeGreaterThan(0);
		}
	});
});
