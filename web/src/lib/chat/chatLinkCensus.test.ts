// TASK-4315 PR 3b: every in-app link to /messages goes through the chat surface,
// so a plain click opens Chat inside the shell and only a modified click is a
// direct entry. A new `href="/messages..."` in a component either routes its
// click through chatSurface.handleLinkClick or is listed here with a reason.
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const SRC = fileURLToPath(new URL('../..', import.meta.url));

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) walk(p, out);
		else if (/\.(svelte|ts)$/.test(name) && !/\.test\.ts$/.test(name)) out.push(p);
	}
	return out;
}

// file (relative to src/) -> why a /messages href there needs no interception
const ALLOWED: Record<string, string> = {};

const HREF = /href\s*=\s*(?:"|'|\{`?["'`])\/messages/;

describe('chat link census', () => {
	const hits = walk(SRC)
		.map((f) => ({ file: relative(SRC, f), text: readFileSync(f, 'utf8') }))
		.filter((f) => HREF.test(f.text));

	it('finds the known entry points', () => {
		const files = hits.map((h) => h.file);
		for (const want of [
			'lib/components/chat/ChatLink.svelte',
			'lib/components/chat/ChatButton.svelte',
			'lib/components/chat/ConversationList.svelte',
			'lib/components/chat/RosterRow.svelte',
			'lib/components/layout/Sidebar.svelte'
		]) {
			expect(files, want).toContain(want);
		}
	});

	it.each(hits.map((h) => [h.file, h.text] as const))('%s routes its /messages links through chatSurface', (file, text) => {
		if (ALLOWED[file]) return;
		expect(text, `${file} links to /messages without chatSurface.handleLinkClick`).toMatch(
			/chatSurface\.handleLinkClick\(/
		);
	});

	it('every allow-listed file still exists and still has a reason', () => {
		for (const [file, why] of Object.entries(ALLOWED)) {
			expect(why.length).toBeGreaterThan(10);
			expect(() => statSync(join(SRC, file)), file).not.toThrow();
		}
	});
});
